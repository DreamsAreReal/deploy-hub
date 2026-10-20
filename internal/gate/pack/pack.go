package pack

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/lexis"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
)

const (
	CheckModules      = "modules"
	CheckDependencies = "dependencies"
	CheckLint         = "lint"
	CheckComplexity   = "cognitive-complexity"
	CheckDeadCode     = "dead-code"
	CheckTests        = "tests"
	CheckLineCoverage = "line-coverage"
	CheckBranches     = "branch-coverage"
	CheckFuzz         = "fuzz"
	CheckMutation     = "mutation"
	CheckVulns        = "vulnerabilities"
)

var Checks = []string{CheckModules, CheckDependencies, CheckLint, CheckComplexity, CheckDeadCode, CheckTests, CheckLineCoverage, CheckBranches, CheckFuzz, CheckMutation, CheckVulns}

const (
	rootDir        = "."
	labelSeparator = ":"
	valueSeparator = "."
	outDirPerm     = 0o700
	ValueProjects  = "projects"
	reasonNoCode   = "no project of a supported stack in the repository"
	reasonOrphans  = "source files that no language pack can check"
	orphanNoPack   = "%s: no language pack for %s files"
	orphanNoRoot   = "%s: %s source outside a project (%s)"
)

type Run func(ctx context.Context, ws gate.Workspace) gate.Outcome

type Pack struct {
	Stack     string
	Manifests []string
	Sources   []string
	Loose     bool
	Checks    map[string]Run
	Passed    func(ctx context.Context, ws gate.Workspace) (map[string]bool, error)
}

type Project struct {
	Pack *Pack
	Root string
}

func (p Project) Label() string {
	if p.Root == rootDir {
		return p.Pack.Stack
	}
	return p.Pack.Stack + labelSeparator + p.Root
}

func (p Project) Workspace(ws gate.Workspace) (gate.Workspace, error) {
	ws.Src = filepath.Join(ws.Src, filepath.FromSlash(p.Root))
	ws.Out = filepath.Join(ws.Out, strings.NewReplacer("/", "-", labelSeparator, "-").Replace(p.Label()))
	return ws, os.MkdirAll(ws.Out, outDirPerm)
}

type Layout struct {
	Projects []Project
	Orphans  []string
}

func Discover(files []string, packs []Pack) Layout {
	var layout Layout
	for i := range packs {
		projects, orphans := discoverPack(&packs[i], files)
		layout.Projects = append(layout.Projects, projects...)
		layout.Orphans = append(layout.Orphans, orphans...)
	}
	layout.Orphans = append(layout.Orphans, unclaimed(files, packs)...)
	slices.Sort(layout.Orphans)
	return layout
}

func discoverPack(p *Pack, files []string) ([]Project, []string) {
	roots := manifestRoots(files, p.Manifests)
	homeless := homelessSources(p, files, roots)
	var orphans []string
	switch {
	case !p.Loose:
		for _, f := range homeless {
			orphans = append(orphans, fmt.Sprintf(orphanNoRoot, f, p.Stack, strings.Join(p.Manifests, ", ")))
		}
	case len(homeless) > 0 && !slices.Contains(roots, rootDir):
		roots = append([]string{rootDir}, roots...)
	}
	projects := make([]Project, 0, len(roots))
	for _, r := range roots {
		projects = append(projects, Project{Pack: p, Root: r})
	}
	return projects, orphans
}

func homelessSources(p *Pack, files, roots []string) []string {
	var out []string
	for _, f := range files {
		if slices.Contains(p.Sources, path.Ext(f)) && nearestRoot(roots, path.Dir(f)) == "" {
			out = append(out, f)
		}
	}
	return out
}

func manifestRoots(files, manifests []string) []string {
	var roots []string
	for _, f := range files {
		if matchesAny(path.Base(f), manifests) && !slices.Contains(roots, path.Dir(f)) {
			roots = append(roots, path.Dir(f))
		}
	}
	slices.Sort(roots)
	return roots
}

func matchesAny(base string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, base); ok {
			return true
		}
	}
	return false
}

func nearestRoot(roots []string, dir string) string {
	best := ""
	for _, r := range roots {
		if (r == rootDir || dir == r || strings.HasPrefix(dir, r+"/")) && len(r) >= len(best) {
			best = r
		}
	}
	return best
}

func unclaimed(files []string, packs []Pack) []string {
	var out []string
	for _, f := range files {
		if lexis.IsCode(f) && owner(packs, path.Ext(f)) == nil {
			out = append(out, fmt.Sprintf(orphanNoPack, f, path.Ext(f)))
		}
	}
	return out
}

func owner(packs []Pack, ext string) *Pack {
	for i := range packs {
		if slices.Contains(packs[i].Sources, ext) {
			return &packs[i]
		}
	}
	return nil
}

func layoutOf(ctx context.Context, ws gate.Workspace, packs []Pack) (Layout, error) {
	files, err := source.Tracked(ctx, ws.Exec, ws.Src)
	return Discover(files, packs), err
}

func Stacks(packs []Pack) Run {
	return func(ctx context.Context, ws gate.Workspace) gate.Outcome {
		layout, err := layoutOf(ctx, ws, packs)
		if err != nil {
			return gate.Fail(err.Error())
		}
		values := map[string]float64{ValueProjects: float64(len(layout.Projects))}
		for _, p := range layout.Projects {
			values[p.Pack.Stack]++
		}
		return gate.Judge(len(layout.Orphans) == 0, reasonOrphans, values, layout.Orphans)
	}
}

type Labeled struct {
	Label string
	gate.Outcome
}

func Dispatch(packs []Pack, check string) Run {
	return func(ctx context.Context, ws gate.Workspace) gate.Outcome {
		layout, err := layoutOf(ctx, ws, packs)
		if err != nil {
			return gate.Fail(err.Error())
		}
		outs := make([]Labeled, 0, len(layout.Projects))
		for _, p := range layout.Projects {
			outs = append(outs, Labeled{Label: p.Label(), Outcome: runIn(ctx, ws, p, check)})
		}
		return Merge(outs)
	}
}

func runIn(ctx context.Context, ws gate.Workspace, p Project, check string) gate.Outcome {
	pws, err := p.Workspace(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	return p.Pack.Checks[check](ctx, pws)
}

var severity = []gate.Status{gate.StatusNA, gate.StatusPass, gate.StatusFail}

func worst(outs []Labeled) gate.Status {
	status := gate.StatusNA
	for _, o := range outs {
		if slices.Index(severity, o.Status) > slices.Index(severity, status) {
			status = o.Status
		}
	}
	return status
}

func Merge(outs []Labeled) gate.Outcome {
	switch len(outs) {
	case 0:
		return gate.Outcome{Status: gate.StatusNA, Reason: reasonNoCode}
	case 1:
		return outs[0].Outcome
	}
	merged := gate.Outcome{Status: worst(outs), Values: map[string]float64{}}
	var reasons []string
	for _, o := range outs {
		for k, v := range o.Values {
			merged.Values[o.Label+valueSeparator+k] = v
		}
		for _, f := range o.Findings {
			merged.Findings = append(merged.Findings, o.Label+": "+f)
		}
		if o.Status == merged.Status && o.Reason != "" {
			reasons = append(reasons, o.Label+": "+o.Reason)
		}
	}
	merged.Reason = strings.Join(reasons, "; ")
	return merged
}
