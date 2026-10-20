package procexec

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	shell     = "sh"
	shellFlag = "-c"
	sleeper   = "sleep"
	missing   = "nope"
)

func testOS() OS {
	return OS{allowed: map[string]builder{
		shell:   func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "sh", a...) },
		"sleep": func(ctx context.Context, a []string) *exec.Cmd { return exec.CommandContext(ctx, "sleep", a...) },
		"nope": func(ctx context.Context, a []string) *exec.Cmd {
			return exec.CommandContext(ctx, "deploy-hub-missing-binary", a...)
		},
	}}
}

func TestRunRejectsBinaryOutsideAllowlist(t *testing.T) {
	_, err := NewOS(shell, "docker").Run(context.Background(), Command{Name: shell, Args: []string{shellFlag, "id"}})
	testutil.Is(t, err, ErrNotAllowed)
	_, err = NewOS("docker").Run(context.Background(), Command{Name: "rm", Args: []string{"-rf", "/"}})
	testutil.Is(t, err, ErrNotAllowed)
	testutil.Equal(t, len(NewOS("docker", "git", shell).allowed), 2)
}

func TestRunMissingBinary(t *testing.T) {
	_, err := testOS().Run(context.Background(), Command{Name: missing})
	testutil.Fails(t, err)
}

func TestRunPassesStdinEnvDirAndCapturesOutput(t *testing.T) {
	dir := t.TempDir()
	res, err := testOS().Run(context.Background(), Command{
		Name:  shell,
		Args:  []string{shellFlag, `read x; echo "$x $DH_TEST $(pwd)"; echo err >&2`},
		Env:   []string{"DH_TEST=value"},
		Stdin: "hello\n",
		Dir:   dir,
	})
	testutil.NoErr(t, err)
	testutil.True(t, strings.HasPrefix(res.Stdout, "hello value "), res.Stdout)
	testutil.Contains(t, res.Stdout, dir[strings.LastIndex(dir, "/"):])
	testutil.Equal(t, res.Stderr, "err\n")
	testutil.Equal(t, res.Combined(), strings.TrimSpace(res.Stdout+res.Stderr))
}

func TestRunReportsExitErrorWithOutput(t *testing.T) {
	res, err := testOS().Run(context.Background(), Command{Name: shell, Args: []string{shellFlag, "echo out; echo bad >&2; exit 3"}})
	testutil.Fails(t, err)
	testutil.Equal(t, Output(err), "out\nbad")
	testutil.Equal(t, res.Stdout, "out\n")
	testutil.Equal(t, ExitCode(err), 3)
	testutil.Contains(t, err.Error(), "exit status 3")
	testutil.Equal(t, Output(errors.New("plain")), "")
	testutil.Equal(t, ExitCode(errors.New("plain")), 0)
}

func TestRunStartFailure(t *testing.T) {
	_, err := testOS().Run(context.Background(), Command{Name: shell, Dir: "/definitely/missing/dir"})
	testutil.Fails(t, err)
}

func TestRunKillsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := testOS().Run(ctx, Command{Name: sleeper, Args: []string{"5"}})
	testutil.Is(t, err, context.DeadlineExceeded)
	testutil.True(t, time.Since(started) < 2*time.Second, "process was not killed promptly")
}
