package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/selfupdate"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

type allowlistProbe struct {
	os      procexec.Runner
	reply   procexectest.Script
	refused []string
}

func (p *allowlistProbe) Run(_ context.Context, c procexec.Command) (procexec.Result, error) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.os.Run(canceled, c)
	if errors.Is(err, procexec.ErrNotAllowed) || errors.Is(err, procexec.ErrArgsNotAllowed) {
		p.refused = append(p.refused, err.Error())
		return procexec.Result{}, err
	}
	return p.reply.Handle(c)
}

func TestRunnerRestartsPassTheAllowlistOfTheCommandsThatRunThem(t *testing.T) {
	const repo = "deploy-hub"
	for _, cmd := range []string{CmdRunnerSync, CmdSelfUpdate} {
		t.Run(cmd, func(t *testing.T) {
			root := t.TempDir()
			probe := &allowlistProbe{os: procexec.NewOS(handlers[cmd].binaries...), reply: procexectest.Script{
				procexec.CIRunnerSystemctl + " show":        {Res: procexec.Result{Stdout: "/user.slice/ci-runner@" + repo + ".service\n"}},
				procexec.CIRunnerSystemctl + " try-restart": {},
			}}
			var log bytes.Buffer
			r := selfupdate.Runners{
				Pending: filepath.Join(root, "pending"), Wants: filepath.Join(root, "wants"),
				Cgroups: filepath.Join(root, "cgroup"), Proc: filepath.Join(root, "proc"),
				Exec: probe, Log: slog.New(slog.NewTextHandler(&log, nil)),
			}
			testutil.Write(t, filepath.Join(r.Wants, "ci-runner@"+repo+".service"), "")
			testutil.NoErr(t, r.MarkAll())
			r.RestartIdle(context.Background())
			testutil.Equal(t, strings.Join(probe.refused, "\n"), "")
			testutil.Contains(t, log.String(), "ci runner restarted")
			_, err := os.Stat(filepath.Join(r.Pending, repo))
			testutil.True(t, errors.Is(err, os.ErrNotExist), "the restart is cleared")
		})
	}
}
