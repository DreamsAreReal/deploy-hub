package runner

import (
	"context"
	"os"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/caddy"
	"github.com/DreamsAreReal/deploy-hub/internal/compose"
	"github.com/DreamsAreReal/deploy-hub/internal/docker"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	standbySuffix      = "-bg"
	dockerMain         = docker.MainService
	targetMain         = "main"
	targetStandby      = "standby"
	standbyStopTimeout = 2 * time.Minute
)

func (s *Service) blueGreen(ctx context.Context, op *operation, conf registry.AppConf, ref string, t health.Target) (bool, bool) {
	old := s.container(ctx, op.app, op.dir, conf)
	if old == "" || !s.Docker.Running(ctx, old) {
		s.Log.Info("in-place recreate", "app", op.app, "reason", "no live instance")
		return s.recreate(ctx, op, conf, ref, t), false
	}
	if !s.overlapAllowed(ctx, op.dir, conf, ref) {
		s.Out.Println(msgOverlapOff)
		s.Log.Info("in-place recreate", "app", op.app, "reason", "overlap disabled by label")
		return s.recreate(ctx, op, conf, ref, t), false
	}
	if !s.standbySafe(ctx, conf, old) {
		s.Out.Printf(msgStatefulRecreate, old)
		s.Log.Info("in-place recreate", "app", op.app, "reason", "writable mount")
		return s.recreate(ctx, op, conf, ref, t), false
	}
	if t.Port == "" {
		return s.overlap(ctx, op, conf, ref, t)
	}
	return s.swap(ctx, op, conf, ref, t, old)
}

func (s *Service) overlapAllowed(ctx context.Context, dir string, conf registry.AppConf, ref string) bool {
	if data, err := os.ReadFile(composePath(dir)); err == nil {
		if v, ok := compose.OverlapLabel(data, mainService(conf)); ok {
			return compose.AllowsOverlap(v)
		}
	}
	return compose.AllowsOverlap(s.Docker.ImageLabel(ctx, ref, compose.LabelOverlap))
}

func (s *Service) standbySafe(ctx context.Context, conf registry.AppConf, old string) bool {
	switch conf.BlueGreen {
	case registry.BlueGreenOn:
		return true
	case registry.BlueGreenOff:
		return false
	}
	mounts, err := s.Docker.WritableMounts(ctx, old)
	if err != nil {
		s.Log.Warn("mounts unknown, treating the app as stateful", "container", old, "error", err.Error())
		return false
	}
	return len(mounts) == 0
}

func mainService(conf registry.AppConf) string {
	if conf.IsBYOC() {
		return conf.Main
	}
	return dockerMain
}

func (s *Service) recreate(ctx context.Context, op *operation, conf registry.AppConf, ref string, t health.Target) bool {
	s.composeUp(ctx, op.dir, conf, ref)
	return s.gateMain(ctx, op, conf, t)
}

func (s *Service) overlap(ctx context.Context, op *operation, conf registry.AppConf, ref string, t health.Target) (bool, bool) {
	standby := s.standbySpec(op, conf, ref)
	s.removeLeftover(ctx, standby.Name)
	s.Out.Printf(msgOverlapStart, standby.Name)
	return s.handover(ctx, op, conf, ref, t, standby, 0)
}

func (s *Service) swap(ctx context.Context, op *operation, conf registry.AppConf, ref string, t health.Target, old string) (bool, bool) {
	standby := s.standbySpec(op, conf, ref)
	apps, err := s.Apps.All()
	if err != nil {
		s.Log.Warn("blue-green skipped", "app", op.app, "reason", "apps.list unreadable", "error", err.Error())
		return s.recreate(ctx, op, conf, ref, t), false
	}
	s.removeLeftover(ctx, standby.Name)
	if standby.HostPort = caddy.StandbyPort(apps, op.app, s.Listening); standby.HostPort == "" {
		s.Err.Println(msgNoStandbyPort)
		s.Log.Warn("no free standby port", "app", op.app, "port", standby.HostPort)
		return s.recreate(ctx, op, conf, ref, t), false
	}
	if standby.Port = s.Docker.ContainerPort(ctx, old, t.Port); standby.Port == "" {
		standby.Port = t.Port
	}
	s.Router.Sync(ctx)
	s.Out.Printf(msgStandbyStart, standby.Name, standby.HostPort, standby.Port)
	return s.handover(ctx, op, conf, ref, t, standby, caddy.Settle)
}

func (s *Service) handover(ctx context.Context, op *operation, conf registry.AppConf, ref string, t health.Target, standby docker.Standby, settle time.Duration) (bool, bool) {
	cut := false
	defer func() { s.cleanupStandby(ctx, standby.Name, cut, settle) }()
	if err := s.Docker.RunStandby(ctx, standby); err != nil {
		s.Err.Println(msgStandbyFallback)
		s.Log.Warn("standby did not start", "app", op.app, "error", err.Error())
		return s.recreate(ctx, op, conf, ref, t), false
	}
	if !s.gateStandby(ctx, op.app, t, standby) {
		s.Err.Println(msgStandbyUnhealthy)
		return false, true
	}
	s.Out.Println(msgCutover)
	cut = true
	if s.recreate(ctx, op, conf, ref, t) {
		return true, false
	}
	s.Err.Println(msgRestorePrevious)
	if prev := registry.LoadState(op.dir).Current; prev != "" {
		s.composeUp(ctx, op.dir, conf, conf.Image+":"+prev)
	}
	return false, true
}

func (s *Service) standbySpec(op *operation, conf registry.AppConf, ref string) docker.Standby {
	return docker.Standby{Name: op.app + standbySuffix, File: composePath(op.dir), Service: mainService(conf), Image: ref}
}

func (s *Service) removeLeftover(ctx context.Context, name string) {
	if err := s.Docker.Remove(ctx, name); err != nil {
		s.Log.Debug("no leftover standby removed", "standby", name, "error", err.Error())
	}
}

func (s *Service) gateStandby(ctx context.Context, app string, t health.Target, standby docker.Standby) bool {
	t.Port, t.Container = standby.HostPort, standby.Name
	started := s.Now()
	ok := s.Gate.Wait(ctx, t)
	s.samples = append(s.samples, obs.Sample{
		Name:   obs.MetricHealthGateDuration,
		Labels: obs.Labels{obs.LabelApp: app, obs.LabelTarget: targetStandby, obs.LabelResult: resultOf(ok)},
		Value:  s.Now().Sub(started).Seconds(),
	})
	s.Log.Info("health gate finished", "app", app, "target", targetStandby, "mode", t.Mode, "healthy", ok)
	return ok
}

func (s *Service) cleanupStandby(ctx context.Context, name string, cut bool, settle time.Duration) {
	ctx = context.WithoutCancel(ctx)
	if cut {
		s.drainStandby(ctx, name, settle)
	}
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	if err := s.Docker.Remove(ctx, name); err != nil {
		s.Log.Error("standby not removed, it keeps its memory", "standby", name, "error", err.Error())
		return
	}
	s.Log.Info("standby cleaned up", "standby", name)
}

func (s *Service) drainStandby(ctx context.Context, name string, settle time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, settle+standbyStopTimeout)
	defer cancel()
	if settle > 0 {
		if err := s.Sleep(ctx, settle); err != nil {
			s.Log.Warn("standby drain cut short", "standby", name, "error", err.Error())
		}
	}
	if err := s.Docker.Stop(ctx, name); err != nil {
		s.Log.Warn("standby not stopped gracefully", "standby", name, "error", err.Error())
	}
}
