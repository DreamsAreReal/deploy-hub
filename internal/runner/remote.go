package runner

import (
	"context"
	"slices"
)

type RemoteSyncer interface {
	Run(ctx context.Context, app, tag string) error
}

func (s *Service) syncRemote(ctx context.Context, op *operation) {
	if s.Remote == nil || !slices.Contains(s.RemoteApps, op.app) {
		return
	}
	if err := s.Remote.Run(ctx, op.app, op.tag); err != nil {
		s.Err.Printf(msgRemoteSyncFail, err.Error())
		s.Log.Warn("remote sync failed, the app itself is deployed", "app", op.app, "tag", op.tag, "error", err.Error())
		return
	}
	s.Log.Info("remote sync done", "app", op.app, "tag", op.tag)
}
