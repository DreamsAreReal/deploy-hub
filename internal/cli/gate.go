package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/tools"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	VerdictFile    = "verdict.json"
	goModFile      = "go.mod"
	gatePrefix     = "gate: "
	summaryFormat  = "%-22s %-4s %s\n"
	verdictFormat  = "gate %s: %s@%s (%d checks, %.0fs)\n"
	verdictPassed  = "PASSED"
	verdictBlocked = "BLOCKED"
	valueFormat    = "%s=%g"
	ownerSeparator = "/"
	shardSeparator = "/"
	outDirPerm     = 0o700
	toolsPrefix    = "tools: "
	usageTools     = "usage: deploy-hub tools --manifest FILE --dir DIR"
	msgToolsDone   = "tools: %d installed into %s\n"
)

var (
	ErrGateArgs  = errors.New("usage: deploy-hub gate --app NAME --repo OWNER/NAME --commit SHA --policy FILE --src DIR --out DIR [--image REF] [--fork] [--mutation-shard K/N | --mutation-shards DIR]")
	ErrShardSpec = errors.New("--mutation-shard must be K/N with 0 <= K < N and N equal to mutation_shards of the app")
)

type gateArgs struct {
	app, repo, commit, src, out, policy, image string
	shard, shards                              string
	fork                                       bool
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
	fs.BoolVar(&g.fork, "fork", false, "the repository is a fork")
	fs.StringVar(&g.shard, "mutation-shard", "", "run only shard K/N of mutation testing")
	fs.StringVar(&g.shards, "mutation-shards", "", "directory with the reports of every mutation shard (absolute path)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return g, ErrGateArgs
	}
	return g, g.normalize()
}

func (g *gateArgs) normalize() error {
	if !filepath.IsAbs(g.src) || !filepath.IsAbs(g.out) {
		return fmt.Errorf("%w: --src and --out must be absolute paths", ErrGateArgs)
	}
	if (g.shards != "" && !filepath.IsAbs(g.shards)) || (g.shard != "" && g.shards != "") {
		return fmt.Errorf("%w: --mutation-shards must be an absolute path and excludes --mutation-shard", ErrGateArgs)
	}
	g.src, g.out = filepath.Clean(g.src), filepath.Clean(g.out)
	if rel, _ := filepath.Rel(g.src, g.out); !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%w: reports must live outside the source tree", ErrGateArgs)
	}
	return errors.Join(validate.AppName(g.app), validate.RepoSlug(g.repo), validate.Commit(g.commit), nonEmptyArg(g.policy))
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
	if ws.Mutation, err = mutationPlan(&g, ws.App, subj); err != nil {
		a.Err.Println(gatePrefix + err.Error())
		return ExitUsage
	}
	v := gate.Runner{Checks: gateChecks(a.Checks(), ws.Mutation), Now: a.Now, Log: a.Log}.Run(ctx, ws, subj)
	if err := v.Save(ws.Report(VerdictFile)); err != nil {
		return a.failWith(gatePrefix, err)
	}
	a.printVerdict(v)
	return exitCode(v.Passed)
}

func mutationPlan(g *gateArgs, app policy.App, subj gate.Subject) (gate.MutationPlan, error) {
	plan := gate.MutationPlan{Shards: app.MutationShards(), Shard: -1, Merge: filepath.Clean(g.shards), Subject: subj}
	if g.shards == "" {
		plan.Merge = ""
	}
	if g.shard == "" {
		return plan, nil
	}
	k, n, ok := strings.Cut(g.shard, shardSeparator)
	shard, kerr := strconv.Atoi(k)
	shards, nerr := strconv.Atoi(n)
	if !ok || kerr != nil || nerr != nil || shards != plan.Shards || !plan.Sharded() || shard < 0 || shard >= shards {
		return plan, fmt.Errorf("%w (%d)", ErrShardSpec, plan.Shards)
	}
	plan.Shard = shard
	return plan, nil
}

func gateChecks(all []gate.Check, plan gate.MutationPlan) []gate.Check {
	if !plan.ShardRun() {
		return all
	}
	return slices.DeleteFunc(all, func(c gate.Check) bool { return c.Name != pack.CheckMutation })
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
	ws := gate.Workspace{Src: g.src, Out: g.out, PolicyDir: filepath.Dir(g.policy), Image: g.image, AppName: g.app, Repo: g.repo, App: app, Policy: pol, Exec: ex, Log: a.Log}
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
	inst := tools.Installer{Client: &http.Client{}, Dir: *dir, Exec: ex, Log: a.Log}
	if err := inst.Install(ctx, m); err != nil {
		return a.failWith(toolsPrefix, err)
	}
	a.Out.Printf(msgToolsDone, len(m.Binaries)+len(m.Go), *dir)
	return ExitOK
}
