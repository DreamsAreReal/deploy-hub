package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/user"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/delivery"
	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/catalog"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/dotnet"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/node"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/python"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/scan"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/shell"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/hostsync"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/onboard"
	"github.com/DreamsAreReal/deploy-hub/internal/preempt"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/runner"
	"github.com/DreamsAreReal/deploy-hub/internal/secrets"
	"github.com/DreamsAreReal/deploy-hub/internal/selfupdate"
	"github.com/DreamsAreReal/deploy-hub/internal/tools"
)

const otherSource = "other"

const (
	ExitOK    = 0
	ExitFail  = 1
	ExitUsage = 2

	CmdRunner     = "runner"
	CmdRender     = "render"
	CmdArchReport = "arch-report"
	CmdCaddySync  = "caddy-sync"
	CmdPrune      = "prune"
	CmdProvision  = "provision"
	CmdFacts      = "facts"
	CmdOnboard    = "onboard"
	CmdGate       = "gate"
	CmdTools      = "tools"
	CmdMCP        = "mcp"
	CmdHostSync   = "host-sync"
	CmdSelfUpdate = "self-update"
	CmdSudoers    = "sudoers-sync"
	CmdVersion    = "version"
	CmdEngine     = "engine-version"
	CmdDispatch   = "mutation-dispatch"
	CmdImport     = "mutation-import"
	CmdChunk      = "mutation-run"
	CmdRunnerSync = "ci-runner-sync"
	CmdBootstrap  = "bootstrap"
	CmdPreempt    = "preempt"
	CmdMainGuard  = "main-guard"
	CmdScaffold   = "scaffold"

	binDocker    = "docker"
	binSudo      = "sudo"
	binCaddy     = "caddy"
	binSystemctl = "systemctl"
	binGH        = "gh"
	binGit       = "git"
	binSSH       = "ssh"

	usage = `usage: deploy-hub <command> [args]
commands:
  runner [request]                 SSH forced command (request from SSH_ORIGINAL_COMMAND)
  render <app> <in> <out>          render a repo compose into a deploy-ready one
  caddy-sync                       regenerate the Caddyfile from apps.list
  host-sync <app> <tag>            install the app's gated host files as root (policy host spec)
  self-update [sha-<commit>]       replace this binary with a released, gated build (root)
  sudoers-sync                     write the sudoers rule of the runner from the policy (root)
  ci-runner-sync                   install the CI runner host files of this build (root)
  bootstrap [sha-<commit>]         install the first gated build on a fresh hub (run on the operator machine)
  preempt                          let main deploys overtake pull request gates on the runner (root, timer)
  main-guard                       alert on a main that moved without a green pull request (root, timer)
  version                          print the commit this binary was built from
  prune                            delete old app images (keep running + 2 previous)
  provision --app NAME ...         prepare /opt/<app> on the VPS (root)
  facts <app>                      print server facts for onboarding
  onboard <repo> [options]         connect a GitHub repo (run on the operator machine)
  scaffold <app> --port N [...]    write a new service from the template that passes the gate (run in a hub clone)
  gate --app NAME --repo O/N ...   run the deploy quality gate on a checkout (CI)
  tools --manifest FILE --dir DIR  install the pinned gate toolchain (CI)
  engine-version                   print the sha256 of the mutation engine sources (make passes it to the build)
  mutation-dispatch <app>          plan the next full mutation chunk of an app for its CI user (root)
  mutation-import <app>            take the verdicts of the last full mutation chunk into the hub (root)
  mutation-run --spool DIR ...     run one full mutation chunk as the app's CI user
  mcp                              MCP server for AI agents on stdio (run on the operator machine)`
)

type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type App struct {
	Config       Config
	IO           IO
	Out          *console.Printer
	Err          *console.Printer
	Exec         func(names ...string) procexec.Runner
	Now          func() time.Time
	Log          *slog.Logger
	Owner        func(username string) func(path string) error
	Group        func(group string) func(path string) error
	Checks       func() []gate.Check
	Sudoers      string
	Token        string
	CIRoot       string
	PreemptState string
	source       string
	textfile     *obs.Textfile
}

type handler struct {
	run      func(ctx context.Context, a *App, ex procexec.Runner, args []string) int
	binaries []string
	onServer bool
	verbose  bool
}

var sourceWord = regexp.MustCompile(`^[a-z][a-z-]{0,39}$`)

