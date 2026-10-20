package node

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	approvedPrefix   = "npm:"
	tsconfigFile     = "tsconfig.json"
	minLockVersion   = 2
	registryPrefix   = "https://"
	formatJSON       = "json"
	flagConfig       = "--config"
	flagFormat       = "--format"
	reasonIntegrity  = "dependencies are not locked with integrity hashes"
	reasonLint       = "linters and the strict type checker reported findings"
	reasonComplexity = "functions exceed the cognitive complexity limit"
	reasonDeadCode   = "unused files, exports or dependencies"
	lockVersionForm  = "%s: lockfileVersion %d is older than %d"
	noIntegrityForm  = "%s: %s has no integrity hash or registry source"
	eslintForm       = "%s:%d %s %s"
	knipForm         = "%s: unused %s %s"
)

var (
	tsDiagnostic = regexp.MustCompile(`^(.+)\((\d+),\d+\): error (TS\d+): (.*)$`)
	tsSources    = []string{extTS, extMTS, extCTS, extTSX}
)

type lockfile struct {
	LockfileVersion int `json:"lockfileVersion"`
	Packages        map[string]struct {
		Integrity string `json:"integrity"`
		Resolved  string `json:"resolved"`
		Link      bool   `json:"link"`
		InBundle  bool   `json:"inBundle"`
	} `json:"packages"`
}

func readLock(dir string) (lockfile, error) {
	var l lockfile
	data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, Lockfile)))
	if err != nil {
		return l, ErrNoLock
	}
	return l, json.Unmarshal(data, &l)
}

func Integrity(ctx context.Context, ws gate.Workspace) gate.Outcome {
	lock, err := readLock(ws.Src)
	if err != nil {
		return gate.Fail(err.Error())
	}
	findings := lockFindings(lock)
	return pack.Locked(ctx, ws, reasonIntegrity, findings, prepare)
}

func lockFindings(lock lockfile) []string {
	var findings []string
	if lock.LockfileVersion < minLockVersion {
		findings = append(findings, fmt.Sprintf(lockVersionForm, Lockfile, lock.LockfileVersion, minLockVersion))
	}
	for _, key := range sortedKeys(lock.Packages) {
		p := lock.Packages[key]
		if key != "" && !p.Link && !p.InBundle && (p.Integrity == "" || !strings.HasPrefix(p.Resolved, registryPrefix)) {
			findings = append(findings, fmt.Sprintf(noIntegrityForm, Lockfile, packageName(key)))
		}
	}
	return findings
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func packageName(key string) string {
	if i := strings.LastIndex(key, modulesDir+"/"); i >= 0 {
		return key[i+len(modulesDir)+1:]
	}
	return key
}

func Dependencies(_ context.Context, ws gate.Workspace) gate.Outcome {
	lock, err := readLock(ws.Src)
	if err != nil {
		return gate.Fail(err.Error())
	}
	return pack.Approval(ws, approvedPrefix, lockedNames(lock), func(name string) string { return name })
}

func lockedNames(lock lockfile) []string {
	var names []string
	for _, key := range sortedKeys(lock.Packages) {
		name := packageName(key)
		if key != "" && !lock.Packages[key].Link && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

type eslintFile struct {
	FilePath string `json:"filePath"`
	Messages []struct {
		RuleID  string `json:"ruleId"`
		Message string `json:"message"`
		Line    int    `json:"line"`
	} `json:"messages"`
}

func eslint(ctx context.Context, ws gate.Workspace, config string) ([]string, error) {
	res, err := node(ctx, ws, nil, toolScript(ws, "eslint", "bin", "eslint.js"), flagConfig, filepath.Join(tools(ws), config), "--no-config-lookup",
		flagFormat, formatJSON, ".")
	var files []eslintFile
	if perr := json.Unmarshal([]byte(res.Stdout), &files); perr != nil {
		return nil, fmt.Errorf("eslint: %w: %s", perr, gate.Tail(procexec.Output(err)))
	}
	var out []string
	for _, f := range files {
		for _, m := range f.Messages {
			out = append(out, fmt.Sprintf(eslintForm, rel(ws, f.FilePath), m.Line, m.RuleID, m.Message))
		}
	}
	return out, nil
}

func runLint(ctx context.Context, ws gate.Workspace) gate.Outcome {
	findings, err := eslint(ctx, ws, eslintConfigFile)
	if err != nil {
		return gate.Fail(err.Error())
	}
	types, err := typecheck(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	return gate.Findings(reasonLint, append(findings, types...))
}

func typecheck(ctx context.Context, ws gate.Workspace) ([]string, error) {
	if !hasTypeScript(stage(ws)) {
		return nil, nil
	}
	tsc := filepath.Join(stage(ws), modulesDir, "typescript", "bin", "tsc")
	if _, err := os.Stat(tsc); err != nil {
		tsc = toolScript(ws, "typescript", "bin", "tsc")
	}
	args := []string{tsc, "--noEmit", "--strict", "--pretty", "false"}
	if _, err := os.Stat(filepath.Join(stage(ws), tsconfigFile)); err == nil {
		args = append(args, "-p", tsconfigFile)
	}
	res, err := node(ctx, ws, nil, args...)
	var out []string
	sc := bufio.NewScanner(strings.NewReader(res.Stdout))
	for sc.Scan() {
		if m := tsDiagnostic.FindStringSubmatch(sc.Text()); m != nil {
			out = append(out, fmt.Sprintf("%s:%s %s %s", m[1], m[2], m[3], m[4]))
		}
	}
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return out, nil
}

func hasTypeScript(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() && d.Name() == modulesDir {
			return filepath.SkipDir
		}
		found = found || slices.Contains(tsSources, filepath.Ext(path))
		return nil
	})
	return found
}

func runComplexity(ctx context.Context, ws gate.Workspace) gate.Outcome {
	findings, err := eslint(ctx, ws, complexityConfigFile)
	if err != nil {
		return gate.Fail(err.Error())
	}
	return gate.Findings(reasonComplexity, findings)
}

type knipReport struct {
	Files  []string                     `json:"files"`
	Issues []map[string]json.RawMessage `json:"issues"`
}

func runDeadCode(ctx context.Context, ws gate.Workspace) gate.Outcome {
	res, err := node(ctx, ws, nil, toolScript(ws, "knip", "bin", "knip.js"), flagConfig, filepath.Join(tools(ws), knipConfigFile),
		"--reporter", formatJSON, "--no-progress", "--no-config-hints")
	var rep knipReport
	if perr := json.Unmarshal([]byte(res.Stdout), &rep); perr != nil {
		return gate.Fail(fmt.Sprintf("knip: %v: %s", perr, gate.Tail(procexec.Output(err))))
	}
	findings := make([]string, 0, len(rep.Files))
	for _, f := range rep.Files {
		findings = append(findings, fmt.Sprintf(knipForm, f, "file", ""))
	}
	for _, issue := range rep.Issues {
		findings = append(findings, knipIssues(issue)...)
	}
	return gate.Findings(reasonDeadCode, findings)
}

func knipIssues(issue map[string]json.RawMessage) []string {
	var file string
	_ = json.Unmarshal(issue["file"], &file)
	var out []string
	for _, kind := range sortedKeys(issue) {
		var items []struct {
			Name string `json:"name"`
		}
		if kind == "file" || json.Unmarshal(issue[kind], &items) != nil {
			continue
		}
		for _, it := range items {
			out = append(out, fmt.Sprintf(knipForm, file, kind, it.Name))
		}
	}
	return out
}
