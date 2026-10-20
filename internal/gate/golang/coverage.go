package golang

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	GobcoBinary        = "gobco"
	ValueLineCoverage  = "line_coverage"
	ValueStmtCoverage  = "statement_coverage"
	ValueBranchCover   = "branch_coverage"
	ValueLines         = "lines"
	ValueBranches      = "branches"
	percentScale       = 100
	modePrefix         = "mode:"
	profileFields      = 3
	reasonLineCoverage = "line coverage is below the policy threshold"
	reasonBranchCover  = "branch coverage is below the policy threshold"
	uncoveredFormat    = "%s: %d of %d lines uncovered"
	gobcoFailed        = "gobco %s: %v: %s"
)

var (
	ErrProfile    = errors.New("malformed coverage profile")
	profileBlock  = regexp.MustCompile(`^(.+):(\d+)\.\d+,(\d+)\.\d+$`)
	gobcoTotal    = regexp.MustCompile(`(?m)^Branch coverage: (\d+)/(\d+)$`)
	gobcoMissLine = regexp.MustCompile(`(?m)^.+:\d+:\d+: condition .+$`)
)

type block struct {
	file       string
	start, end int
	stmts      int
	count      int64
}

type blockKey struct {
	file, pos string
}

func parseProfile(text string) ([]block, error) {
	merged := map[blockKey]*block{}
	var order []blockKey
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, modePrefix) {
			continue
		}
		b, key, err := parseBlock(line)
		if err != nil {
			return nil, err
		}
		if prev, ok := merged[key]; ok {
			prev.count += b.count
			continue
		}
		merged[key] = &b
		order = append(order, key)
	}
	out := make([]block, 0, len(order))
	for _, k := range order {
		out = append(out, *merged[k])
	}
	return out, sc.Err()
}

func parseBlock(line string) (block, blockKey, error) {
	f := strings.Fields(line)
	if len(f) != profileFields {
		return block{}, blockKey{}, fmt.Errorf("%w: %q", ErrProfile, line)
	}
	m := profileBlock.FindStringSubmatch(f[0])
	stmts, serr := strconv.Atoi(f[1])
	count, cerr := strconv.ParseInt(f[2], 10, 64)
	if m == nil || serr != nil || cerr != nil {
		return block{}, blockKey{}, fmt.Errorf("%w: %q", ErrProfile, line)
	}
	start, _ := strconv.Atoi(m[2])
	end, _ := strconv.Atoi(m[3])
	return block{file: m[1], start: start, end: end, stmts: stmts, count: count}, blockKey{file: m[1], pos: f[0]}, nil
}

type lineCoverage struct {
	covered, total     int
	stmtCovered, stmts int
	uncoveredByFile    map[string]int
	linesByFile        map[string]int
}

func lineStates(blocks []block) map[string]map[int]bool {
	state := map[string]map[int]bool{}
	for _, b := range blocks {
		lines := state[b.file]
		if lines == nil {
			lines = map[int]bool{}
			state[b.file] = lines
		}
		for l := b.start; l <= b.end; l++ {
			lines[l] = lines[l] || b.count > 0
		}
	}
	return state
}

func measureLines(blocks []block) lineCoverage {
	lc := lineCoverage{uncoveredByFile: map[string]int{}, linesByFile: map[string]int{}}
	for _, b := range blocks {
		lc.stmts += b.stmts
		if b.count > 0 {
			lc.stmtCovered += b.stmts
		}
	}
	for file, lines := range lineStates(blocks) {
		for _, covered := range lines {
			lc.total++
			lc.linesByFile[file]++
			if covered {
				lc.covered++
				continue
			}
			lc.uncoveredByFile[file]++
		}
	}
	return lc
}

func ratio(part, whole int) float64 {
	if whole == 0 {
		return percentScale
	}
	return float64(part) * percentScale / float64(whole)
}

func Coverage(_ context.Context, ws gate.Workspace) gate.Outcome {
	data, err := os.ReadFile(filepath.Clean(ws.Report(CoverReport)))
	if err != nil {
		return gate.Fail(fmt.Errorf(reportMissing, err).Error())
	}
	blocks, err := parseProfile(string(data))
	if err != nil {
		return gate.Fail(err.Error())
	}
	lc := measureLines(blocks)
	line := ratio(lc.covered, lc.total)
	values := map[string]float64{ValueLineCoverage: line, ValueStmtCoverage: ratio(lc.stmtCovered, lc.stmts), ValueLines: float64(lc.total)}
	return gate.Judge(lc.total > 0 && line >= ws.Policy.Thresholds.LineCoverage, reasonLineCoverage, values, lc.uncovered())
}

func (lc lineCoverage) uncovered() []string {
	files := make([]string, 0, len(lc.uncoveredByFile))
	for f := range lc.uncoveredByFile {
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b string) int {
		return cmp.Or(cmp.Compare(lc.uncoveredByFile[b], lc.uncoveredByFile[a]), strings.Compare(a, b))
	})
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, fmt.Sprintf(uncoveredFormat, f, lc.uncoveredByFile[f], lc.linesByFile[f]))
	}
	return out
}

func Branches(ctx context.Context, ws gate.Workspace) gate.Outcome {
	pkgs, err := List(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	covered, total := 0, 0
	var findings []string
	for _, p := range pkgs.Production() {
		rel, rerr := filepath.Rel(ws.Src, p.Dir)
		if rerr != nil {
			return gate.Fail(rerr.Error())
		}
		res, err := ws.Run(ctx, GobcoBinary, "-branch", "./"+filepath.ToSlash(rel))
		bc, perr := parseGobco(res.Stdout)
		if err != nil || perr != nil {
			return gate.Fail(fmt.Sprintf(gobcoFailed, rel, errors.Join(err, perr), gate.Tail(res.Combined())))
		}
		covered, total = covered+bc.covered, total+bc.total
		findings = append(findings, gobcoMissLine.FindAllString(res.Stdout, -1)...)
	}
	pct := ratio(covered, total)
	values := map[string]float64{ValueBranchCover: pct, ValueBranches: float64(total)}
	return gate.Judge(pct >= ws.Policy.Thresholds.BranchCoverage, reasonBranchCover, values, findings)
}

type branchCount struct {
	covered, total int
}

func parseGobco(out string) (branchCount, error) {
	m := gobcoTotal.FindStringSubmatch(out)
	if m == nil {
		return branchCount{}, fmt.Errorf("%w: no branch coverage summary", ErrProfile)
	}
	covered, _ := strconv.Atoi(m[1])
	total, _ := strconv.Atoi(m[2])
	return branchCount{covered: covered, total: total}, nil
}

func ProfileLines(text string) (map[string]map[int]bool, error) {
	blocks, err := parseProfile(text)
	if err != nil {
		return nil, err
	}
	return lineStates(blocks), nil
}
