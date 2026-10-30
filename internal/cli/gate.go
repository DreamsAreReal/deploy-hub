package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/tools"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	VerdictFile       = "verdict.json"
	toolCacheEnv      = "RUNNER_TOOL_CACHE"
	toolCacheDir      = "deploy-hub-tools"
	toolsRetry        = 5 * time.Second
	toolsTLSHandshake = 30 * time.Second
	goModFile         = "go.mod"
	gatePrefix        = "gate: "
	summaryFormat     = "%-22s %-4s %s\n"
	verdictFormat     = "gate %s: %s@%s (%d checks, %.0fs)\n"
	verdictPassed     = "PASSED"
	verdictBlocked    = "BLOCKED"
	valueFormat       = "%s=%g"
	ownerSeparator    = "/"
	outDirPerm        = 0o700
	toolsPrefix       = "tools: "
	pathEnv           = "PATH"
	usageTools        = "usage: deploy-hub tools --manifest FILE --dir DIR"
	msgToolsDone      = "tools: %d installed into %s\n"
	usageEngine       = "usage: deploy-hub engine-version (run in the deploy-hub checkout)"
	enginePrefix      = "engine-version: "
	digestPrefix      = "sha256:"
	msgVerdicts       = "mutation verdicts: %s %s%s\n"
)

var ErrGateArgs = errors.New("usage: deploy-hub gate --app NAME --repo OWNER/NAME --commit SHA --policy FILE --src DIR --out DIR [--image REF] [--previous-image REF:sha-COMMIT] [--fork] [--mutation-cache FILE --mutation-cache-sha256 sha256:HEX] [--mutation-full] [--mutation-budget DURATION] [--mutation-last FILE] [--mutation-yield FILE]")

type gateArgs struct {
	app, repo, commit, src, out, policy, image string
	cache, cacheSum, last, yield, baseline     string
	previous                                   string
	budget                                     time.Duration
	fork, full                                 bool
}

func parseGateArgs(args []string) (gateArgs, error) {
	var g gateArgs
	fs := flag.NewFlagSet(CmdGate, flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	fs.StringVar(&g.app, "app", "", "app name")
	fs.StringVar(&g.repo, "repo", "", "repository owner/name")
	fs.StringVar(&g.commit, "commit", "", "commit sha")
	fs.StringVar(&g.src, "src", "", "repository checkout (absolute path)")
	fs.StringVar(&g.out, "out", "", "report directory outside the checkout")
	fs.StringVar(&g.policy, "policy", "", "central gate policy file")
	fs.StringVar(&g.image, "image", "", "locally built image reference")
	fs.StringVar(&g.previous, "previous-image", "", "image of the deployed release, pulled locally, for the rollback check")
	fs.BoolVar(&g.fork, "fork", false, "the repository is a fork")
	fs.StringVar(&g.cache, "mutation-cache", "", "mutation verdicts from the hub (absolute path)")
	fs.StringVar(&g.cacheSum, "mutation-cache-sha256", "", "sha256 the hub recorded for the mutation verdicts")
	fs.BoolVar(&g.full, "mutation-full", false, "a chunk of a full mutation run: only tests and mutation run")
	fs.DurationVar(&g.budget, "mutation-budget", 0, "stop mutation testing after this long and keep the finished verdicts")
	fs.StringVar(&g.last, "mutation-last", "", "the last full mutation run the hub recorded (absolute path)")
	fs.StringVar(&g.yield, "mutation-yield", "", "stop mutation testing as soon as this file exists (absolute path)")
	fs.StringVar(&g.baseline, "ui-baseline", "", "screenshots of the deployed version from the hub (absolute path)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return g, ErrGateArgs
	}
	return g, g.normalize()
}

func (g *gateArgs) normalize() error {
	if !filepath.IsAbs(g.src) || !filepath.IsAbs(g.out) {
		return fmt.Errorf("%w: --src and --out must be absolute paths", ErrGateArgs)
	}
	g.src, g.out = filepath.Clean(g.src), filepath.Clean(g.out)
	if rel, _ := filepath.Rel(g.src, g.out); !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%w: reports must live outside the source tree", ErrGateArgs)
	}
	return errors.Join(validate.AppName(g.app), validate.RepoSlug(g.repo), validate.Commit(g.commit), nonEmptyArg(g.policy), g.cacheArgs(), g.fullRunArgs(), g.baselineArgs(), g.previousArgs())
}

func (g *gateArgs) previousArgs() error {
	if g.previous == "" {
		return nil
	}
	repo, tag, ok := strings.Cut(g.previous, ":")
	if !ok || validate.Image(repo) != nil || validate.Tag(tag) != nil {
		return fmt.Errorf("%w: --previous-image must be ghcr.io/<owner>/<app>:sha-<commit>", ErrGateArgs)
	}
	return nil
}

func (g *gateArgs) baselineArgs() error {
	if g.baseline == "" {
		return nil
	}
	if !filepath.IsAbs(g.baseline) || !outside(g.src, filepath.Clean(g.baseline)) {
		return fmt.Errorf("%w: --ui-baseline needs an absolute path outside the source tree", ErrGateArgs)
	}
	g.baseline = filepath.Clean(g.baseline)
	return nil
}

