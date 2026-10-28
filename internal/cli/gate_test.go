package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const commitSha = "0123456789abcdef0123456789abcdef01234567"

func gatePolicy(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "policy", "gate.yaml")
	_, err := os.Stat(path)
	testutil.NoErr(t, err)
	return path
}

const (
	flagApp    = "--app"
	flagRepo   = "--repo"
	flagCommit = "--commit"
	flagPolicy = "--policy"
	flagSrc    = "--src"
	flagOut    = "--out"
	testApp    = "web"
	testRepo   = "o/web"
	hubApp     = "deploy-hub"
	hubRepo    = "DreamsAreReal/deploy-hub"
)

func gateFlags(app, repo, commit, policyPath, src, out string) []string {
	args := []string{flagApp, app, flagRepo, repo, flagCommit, commit, flagOut, out}
	if policyPath != "" {
		args = append(args, flagPolicy, policyPath)
	}
	if src != "" {
		args = append(args, flagSrc, src)
	}
	return args
}

func TestParseGateArgs(t *testing.T) {
	src := t.TempDir()
	out := filepath.Join(t.TempDir(), "reports")
	g, err := parseGateArgs(append(gateFlags(testApp, testRepo, commitSha, "p.yaml", src+"/", out), "--image", "web:gate", "--fork"))
	testutil.NoErr(t, err)
	testutil.Equal(t, g, gateArgs{app: testApp, repo: testRepo, commit: commitSha, src: src, out: out, policy: "p.yaml", image: "web:gate", fork: true})
	sibling := filepath.Join(filepath.Dir(src), filepath.Base(src)+"-reports")
	_, err = parseGateArgs(gateFlags(testApp, testRepo, commitSha, "p", src, sibling))
	testutil.NoErr(t, err)
	bad := [][]string{
		gateFlags(testApp, testRepo, commitSha, "p", src, filepath.Join(src, "inside")),
		gateFlags(testApp, testRepo, commitSha, "p", src, src),
		gateFlags("Web!", testRepo, commitSha, "p", src, out),
		gateFlags(testApp, testApp, commitSha, "p", src, out),
		gateFlags(testApp, testRepo, "abc", "p", src, out),
		gateFlags(testApp, testRepo, commitSha, "", src, out),
		gateFlags(testApp, testRepo, commitSha, "p", "relative", out),
		gateFlags(testApp, testRepo, commitSha, "p", src, "relative"),
		gateFlags(testApp, testRepo, commitSha, "p", "", out),
		{"--nope"},
		{flagApp, testApp, "extra"},
	}
	for _, args := range bad {
		_, err := parseGateArgs(args)
		testutil.Fails(t, err)
	}
}

func TestGateCommandBlocksAndWritesVerdict(t *testing.T) {
	h := newHarness(t)
	src := t.TempDir()
	testutil.Write(t, filepath.Join(src, "go.mod"), "module example.com/web\n")
	h.exec.Handler = func(c procexec.Command) (procexec.Result, error) {
		if c.Name == goBin && len(c.Args) > 1 && c.Args[0] == "list" && c.Args[1] == "-m" {
			return procexec.Result{Stdout: webModule}, nil
		}
		return procexec.Result{}, nil
	}
	out := filepath.Join(t.TempDir(), "gate")
	code := h.run(append([]string{CmdGate}, gateFlags(hubApp, hubRepo, commitSha, gatePolicy(t), src, out)...)...)
	testutil.Equal(t, code, ExitFail)
	var v gate.Verdict
	testutil.NoErr(t, json.Unmarshal([]byte(testutil.Read(t, filepath.Join(out, VerdictFile))), &v))
	testutil.True(t, !v.Passed && v.Own && v.App == hubApp, "blocked own verdict")
	testutil.Contains(t, h.out.String(), "registration           pass", "gate BLOCKED: deploy-hub@"+commitSha)
	testutil.True(t, len(h.exec.Calls()) > len(v.Results)/2, "tools were invoked")
}

func TestGateCommandPasses(t *testing.T) {
	h := newHarness(t)
	h.gateChecks = []gate.Check{{Name: checkOnly, Run: func(context.Context, gate.Workspace) gate.Outcome { return gate.Pass(nil) }}}
	out := filepath.Join(t.TempDir(), "gate")
	code := h.run(append([]string{CmdGate}, gateFlags(hubApp, hubRepo, commitSha, gatePolicy(t), t.TempDir(), out)...)...)
	testutil.Equal(t, code, ExitOK)
	testutil.Contains(t, h.out.String(), "only                   pass", "gate PASSED: deploy-hub@"+commitSha+" (1 checks")
	testutil.Contains(t, testutil.Read(t, filepath.Join(out, VerdictFile)), `"passed":true`)
}

