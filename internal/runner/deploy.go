package runner

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/hub"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const stagedComposeSuffix = ".new"

func (s *Service) deploy(ctx context.Context, raw string, entry registry.Entry, tag string, stdin io.Reader) int {
	conf, err := s.loadConf(entry.Dir, entry.Name)
	if err != nil {
		return ExitFail
	}
	meta, err := ReadMeta(stdin)
	if err == nil {
		err = meta.ValidateDeploy()
	}
	if err != nil {
		s.Err.Println(err.Error())
		s.Log.Warn("deploy metadata rejected", "app", entry.Name, "error", err.Error())
		return ExitFail
	}
	if aerr := s.authorize(ctx, VerbDeploy, entry, conf, tag, meta.OIDC); aerr != nil {
		return s.refuse(raw, refusalMessage(aerr))
	}
	op := s.newOperation(entry, tag, meta.Start)
	s.Log.Info("deploy started", "app", op.app, "tag", tag, "byoc", meta.BYOC(), "actor", meta.Actor, "subject", meta.Subject)
	release, err := s.acquire(ctx, op.app)
	if err != nil {
		return ExitFail
	}
	defer release()
	return s.deployLocked(ctx, op, conf, meta)
}

func (s *Service) newOperation(entry registry.Entry, tag, start string) *operation {
	now := s.Now()
	op := &operation{app: entry.Name, dir: entry.Dir, tag: tag, sha7: registry.Sha7(tag), t0: now, start: now}
	if epoch, err := strconv.ParseInt(start, 10, 64); err == nil {
		op.start = time.Unix(epoch, 0)
	}
	return op
}

func (s *Service) deployLocked(ctx context.Context, op *operation, conf registry.AppConf, meta Meta) int {
	if meta.BYOC() {
		installed, err := s.installCompose(ctx, op, conf, meta)
		if err != nil {
			s.Err.Println(err.Error())
			return ExitFail
		}
		conf = installed
	}
	ref := conf.Image + ":" + op.tag
	if !s.pull(ctx, op, conf.Image, meta) {
		return ExitFail
	}
	prev := registry.LoadState(op.dir).Current
	s.Out.Printf(msgStarting, op.app, op.tag)
	s.upSidecars(ctx, op.dir, conf, ref)
	t := targetFor(conf)
	healthy, oldAlive := s.blueGreen(ctx, op, conf, ref, t)
	if healthy && !s.syncHost(ctx, op) {
		return s.hostFailed(ctx, op, conf, prev, t)
	}
	if healthy {
		return s.deploySucceeded(ctx, op, conf, prev)
	}
	return s.deployFailed(ctx, op, conf, prev, oldAlive, t)
}

func (s *Service) installCompose(ctx context.Context, op *operation, conf registry.AppConf, meta Meta) (registry.AppConf, error) {
	data, err := base64.StdEncoding.DecodeString(meta.Compose)
	if err != nil {
		return conf, errBadCompose
	}
	staged := composePath(op.dir) + stagedComposeSuffix
	if err := os.WriteFile(staged, data, fileMode); err != nil {
		return conf, err
	}
	if err := ensureFile(filepath.Join(op.dir, registry.EnvFile)); err != nil {
		_ = os.Remove(staged)
		return conf, err
	}
	if err := s.Docker.ComposeValidate(ctx, op.dir, filepath.Base(staged), conf.Image+":"+op.tag); err != nil {
		_ = os.Remove(staged)
		s.Log.Warn("rendered compose rejected", "app", op.app, "error", err.Error())
		return conf, errComposeInvalid
	}
	if err := os.Rename(staged, composePath(op.dir)); err != nil {
		return conf, err
	}
	next := registry.AppConf{
		Mode: registry.ModeBYOC, Main: meta.Main, Port: meta.HPort,
		HealthPath: registry.DefaultHealthPath, HMode: meta.HMode, Image: conf.Image, Repo: conf.Repo,
		BlueGreen: conf.BlueGreen,
	}
	s.Log.Info("byoc compose installed", "app", op.app, "main", meta.Main, "port", meta.HPort, "hmode", meta.HMode)
	return next, next.Save(op.dir)
}

func (s *Service) pull(ctx context.Context, op *operation, image string, meta Meta) bool {
	ref, pinned := image+":"+op.tag, image+"@"+meta.Digest
	if meta.Token != "" {
		actor := meta.Actor
		if actor == "" {
			actor = defaultActor
		}
		if err := s.Docker.Login(ctx, hub.RegistryHost, actor, meta.Token); err != nil {
			s.Err.Println(msgLoginFail)
			s.Log.Error("registry login failed", "app", op.app, "registry", hub.RegistryHost)
			s.record(journal.Entry{App: op.app, Sha7: op.sha7, Action: journal.ActionDeploy, Result: journal.ResultFail, Seconds: s.since(op.t0)})
			return false
		}
		defer s.logout(context.WithoutCancel(ctx), op.app)
	}
	s.Out.Printf(msgPulling, pinned)
	started := s.Now()
	out, err := s.Docker.Pull(ctx, pinned)
	if err == nil {
		err = s.Docker.Tag(ctx, pinned, ref)
	}
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricImagePullDuration, Labels: obs.Labels{obs.LabelApp: op.app, obs.LabelResult: resultOf(err == nil)}, Value: s.Now().Sub(started).Seconds()})
	if err != nil {
		s.appendLastError(op.app, out)
		s.Log.Error("image pull failed", "app", op.app, "image", ref, "output", out)
		s.record(journal.Entry{App: op.app, Sha7: op.sha7, Action: journal.ActionDeploy, Result: journal.ResultFail, Seconds: s.since(op.t0)})
		s.Err.Printf("%s: %s\n", msgPullFail, out)
		return false
	}
	s.Log.Info("image pulled", "app", op.app, "image", ref, "digest", meta.Digest)
	return true
}