func outside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func (g *gateArgs) cacheArgs() error {
	if g.cache == "" && g.cacheSum == "" {
		return nil
	}
	if !filepath.IsAbs(g.cache) || !outside(g.src, filepath.Clean(g.cache)) || validate.Digest(g.cacheSum) != nil {
		return fmt.Errorf("%w: --mutation-cache needs an absolute path outside the source tree and --mutation-cache-sha256 sha256:HEX", ErrGateArgs)
	}
	return nil
}

func nonEmptyArg(v string) error {
	if v == "" {
		return ErrGateArgs
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func runGate(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	g, err := parseGateArgs(args)
	if err != nil {
		a.Err.Println(gatePrefix + err.Error())
		return ExitUsage
	}
	pol, err := policy.LoadFile(g.policy)
	if err != nil {
		return a.failWith(gatePrefix, err)
	}
	ws, err := newWorkspace(ctx, &g, a, ex, pol)
	if err != nil {
		return a.failWith(gatePrefix, err)
	}
	owner, _, _ := strings.Cut(g.repo, ownerSeparator)
	subj := gate.Subject{App: g.app, Repo: g.repo, Commit: g.commit, Own: pol.Owns(owner) && !g.fork}
	v := gate.Runner{Checks: gateChecks(a.Checks(), g.full), Now: a.Now, Log: a.Log}.Run(ctx, ws, subj)
	if err := v.Save(ws.Report(VerdictFile)); err != nil {
		return a.failWith(gatePrefix, err)
	}
	if err := errors.Join(a.keepVerdicts(ws), a.keepRunSummary(ws)); err != nil {
		return a.failWith(gatePrefix, err)
	}
	a.printVerdict(v)
	return exitCode(v.Passed)
}

func exitCode(passed bool) int {
	if passed {
		return ExitOK
	}
	return ExitFail
}

func newWorkspace(ctx context.Context, g *gateArgs, a *App, ex procexec.Runner, pol policy.Policy) (gate.Workspace, error) {
	if err := os.MkdirAll(g.out, outDirPerm); err != nil {
		return gate.Workspace{}, err
	}
	app, _ := pol.App(g.app)
	ws := gate.Workspace{Src: g.src, Out: g.out, PolicyDir: filepath.Dir(g.policy), Image: g.image, AppName: g.app, Repo: g.repo, App: app, Policy: pol, Exec: ex, Log: a.Log,
		UIBaseline: g.baseline, Previous: g.previous}
	plan, err := g.mutationPlan()
	if err != nil {
		return ws, err
	}
	ws.Mutation = plan
	if !exists(filepath.Join(g.src, goModFile)) {
		return ws, nil
	}
	module, err := golang.Module(ctx, ws)
	ws.Module = module
	return ws, err
}

func (a *App) printVerdict(v gate.Verdict) {
	for _, r := range v.Results {
		a.Out.Printf(summaryFormat, r.Check, r.Status, describe(r))
		if r.Status != gate.StatusFail {
			continue
		}
		for _, f := range r.Findings {
			a.Out.Println("    " + f)
		}
	}
	word := verdictPassed
	if !v.Passed {
		word = verdictBlocked
	}
	a.Out.Printf(verdictFormat, word, v.App, v.Commit, len(v.Results), v.Seconds)
}

func describe(r gate.Result) string {
	keys := make([]string, 0, len(r.Values))
	for k := range r.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	if r.Reason != "" {
		parts = append(parts, r.Reason)
	}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(valueFormat, k, r.Values[k]))
	}
	return strings.Join(parts, "; ")
}

func runTools(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	fs := flag.NewFlagSet(CmdTools, flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	manifest := fs.String("manifest", "", "tools manifest")
	dir := fs.String("dir", "", "install directory")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *manifest == "" || *dir == "" {
		a.Err.Println(usageTools)
		return ExitUsage
	}
	m, err := tools.LoadManifest(*manifest)
	if err != nil {
		return a.failWith(toolsPrefix, err)
	}
	if err := os.Setenv(pathEnv, *dir+string(os.PathListSeparator)+os.Getenv(pathEnv)); err != nil {
		return a.failWith(toolsPrefix, err)
	}
	inst := tools.Installer{Client: toolsClient(), Dir: *dir, Cache: toolsCache(), Retry: toolsRetry, Exec: ex, Log: a.Log}
	if err := inst.Install(ctx, m); err != nil {
		return a.failWith(toolsPrefix, err)
	}
	a.Out.Printf(msgToolsDone, m.Count(), *dir)
	return ExitOK
}

func toolsClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: toolsTLSHandshake, ForceAttemptHTTP2: true}}
}

func toolsCache() string {
	if dir := os.Getenv(toolCacheEnv); dir != "" {
		return filepath.Join(dir, toolCacheDir)
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, toolCacheDir)
}

func runEngineVersion(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	dir, err := os.Getwd()
	if err != nil || len(args) != 0 {
		a.Err.Println(usageEngine)
		return ExitUsage
	}
	v, err := mutation.SourceVersion(ctx, gate.Workspace{Src: dir, Exec: ex, Log: a.Log})
	if err != nil {
		return a.failWith(enginePrefix, err)
	}
	a.Out.Println(v)
	return ExitOK
}