func TestGateCommandErrors(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdGate), ExitUsage)
	testutil.Contains(t, h.err.String(), "gate: usage")
	out := filepath.Join(t.TempDir(), "o")
	gateRun := func(h *harness, policyPath, src, out string) int {
		return h.run(append([]string{CmdGate}, gateFlags(testApp, testRepo, commitSha, policyPath, src, out)...)...)
	}
	testutil.Equal(t, gateRun(h, filepath.Join(t.TempDir(), "missing.yaml"), t.TempDir(), out), ExitFail)
	src := t.TempDir()
	testutil.Write(t, filepath.Join(src, "go.mod"), "module m\n")
	h.exec.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }
	testutil.Equal(t, gateRun(h, gatePolicy(t), src, out), ExitFail)
	testutil.Contains(t, h.err.String(), "go list -m")
	blocker := filepath.Join(t.TempDir(), "file")
	testutil.Write(t, blocker, "")
	testutil.Equal(t, gateRun(h, gatePolicy(t), t.TempDir(), filepath.Join(blocker, "x")), ExitFail)
	h2 := newHarness(t)
	h2.exec.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, nil }
	verdictBlocked := t.TempDir()
	testutil.Mkdir(t, filepath.Join(verdictBlocked, VerdictFile))
	testutil.Equal(t, gateRun(h2, gatePolicy(t), t.TempDir(), verdictBlocked), ExitFail)
	testutil.Contains(t, h2.err.String(), "gate: ")
}

func TestPrintVerdict(t *testing.T) {
	var b bytes.Buffer
	a := &App{Out: console.New(&b)}
	a.printVerdict(gate.Verdict{Subject: gate.Subject{App: testApp, Commit: "c"}, Seconds: 12, Results: []gate.Result{
		{Check: "tests", Status: gate.StatusPass, Values: map[string]float64{"b": 2, "a": 1.5}, Findings: []string{"kept in the verdict only"}},
		{Check: "dast", Status: gate.StatusNA, Reason: "no HTTP", Findings: []string{"n/a detail"}},
		{Check: "sast", Status: gate.StatusFail, Reason: "found", Findings: []string{"x"}},
	}})
	testutil.Equal(t, strings.Split(strings.TrimSpace(b.String()), "\n"), []string{
		"tests                  pass a=1.5; b=2",
		"dast                   n/a  no HTTP",
		"sast                   fail found",
		"    x",
		"gate BLOCKED: web@c (3 checks, 12s)",
	})
	n, err := nopWriter{}.Write([]byte("abc"))
	testutil.NoErr(t, err)
	testutil.Equal(t, n, 3)
}

func TestToolsCommand(t *testing.T) {
	h := newHarness(t)
	testutil.Equal(t, h.run(CmdTools), ExitUsage)
	testutil.Equal(t, h.run(CmdTools, "--manifest", "m"), ExitUsage)
	testutil.Equal(t, h.run(CmdTools, "--dir", "d"), ExitUsage)
	testutil.Equal(t, h.run(CmdTools, "--manifest", "m", "--dir", "d", "extra"), ExitUsage)
	testutil.Equal(t, h.run(CmdTools, "--bogus"), ExitUsage)
	testutil.Contains(t, h.err.String(), usageTools)
	dir := t.TempDir()
	testutil.Equal(t, h.run(CmdTools, "--manifest", filepath.Join(dir, "missing.yaml"), "--dir", dir), ExitFail)
	manifest := filepath.Join(dir, "tools.yaml")
	testutil.Write(t, manifest, "go:\n  - example.com/x/cmd/y@v1.2.3\n")
	h.exec.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }
	testutil.Equal(t, h.run(CmdTools, "--manifest", manifest, "--dir", filepath.Join(dir, "bin")), ExitFail)
	testutil.Contains(t, h.err.String(), "tools: go install example.com/x/cmd/y@v1.2.3")
	h.exec.Handler = nil
	h.out.Reset()
	testutil.Equal(t, h.run(CmdTools, "--manifest", manifest, "--dir", filepath.Join(dir, "bin")), ExitOK)
	testutil.Equal(t, h.out.String(), "tools: 1 installed into "+filepath.Join(dir, "bin")+"\n")
	testutil.Equal(t, h.exec.Lines()[len(h.exec.Lines())-1], "go install example.com/x/cmd/y@v1.2.3")
}

func TestBadJWKSURLIsRejected(t *testing.T) {
	for _, bad := range []string{"", "ftp://keys", "not a url"} {
		_, err := LoadConfig(func(k string) (string, bool) {
			if k == envJWKSURL {
				return bad, true
			}
			return "", false
		})
		if bad == "" {
			testutil.NoErr(t, err)
			continue
		}
		testutil.Is(t, err, ErrJWKSURL)
	}
}

func TestEveryHandlerBinaryIsAllowed(t *testing.T) {
	for name, h := range handlers {
		for _, bin := range h.binaries {
			_, err := procexec.NewOS(bin).Run(context.Background(), procexec.Command{Name: bin, Dir: filepath.Join(t.TempDir(), "missing")})
			testutil.True(t, !errors.Is(err, procexec.ErrNotAllowed), name+" needs "+bin+" in the subprocess table")
		}
	}
}