var handlers = map[string]handler{
	CmdRunner:     {run: runRunner, binaries: []string{binDocker, binSudo}, onServer: true},
	CmdRender:     {run: runRender},
	CmdArchReport: {run: runArchReport},
	CmdCaddySync:  {run: runCaddySync, binaries: []string{binCaddy, binSystemctl}, onServer: true},
	CmdPrune:      {run: runPrune, binaries: []string{binDocker}, onServer: true},
	CmdProvision:  {run: runProvision, binaries: []string{binCaddy, binSystemctl}, onServer: true},
	CmdFacts:      {run: runFacts, onServer: true},
	CmdOnboard:    {run: runOnboard, binaries: []string{binGH, binGit, binSSH, onboard.SSHKeygen}},
	CmdScaffold:   {run: runScaffold},
	CmdGate:       {run: runGate, binaries: gateBinaries, verbose: true},
	CmdTools:      {run: runTools, binaries: []string{tools.GoBinary, tools.TarBinary, tools.CmakeBinary, tools.CCBinary, tools.LemonBinary, tools.MakeBinary}, verbose: true},
	CmdMCP:        {run: runMCP, binaries: []string{binSSH}},
	CmdSelfUpdate: {run: runSelfUpdate, binaries: []string{selfupdate.InstalledCommand, selfupdate.CandidateCommand, selfupdate.VisudoCommand, selfupdate.SetupCommand, procexec.CIRunnerSystemctl}, onServer: true},
	CmdRunnerSync: {run: runRunnerSync, binaries: []string{selfupdate.SetupCommand, procexec.CIRunnerSystemctl}, onServer: true},
	CmdSudoers:    {run: runSudoersSync, binaries: []string{selfupdate.VisudoCommand}, onServer: true},
	CmdVersion:    {run: runVersion},
	CmdBootstrap:  {run: runBootstrap, binaries: []string{selfupdate.SSHBinary}},
	CmdPreempt:    {run: runPreempt, onServer: true},
	CmdMainGuard:  {run: runMainGuard, onServer: true},
	CmdHostSync:   {run: runHostSync, binaries: []string{hostsync.DockerBinary, hostsync.SystemctlBinary, hostsync.AnalyzeBinary, hostsync.CaddyBinary}, onServer: true},
	CmdEngine:     {run: runEngineVersion, binaries: []string{tools.GoBinary}},
	CmdDispatch:   {run: runMutationDispatch, onServer: true},
	CmdImport:     {run: runMutationImport, onServer: true},
	CmdChunk:      {run: runMutationRun, binaries: chunkBinaries, verbose: true},
}

var gateBinaries = []string{
	golang.GoBinary, source.GitBinary, golang.LintBinary, golang.CognitBinary, golang.DeadcodeBinary, golang.GobcoBinary,
	golang.VulncheckBinary, scan.GitleaksBinary, scan.TrufflehogBinary, scan.DockerBinary, scan.OSVBinary, scan.ZizmorBinary,
	scan.ActionlintBinary, scan.HadolintBinary, scan.TrivyBinary, scan.SyftBinary, python.UVBinary, python.RuffBinary,
	node.NodeBinary, node.NPMBinary, dotnet.Binary, shell.ShellcheckBinary, shell.ShfmtBinary, shell.BatsBinary, shell.BashBinary,
}

type Deps struct {
	Exec         func(names ...string) procexec.Runner
	Owner        func(username string) func(path string) error
	Group        func(group string) func(path string) error
	Now          func() time.Time
	Checks       func() []gate.Check
	Sudoers      string
	Token        string
	CIRoot       string
	PreemptState string
}

func OSDeps() Deps {
	return Deps{Exec: func(names ...string) procexec.Runner { return procexec.NewOS(names...) }, Owner: ChownTo, Group: GroupTo, Now: time.Now, Checks: catalog.Checks,
		Sudoers: selfupdate.DefaultSudoers, Token: selfupdate.DefaultPATFile,
		CIRoot: preempt.DefaultCIRoot, PreemptState: preempt.DefaultState}
}

func Main(ctx context.Context, args []string, env Env, stdio IO) int {
	return Run(ctx, args, env, stdio, OSDeps())
}

