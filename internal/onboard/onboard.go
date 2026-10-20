package onboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/console"

	"github.com/DreamsAreReal/deploy-hub/internal/compose"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/provision"
)

const (
	ExitOK            = 0
	ExitFail          = 1
	verbStatus        = "status"
	verbHistory       = "history"
	tempPattern       = "deploy-hub-onboard-*"
	gitHubPrefix      = "https://github.com/"
	gitSuffix         = ".git"
	imagePrefix       = "ghcr.io/"
	ghBinary          = "gh"
	gitBinary         = "git"
	sshBinary         = "ssh"
	SSHKeygen         = "ssh-keygen"
	knownHostsComment = "#"
	repoDir           = "repo"
)

type Config struct {
	HostAlias    string
	Addr         string
	DeployKey    string
	HubRepo      string
	Owner        string
	RemoteBinary string
	DeployUser   string
}

type Onboarder struct {
	Config Config
	Exec   procexec.Runner
	Out    *console.Printer
	Err    *console.Printer
	Log    *slog.Logger
}

type session struct {
	opts      Options
	work      string
	branch    string
	image     string
	compose   *compose.Summary
	server    provision.Facts
	stubWant  string
	stubState string
	secrets   map[string]bool
	envVars   []string
}

func (o Onboarder) Run(ctx context.Context, args []string) int {
	if len(args) > 0 && (args[0] == verbStatus || args[0] == verbHistory) {
		return o.proxy(ctx, args)
	}
	opts, err := ParseOptions(args, o.Config.Owner)
	if errors.Is(err, ErrHelp) {
		o.Out.Println(Usage)
		return ExitOK
	}
	if err != nil {
		return o.fail(err)
	}
	work, err := os.MkdirTemp("", tempPattern)
	if err != nil {
		return o.fail(err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	return o.onboard(ctx, &session{opts: opts, work: work})
}

func (o Onboarder) fail(err error) int {
	o.Err.Println(err.Error())
	o.Log.Error("onboard failed", "error", err.Error())
	return ExitFail
}

func (o Onboarder) onboard(ctx context.Context, s *session) int {
	profile := ""
	if s.opts.Profile != "" {
		profile = " (profile " + s.opts.Profile + ")"
	}
	o.Out.Printf(msgHeader, s.opts.Repo, s.opts.App, profile)
	o.Out.Println(msgGathering)
	if err := o.gather(ctx, s); err != nil {
		return o.fail(err)
	}
	plan := s.plan()
	o.Out.Println(msgPlan)
	for _, line := range plan {
		o.Out.Printf(msgPlanLine, line)
	}
	o.Log.Info("onboard plan", "repo", s.opts.Repo, "app", s.opts.App, "steps", len(plan), "dry_run", s.opts.DryRun)
	switch {
	case len(plan) == 0:
		o.Out.Println(msgNoChanges)
		return ExitOK
	case s.opts.DryRun:
		o.Out.Println(msgDry)
		return ExitOK
	}
	if err := o.apply(ctx, s); err != nil {
		return o.fail(err)
	}
	o.finish(s)
	return ExitOK
}

func (o Onboarder) run(ctx context.Context, name string, args ...string) (string, error) {
	res, err := o.Exec.Run(ctx, procexec.Command{Name: name, Args: args})
	if err != nil {
		o.Log.Warn("command failed", "binary", name, "error", err.Error())
	}
	return strings.TrimSpace(res.Stdout), err
}

func (o Onboarder) proxy(ctx context.Context, args []string) int {
	target := o.Config.DeployUser + "@" + o.Config.Addr
	res, err := o.Exec.Run(ctx, procexec.Command{Name: sshBinary, Args: []string{"-i", o.Config.DeployKey, "-o", "IdentitiesOnly=yes", target, strings.Join(args, " ")}})
	o.Out.Print(res.Stdout)
	o.Err.Print(res.Stderr)
	if err != nil {
		return ExitFail
	}
	return ExitOK
}

func (o Onboarder) remote(ctx context.Context, args ...string) (string, error) {
	full := append([]string{o.Config.HostAlias, o.Config.RemoteBinary}, args...)
	res, err := o.Exec.Run(ctx, procexec.Command{Name: sshBinary, Args: full})
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, procexec.Output(err))
	}
	return res.Stdout, nil
}