const (
	flagCache    = "--mutation-cache"
	flagCacheSum = "--mutation-cache-sha256"
	checkOnly    = "only"
	verdictKey   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cacheSum     = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestParseGateArgsMutationCache(t *testing.T) {
	src := t.TempDir()
	out := filepath.Join(t.TempDir(), "reports")
	cache := filepath.Join(t.TempDir(), "verdicts.json")
	base := gateFlags(testApp, testRepo, commitSha, "p", src, out)
	g, err := parseGateArgs(append(base, flagCache, cache, flagCacheSum, cacheSum))
	testutil.NoErr(t, err)
	testutil.Equal(t, g.cache, cache)
	bad := [][]string{
		{flagCache, cache},
		{flagCacheSum, cacheSum},
		{flagCache, "relative.json", flagCacheSum, cacheSum},
		{flagCache, filepath.Join(src, "planted.json"), flagCacheSum, cacheSum},
		{flagCache, filepath.Join(src, "..planted.json"), flagCacheSum, cacheSum},
		{flagCache, cache, flagCacheSum, "0123"},
	}
	for _, extra := range bad {
		_, err := parseGateArgs(append(append([]string{}, base...), extra...))
		testutil.Fails(t, err)
	}
}

func TestGateCommandHandsTheCacheOverAndKeepsTheVerdicts(t *testing.T) {
	h := newHarness(t)
	var seen gate.MutationPlan
	h.gateChecks = []gate.Check{{Name: checkOnly, Run: func(_ context.Context, ws gate.Workspace) gate.Outcome {
		seen = ws.Mutation
		v := mutation.Verdicts{Version: 1, Bases: []string{verdictKey}, Entries: map[string]mutation.Status{verdictKey: mutation.StatusKilled}}
		testutil.Mkdir(t, filepath.Join(ws.Out, "go-app"))
		_, err := v.Save(filepath.Join(ws.Out, "go-app", mutation.VerdictsFile))
		testutil.NoErr(t, err)
		return gate.Pass(nil)
	}}}
	out := filepath.Join(t.TempDir(), "gate")
	cache := filepath.Join(t.TempDir(), "verdicts.json")
	args := append(gateFlags(hubApp, hubRepo, commitSha, gatePolicy(t), t.TempDir(), out), flagCache, cache, flagCacheSum, cacheSum)
	testutil.Equal(t, h.run(append([]string{CmdGate}, args...)...), ExitOK)
	testutil.Equal(t, seen, gate.MutationPlan{Cache: cache, CacheSHA256: strings.TrimPrefix(cacheSum, digestPrefix)})
	merged := testutil.Read(t, filepath.Join(out, mutation.VerdictsFile))
	testutil.Contains(t, h.out.String(), "mutation verdicts: "+filepath.Join(out, mutation.VerdictsFile)+" sha256:"+mutation.Digest([]byte(merged)))
	h2 := newHarness(t)
	h2.gateChecks = []gate.Check{{Name: checkOnly, Run: func(_ context.Context, ws gate.Workspace) gate.Outcome {
		testutil.Write(t, filepath.Join(ws.Out, "go-app", mutation.VerdictsFile), "{")
		return gate.Pass(nil)
	}}}
	testutil.Equal(t, h2.run(append([]string{CmdGate}, gateFlags(hubApp, hubRepo, commitSha, gatePolicy(t), t.TempDir(), filepath.Join(t.TempDir(), "g"))...)...), ExitFail)
}

func TestEngineVersionCommand(t *testing.T) {
	h := newHarness(t)
	h.exec.Handler = func(c procexec.Command) (procexec.Result, error) {
		if slices.Contains(c.Args, "{{.Dir}}") {
			return procexec.Result{Stdout: "/nowhere\n"}, nil
		}
		return procexec.Result{Stdout: ""}, nil
	}
	testutil.Equal(t, h.run(CmdEngine), ExitOK)
	testutil.Equal(t, len(strings.TrimSpace(h.out.String())), 64)
	testutil.Equal(t, h.run(CmdEngine, "extra"), ExitUsage)
	testutil.Contains(t, h.err.String(), usageEngine)
	h.exec.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }
	testutil.Equal(t, h.run(CmdEngine), ExitFail)
	testutil.Contains(t, h.err.String(), enginePrefix)
}

func TestKeepVerdictsNeedsAReadableReportDirectory(t *testing.T) {
	h := newHarness(t)
	out := t.TempDir()
	a := &App{Out: console.New(h.out)}
	testutil.NoErr(t, a.keepVerdicts(gate.Workspace{Out: out}))
	testutil.Missing(t, filepath.Join(out, mutation.VerdictsFile))
	testutil.Equal(t, h.out.String(), "")
	file := filepath.Join(t.TempDir(), "file")
	testutil.Write(t, file, "")
	testutil.Fails(t, a.keepVerdicts(gate.Workspace{Out: file}))
}
