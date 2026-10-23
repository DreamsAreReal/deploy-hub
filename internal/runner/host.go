package runner

import (
	"context"
	"fmt"
	"slices"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const HostSyncVerb = "host-sync"

type HostSyncer interface {
	Sync(ctx context.Context, app, tag string) error
}

type SudoHost struct {
	Exec   procexec.Runner
	Binary string
}

func (h SudoHost) Sync(ctx context.Context, app, tag string) error {
	_, err := h.Exec.Run(ctx, procexec.Command{Name: sudoBinary, Args: []string{sudoNonInteract, h.Binary, HostSyncVerb, app, tag}})
	if err != nil {
		return fmt.Errorf("%w: %s", err, procexec.Output(err))
	}
	return nil
}

func (s *Service) syncHost(ctx context.Context, op *operation) bool {
	if s.Host == nil || !slices.Contains(s.HostApps, op.app) {
		return true
	}
	if err := s.Host.Sync(ctx, op.app, op.tag); err != nil {
		s.Err.Printf(msgHostSyncFail, err.Error())
		s.Log.Error("host sync failed", "app", op.app, "tag", op.tag, "error", err.Error())
		return false
	}
	s.Log.Info("host sync done", "app", op.app, "tag", op.tag)
	return true
}