func Run(ctx context.Context, args []string, env Env, stdio IO, deps Deps) int {
	stdout, stderr := console.New(stdio.Stdout), console.New(stdio.Stderr)
	var h handler
	ok := len(args) > 0
	if ok {
		h, ok = handlers[args[0]]
	}
	if !ok {
		stderr.Println(usage)
		return ExitUsage
	}
	cfg, err := LoadConfig(env)
	if err != nil {
		stderr.Println(err.Error())
		return ExitFail
	}
	counts := &obs.LevelCounts{}
	log, closeLog := openLogger(cfg, args[0], h, stdio.Stderr, counts)
	defer closeLog()
	a := &App{Config: cfg, IO: stdio, Out: stdout, Err: stderr, Now: deps.Now, Log: log, Owner: deps.Owner, Group: deps.Group, Exec: deps.Exec, Checks: deps.Checks, Sudoers: deps.Sudoers, Token: deps.Token, CIRoot: deps.CIRoot, PreemptState: deps.PreemptState,
		source: textfileSource(args[0], cfg.SSHCommand)}
	started := time.Now()
	log.Info("command started", "args", len(args)-1)
	code, panicked := a.invoke(ctx, h, args[1:])
	if werr := errors.Join(stdout.Err(), stderr.Err()); werr != nil {
		log.Error("output write failed", "error", werr.Error())
		code = ExitFail
	}
	run := obs.CommandRun{Command: args[0], ExitCode: code, Panicked: panicked, Seconds: time.Since(started).Seconds(), Logs: counts}
	log.Info("command finished", "exit_code", code, "result", run.Result(), "duration_seconds", run.Seconds, "warnings", counts.Warnings(), "errors", counts.Errors())
	if h.onServer {
		a.metrics().Push(context.WithoutCancel(ctx), run.Samples())
	}
	return code
}

func (a *App) invoke(ctx context.Context, h handler, args []string) (code int, panicked bool) {
	defer func() {
		if p := recover(); p != nil {
			a.Log.Error("command panicked", "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			code, panicked = ExitFail, true
		}
	}()
	return h.run(ctx, a, a.Exec(h.binaries...), args), false
}

func openLogger(cfg Config, command string, h handler, stderr io.Writer, counts *obs.LevelCounts) (*slog.Logger, func()) {
	level := obs.ParseLevel(cfg.LogLevel)
	path := cfg.LogFile
	if path == "" && h.onServer {
		path = cfg.Paths.LogFile(command)
	}
	if path == "" {
		if cfg.LogLevel == "" && !h.verbose {
			level = slog.LevelWarn
		}
		return obs.NewLogger(stderr, level, command, counts), func() {}
	}
	f, err := obs.OpenLogFile(path)
	if err != nil {
		l := obs.NewLogger(stderr, slog.LevelWarn, command, counts)
		l.Warn("log file unavailable, logging to stderr", "path", path, "error", err.Error())
		return l, func() {}
	}
	return obs.NewLogger(f, level, command, counts), func() { _ = f.Close() }
}

func (a *App) metrics() obs.Pusher {
	var pushers obs.Fanout
	if a.Config.MetricsURL != "" {
		pushers = append(pushers, obs.HTTPPusher{URL: a.Config.MetricsURL, Client: &http.Client{}, Now: a.Now, Log: a.Log})
	}
	if a.Config.TextfileDir != "" {
		if a.textfile == nil {
			a.textfile = &obs.Textfile{Dir: a.Config.TextfileDir, Source: a.source, Now: a.Now, Log: a.Log}
		}
		pushers = append(pushers, a.textfile)
	}
	switch len(pushers) {
	case 0:
		return obs.Discard{}
	case 1:
		return pushers[0]
	}
	return pushers
}

func textfileSource(command, request string) string {
	if command != CmdRunner {
		return command
	}
	verb, _, _ := strings.Cut(strings.TrimSpace(request), " ")
	if !sourceWord.MatchString(verb) {
		verb = otherSource
	}
	return command + "-" + verb
}

func (a *App) delivery() delivery.Notifier {
	return delivery.LogNotifier{Log: a.Log}
}

func (a *App) secrets(ex procexec.Runner) runner.SecretLayer {
	c := a.Config.Secrets
	if c.File == "" {
		return nil
	}
	return secrets.Store{File: c.File, KeyFile: c.Key, Age: c.Age, Exec: ex}
}

func ChownTo(username string) func(path string) error {
	return func(path string) error {
		u, err := user.Lookup(username)
		if err != nil {
			return err
		}
		uid, err := strconv.Atoi(u.Uid)
		if err != nil {
			return err
		}
		gid, err := strconv.Atoi(u.Gid)
		if err != nil {
			return err
		}
		return os.Chown(path, uid, gid)
	}
}

func GroupTo(group string) func(path string) error {
	return func(path string) error {
		g, err := user.LookupGroup(group)
		if err != nil {
			return err
		}
		gid, err := strconv.Atoi(g.Gid)
		if err != nil {
			return err
		}
		return os.Chown(path, 0, gid)
	}
}
