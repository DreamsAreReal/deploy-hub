package runner

import (
	"context"
	"log/slog"
	"os"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	sudoBinary      = "sudo"
	sudoNonInteract = "-n"
	CaddySyncVerb   = "caddy-sync"
	execBits        = 0o111
)

type SudoRouter struct {
	Exec   procexec.Runner
	Binary string
	Log    *slog.Logger
}

func (r SudoRouter) Sync(ctx context.Context) {
	info, err := os.Stat(r.Binary)
	if err != nil || info.Mode().Perm()&execBits == 0 {
		r.Log.Debug("caddy-sync skipped", "reason", "binary not executable")
		return
	}
	_, err = r.Exec.Run(ctx, procexec.Command{Name: sudoBinary, Args: []string{sudoNonInteract, r.Binary, CaddySyncVerb}})
	if err != nil {
		r.Log.Warn("caddy-sync failed", "error", err.Error(), "output", procexec.Output(err))
		return
	}
	r.Log.Debug("caddy-sync done")
}
