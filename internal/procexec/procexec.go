package procexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Command struct {
	Name        string
	Args        []string
	Env         []string
	Stdin       string
	Dir         string
	MemoryLimit int64
}

type Result struct {
	Stdout string
	Stderr string
}

func (r Result) Combined() string {
	return strings.TrimSpace(r.Stdout + r.Stderr)
}

type Runner interface {
	Run(ctx context.Context, cmd Command) (Result, error)
}

type Error struct {
	Err    error
	Output string
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

func Output(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Output
	}
	return ""
}

func ExitCode(err error) int {
	if e, ok := errors.AsType[*exec.ExitError](err); ok {
		return e.ExitCode()
	}
	return 0
}

const waitDelay = 5 * time.Second

var (
	ErrNotAllowed     = errors.New("binary is not in the allowlist")
	ErrArgsNotAllowed = errors.New("arguments are not in the allowlist")
)

const CIRunnerSystemctl = "systemctl-ci-runner"

var argRules = map[string]*regexp.Regexp{
	CIRunnerSystemctl: regexp.MustCompile(`^(show -p ControlGroup --value ci-runner@[a-z0-9][a-z0-9_-]{0,28}\.service|try-restart ci-docker@[a-z0-9][a-z0-9_-]{0,28}\.service ci-runner@[a-z0-9][a-z0-9_-]{0,28}\.service)$`),
}

type builder func(ctx context.Context, args []string) *exec.Cmd

var known = map[string]builder{
	"actionlint": func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "actionlint", a...) },
	"caddy":      func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "caddy", a...) },
	"deadcode":   func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "deadcode", a...) },
	"docker":     func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "docker", a...) },
	"gh":         func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "gh", a...) },
	"git":        func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "git", a...) },
	"gitleaks":   func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "gitleaks", a...) },
	"go":         func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "go", a...) },
	"gobco":      func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "gobco", a...) },
	"gocognit":   func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "gocognit", a...) },
	"golangci-lint": func(ctx context.Context, a []string) *exec.Cmd {
		return exec.CommandContext(ctx, "golangci-lint", a...)
	},
	"govulncheck": func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "govulncheck", a...) },
	"hadolint":    func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "hadolint", a...) },
	"osv-scanner": func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "osv-scanner", a...) },
	"ssh":         func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "ssh", a...) },
	"ssh-keygen":  func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "ssh-keygen", a...) },
	"sudo":        func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "sudo", a...) },
	"syft":        func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "syft", a...) },
	"tar":         func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "tar", a...) },
	"systemctl":   func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "systemctl", a...) },
	"visudo":      func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "visudo", a...) },
	"deploy-hub-installed": func(ctx context.Context, a []string) *exec.Cmd {
		return exec.CommandContext(ctx, "/opt/deploy-hub/bin/deploy-hub", a...)
	},
	"deploy-hub-candidate": func(ctx context.Context, a []string) *exec.Cmd {
		return exec.CommandContext(ctx, "/opt/deploy-hub/bin/deploy-hub.new", a...)
	},
	"ci-runner-setup": func(ctx context.Context, a []string) *exec.Cmd {
		return exec.CommandContext(ctx, "/opt/deploy-hub/ci-runner/ci-runner-setup", a...)
	},
	CIRunnerSystemctl: func(ctx context.Context, a []string) *exec.Cmd {
		return exec.CommandContext(ctx, "systemctl", a...)
	},
	"systemd-analyze": func(ctx context.Context, a []string) *exec.Cmd {
		return exec.CommandContext(ctx, "systemd-analyze", a...)
	},
	"trivy":      func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "trivy", a...) },
	"trufflehog": func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "trufflehog", a...) },
	"ruff":       func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "ruff", a...) },
	"node":       func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "node", a...) },
	"dotnet":     func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "dotnet", a...) },
	"shellcheck": func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "shellcheck", a...) },
	"shfmt":      func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "shfmt", a...) },
	"bats":       func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "bats", a...) },
	"cmake":      func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "cmake", a...) },
	"cc":         func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "cc", a...) },
	"make":       func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "make", a...) },
	"jsonpath-lemon": func(ctx context.Context, a []string) *exec.Cmd {
		return exec.CommandContext(ctx, "./contrib/lemon", a...)
	},
	"bash":   func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "bash", a...) },
	"npm":    func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "npm", a...) },
	"uv":     func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "uv", a...) },
	"zizmor": func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "zizmor", a...) },
}

type OS struct {
	allowed map[string]builder
}

func NewOS(names ...string) OS {
	allowed := make(map[string]builder, len(names))
	for _, n := range names {
		if b, ok := known[n]; ok {
			allowed[n] = b
		}
	}
	return OS{allowed: allowed}
}

func (o OS) Run(ctx context.Context, c Command) (Result, error) {
	build, ok := o.allowed[c.Name]
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrNotAllowed, c.Name)
	}
	if rule, ok := argRules[c.Name]; ok && !rule.MatchString(strings.Join(c.Args, " ")) {
		return Result{}, fmt.Errorf("%w: %s %s", ErrArgsNotAllowed, c.Name, strings.Join(c.Args, " "))
	}
	var stdout, stderr bytes.Buffer
	cmd := build(ctx, c.Args)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Dir = c.Dir
	cmd.Stdin = strings.NewReader(c.Stdin)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = waitDelay
	over, err := execute(cmd, c.MemoryLimit)
	if over {
		err = errors.Join(ErrMemoryLimit, err)
	}
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		return res, &Error{Err: errors.Join(ctx.Err(), err), Output: res.Combined()}
	}
	return res, nil
}

func execute(cmd *exec.Cmd, limit int64) (bool, error) {
	if limit <= 0 {
		return false, cmd.Run()
	}
	isolate(cmd)
	if err := cmd.Start(); err != nil {
		return false, err
	}
	var over atomic.Bool
	var watching sync.WaitGroup
	stop := make(chan struct{})
	watching.Go(func() { guard(cmd.Process.Pid, limit, &over, stop) })
	err := cmd.Wait()
	close(stop)
	watching.Wait()
	return over.Load(), err
}
