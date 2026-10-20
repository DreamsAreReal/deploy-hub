package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/catalog"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/node"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/python"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/scan"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/onboard"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/tools"
)

const (
	ExitOK    = 0
	ExitFail  = 1
	ExitUsage = 2

	CmdRunner    = "runner"
	CmdRender    = "render"
	CmdCaddySync = "caddy-sync"
	CmdPrune     = "prune"
	CmdProvision = "provision"
	CmdFacts     = "facts"
	CmdOnboard   = "onboard"
	CmdGate      = "gate"
	CmdTools     = "tools"

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
  prune                            delete old app images (keep running + 2 previous)
  provision --app NAME ...         prepare /opt/<app> on the VPS (root)
  facts <app>                      print server facts for onboarding
  onboard <repo> [options]         connect a GitHub repo (run on the operator machine)
  gate --app NAME --repo O/N ...   run the deploy quality gate on a checkout (CI)
  tools --manifest FILE --dir DIR  install the pinned gate toolchain (CI)`
)

type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type App struct {
	Config Config
	IO     IO
	Out    *console.Printer
	Err    *console.Printer
	Exec   func(names ...string) procexec.Runner
	Now    func() time.Time
	Log    *slog.Logger
	Owner  func(username string) func(path string) error
	Checks func() []gate.Check
}

type handler struct {
	run       func(ctx context.Context, a *App, ex procexec.Runner, args []string) int
	binaries  []string
	logToFile bool
	verbose   bool
}

var handlers = map[string]handler{
	CmdRunner:    {run: runRunner, binaries: []string{binDocker, binSudo}, logToFile: true},
	CmdRender:    {run: runRender},
	CmdCaddySync: {run: runCaddySync, binaries: []string{binCaddy, binSystemctl}, logToFile: true},
	CmdPrune:     {run: runPrune, binaries: []string{binDocker}, logToFile: true},
	CmdProvision: {run: runProvision, binaries: []string{binCaddy, binSystemctl}, logToFile: true},
	CmdFacts:     {run: runFacts, logToFile: true},
	CmdOnboard:   {run: runOnboard, binaries: []string{binGH, binGit, binSSH, onboard.SSHKeygen}},
	CmdGate:      {run: runGate, binaries: gateBinaries, verbose: true},
	CmdTools:     {run: runTools, binaries: []string{tools.GoBinary}, verbose: true},
}

var gateBinaries = []string{
	golang.GoBinary, source.GitBinary, golang.LintBinary, golang.CognitBinary, golang.DeadcodeBinary, golang.GobcoBinary,
	golang.VulncheckBinary, scan.GitleaksBinary, scan.TrufflehogBinary, scan.DockerBinary, scan.OSVBinary, scan.ZizmorBinary,
	scan.ActionlintBinary, scan.HadolintBinary, scan.TrivyBinary, scan.SyftBinary, python.UVBinary, python.RuffBinary,
	node.NodeBinary, node.NPMBinary,
}

type Deps struct {
	Exec   func(names ...string) procexec.Runner
	Owner  func(username string) func(path string) error
	Now    func() time.Time
	Checks func() []gate.Check
}

func OSDeps() Deps {
	return Deps{Exec: func(names ...string) procexec.Runner { return procexec.NewOS(names...) }, Owner: ChownTo, Now: time.Now, Checks: catalog.Checks}
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
	log, closeLog := openLogger(cfg, args[0], h, stdio.Stderr)
	defer closeLog()
	a := &App{Config: cfg, IO: stdio, Out: stdout, Err: stderr, Now: deps.Now, Log: log, Owner: deps.Owner, Exec: deps.Exec, Checks: deps.Checks}
	started := time.Now()
	code := h.run(ctx, a, a.Exec(h.binaries...), args[1:])
	if werr := errors.Join(stdout.Err(), stderr.Err()); werr != nil {
		log.Error("output write failed", "error", werr.Error())
		code = ExitFail
	}
	log.Info("command finished", "exit_code", code, "duration_seconds", time.Since(started).Seconds())
	return code
}

func openLogger(cfg Config, command string, h handler, stderr io.Writer) (*slog.Logger, func()) {
	level := obs.ParseLevel(cfg.LogLevel)
	path := cfg.LogFile
	if path == "" && h.logToFile {
		path = cfg.Paths.LogFile(command)
	}
	if path == "" {
		if cfg.LogLevel == "" && !h.verbose {
			level = slog.LevelWarn
		}
		return obs.NewLogger(stderr, level, command), func() {}
	}
	f, err := obs.OpenLogFile(path)
	if err != nil {
		l := obs.NewLogger(stderr, slog.LevelWarn, command)
		l.Warn("log file unavailable, logging to stderr", "path", path, "error", err.Error())
		return l, func() {}
	}
	return obs.NewLogger(f, level, command), func() { _ = f.Close() }
}

func (a *App) metrics() obs.Pusher {
	if a.Config.MetricsURL == "" {
		return obs.Discard{}
	}
	return obs.HTTPPusher{URL: a.Config.MetricsURL, Client: &http.Client{}, Now: a.Now, Log: a.Log}
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
