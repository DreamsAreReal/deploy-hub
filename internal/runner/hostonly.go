package runner

import (
	"context"
	"slices"

	"github.com/DreamsAreReal/deploy-hub/internal/docker"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

func (s *Service) hostOnly(app string) bool {
	return slices.Contains(s.HostOnly, app)
}

func (s *Service) installHost(ctx context.Context, op *operation) bool {
	if !slices.Contains(s.HostApps, op.app) || s.Host == nil {
		s.Err.Printf(msgNoHostSpec, op.app)
		s.Log.Error("host-only app without a host spec", "app", op.app)
		return false
	}
	return s.syncHost(ctx, op)
}

func (s *Service) deployHost(ctx context.Context, op *operation, conf registry.AppConf, prev string) int {
	s.Out.Printf(msgStarting, op.app, op.tag)
	if !s.installHost(ctx, op) {
		s.record(journal.Entry{App: op.app, Sha7: op.sha7, Action: journal.ActionDeploy, Result: journal.ResultFail, Seconds: s.since(op.t0)})
		s.Log.Error("deploy failed host sync", "app", op.app, "sha", op.sha7, "previous", prev)
		return ExitFail
	}
	return s.deploySucceeded(ctx, op, conf, prev, msgHostFilesOK)
}

func hostStatus(st appStatus, entries []journal.Entry) appStatus {
	st.state, st.health = docker.StateMissing, docker.NoHealth
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Action != journal.ActionHostSync {
			continue
		}
		st.state, st.health = stateNotInstalled, stateUnhealthy
		if entries[i].Result == journal.ResultOK {
			st.state, st.health = stateInstalled, health.StatusHealthy
		}
		return st
	}
	return st
}