func (s *Service) deploySucceeded(ctx context.Context, op *operation, conf registry.AppConf, prev string) int {
	if prev != op.tag {
		if err := (registry.DeployState{Current: op.tag, Previous: prev}).Save(op.dir); err != nil {
			s.Log.Error("deploy state not saved", "app", op.app, "error", err.Error())
		}
	}
	s.record(journal.Entry{App: op.app, Sha7: op.sha7, Action: journal.ActionDeploy, Result: journal.ResultOK, Seconds: s.since(op.t0)})
	lead := s.Now().Sub(op.start)
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricLeadTime, Labels: obs.Labels{obs.LabelApp: op.app}, Value: lead.Seconds()})
	s.Out.Printf(msgLive, msgHealthOK, op.app, op.sha7, FormatDuration(lead))
	if conf.IsBYOC() {
		s.Router.Sync(ctx)
	}
	s.Log.Info("deploy succeeded", "app", op.app, "sha", op.sha7, "previous", prev, "lead_seconds", lead.Seconds())
	return ExitOK
}

func failureReason(t health.Target) string {
	switch t.Mode {
	case registry.HealthCompose:
		return reasonCompose
	case registry.HealthProcess:
		return reasonProcess
	default:
		return fmt.Sprintf(reasonHTTP, t.Port, t.Path)
	}
}

func (s *Service) deployFailed(ctx context.Context, op *operation, conf registry.AppConf, prev string, oldAlive bool, t health.Target) int {
	reason := failureReason(t)
	s.record(journal.Entry{App: op.app, Sha7: op.sha7, Action: journal.ActionDeploy, Result: journal.ResultFail, Seconds: s.since(op.t0)})
	s.Err.Printf(msgHealthFail, s.Gate.Timeout, reason)
	s.Log.Error("deploy failed health gate", "app", op.app, "sha", op.sha7, "reason", reason, "old_alive", oldAlive, "previous", prev)
	switch {
	case oldAlive:
		s.Err.Printf("%s (%s) — %s\n", msgRolledBack, registry.Sha7(prev), msgZeroDowntime)
	case prev != "":
		s.rollbackAfterFailure(ctx, op, conf, prev, t)
	default:
		s.stopFirstDeploy(ctx, op, conf)
	}
	return ExitFail
}

func (s *Service) hostFailed(ctx context.Context, op *operation, conf registry.AppConf, prev string, t health.Target) int {
	s.record(journal.Entry{App: op.app, Sha7: op.sha7, Action: journal.ActionDeploy, Result: journal.ResultFail, Seconds: s.since(op.t0)})
	s.Log.Error("deploy failed host sync", "app", op.app, "sha", op.sha7, "previous", prev)
	if prev == "" {
		s.stopFirstDeploy(ctx, op, conf)
		return ExitFail
	}
	s.rollbackAfterFailure(ctx, op, conf, prev, t)
	return ExitFail
}

func (s *Service) rollbackAfterFailure(ctx context.Context, op *operation, conf registry.AppConf, prev string, t health.Target) {
	started := s.Now()
	prev7 := registry.Sha7(prev)
	s.Err.Printf(msgRollingBack, prev)
	s.composeUp(ctx, op.dir, conf, conf.Image+":"+prev)
	ok := s.gateMain(ctx, op, conf, t)
	s.record(journal.Entry{App: op.app, Sha7: prev7, Action: journal.ActionRollback, Result: resultOf(ok), Seconds: s.since(started)})
	if ok {
		s.Err.Printf("%s (%s)\n", msgRolledBack, prev7)
		return
	}
	s.Err.Println(msgRollbackFailed)
}

func (s *Service) stopFirstDeploy(ctx context.Context, op *operation, conf registry.AppConf) {
	err := s.Docker.ComposeStop(ctx, composePath(op.dir), conf.Image+":"+op.tag, dockerMain)
	s.record(journal.Entry{App: op.app, Sha7: op.sha7, Action: journal.ActionStop, Result: resultOf(err == nil)})
	s.Err.Println(msgFirstFail)
}

func (s *Service) logout(ctx context.Context, app string) {
	if err := s.Docker.Logout(ctx, hub.RegistryHost); err != nil {
		s.Log.Error("registry logout failed, credentials may remain in the docker config", "app", app, "registry", hub.RegistryHost, "error", err.Error())
	}
}
