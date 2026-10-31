package runner

import (
	"context"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

func (s *Service) rollback(ctx context.Context, raw string, entry registry.Entry, tag string) int {
	action := journal.ActionRollback
	if tag != "" {
		action = journal.ActionRedeploy
	}
	conf, err := s.loadConf(entry.Dir, entry.Name)
	if err != nil {
		return ExitFail
	}
	if tag == "" {
		tag = registry.LoadState(entry.Dir).Previous
	}
	if tag == "" {
		s.Err.Printf("%s %s\n", msgNoPrevious, entry.Name)
		s.Log.Warn("rollback without target", "app", entry.Name)
		return ExitFail
	}
	if !s.previouslyDeployed(entry, tag) {
		s.Log.Warn("rollback target never passed a deploy", "app", entry.Name, "tag", tag)
		return s.refuse(raw, msgNotGated)
	}
	op := s.newOperation(entry, tag, "")
	s.Log.Info("rollback started", "app", op.app, "action", action, "tag", tag)
	release, err := s.acquire(ctx, op.app)
	if err != nil {
		return ExitFail
	}
	defer release()
	healthy := s.restore(ctx, op, conf)
	s.record(journal.Entry{App: op.app, Sha7: op.sha7, Action: action, Result: resultOf(healthy), Seconds: s.since(op.t0)})
	if !healthy {
		s.Err.Printf(msgTargetUnhealthy, action)
		s.Log.Error("rollback target unhealthy", "app", op.app, "action", action, "tag", tag)
		return ExitFail
	}
	s.advanceState(op)
	s.reportRollback(action, op.sha7)
	if !s.hostOnly(op.app) && !s.syncHost(ctx, op) {
		return ExitFail
	}
	s.syncRemote(ctx, op)
	return ExitOK
}

func (s *Service) restore(ctx context.Context, op *operation, conf registry.AppConf) bool {
	if s.hostOnly(op.app) {
		return s.installHost(ctx, op)
	}
	ref := conf.Image + ":" + op.tag
	s.upSidecars(ctx, op.dir, conf, ref)
	s.Out.Println(msgWaitingGate)
	healthy, _ := s.blueGreen(ctx, op, conf, ref, targetFor(conf))
	return healthy
}

func (s *Service) advanceState(op *operation) {
	cur := registry.LoadState(op.dir).Current
	if cur == op.tag {
		return
	}
	if err := (registry.DeployState{Current: op.tag, Previous: cur}).Save(op.dir); err != nil {
		s.Log.Error("deploy state not saved", "app", op.app, "error", err.Error())
	}
}

func (s *Service) reportRollback(action, sha7 string) {
	msg := msgRolledBack
	if action == journal.ActionRedeploy {
		msg = msgRedeployed
	}
	s.Out.Printf("%s (%s)\n", msg, sha7)
}
