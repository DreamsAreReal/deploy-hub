package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"github.com/DreamsAreReal/deploy-hub/internal/caddy"
	"github.com/DreamsAreReal/deploy-hub/internal/docker"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	standbySuffix = "-bg"
	dockerMain    = docker.MainService
	targetMain    = "main"
	targetStandby = "standby"
)

func (s *Service) blueGreen(ctx context.Context, op *operation, conf registry.AppConf, ref string, t health.Target) (bool, bool) {
	old := s.container(ctx, op.app, op.dir, conf)
	if t.Port == "" || old == "" || !s.Docker.Running(ctx, old) {
		s.Log.Info("in-place recreate", "app", op.app, "reason", "no live instance or no port")
		return s.recreate(ctx, op, conf, ref, t), false
	}
	return s.swap(ctx, op, conf, ref, t, old)
}

func (s *Service) recreate(ctx context.Context, op *operation, conf registry.AppConf, ref string, t health.Target) bool {
	s.composeUp(ctx, op.dir, conf, ref)
	return s.gateMain(ctx, op, conf, t)
}

func (s *Service) swap(ctx context.Context, op *operation, conf registry.AppConf, ref string, t health.Target, old string) (bool, bool) {
	name := op.app + standbySuffix
	apps, err := s.Apps.All()
	if err != nil {
		return s.recreate(ctx, op, conf, ref, t), false
	}
	standby := s.standbySpec(ctx, op.dir, name, old, ref, t.Port)
	standby.HostPort = caddy.StandbyPort(apps, op.app)
	s.Docker.Remove(ctx, name)
	if hostPort, err := strconv.Atoi(standby.HostPort); err != nil || s.Listening(hostPort) {
		s.Err.Println(msgNoStandbyPort)
		s.Log.Warn("no free standby port", "app", op.app, "port", standby.HostPort)
		return s.recreate(ctx, op, conf, ref, t), false
	}
	drain := false
	defer func() { s.cleanupStandby(ctx, name, drain) }()
	s.Out.Printf(msgStandbyStart, name, standby.HostPort, standby.Port)
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
	drain = true
	if s.recreate(ctx, op, conf, ref, t) {
		return true, false
	}
	s.Err.Println(msgRestorePrevious)
	if prev := registry.LoadState(op.dir).Current; prev != "" {
		s.composeUp(ctx, op.dir, conf, conf.Image+":"+prev)
	}
	return false, true
}

func (s *Service) standbySpec(ctx context.Context, dir, name, old, ref, port string) docker.Standby {
	spec := docker.Standby{Name: name, Network: s.Docker.Network(ctx, old), Port: s.Docker.ContainerPort(ctx, old, port), Image: ref}
	if spec.Port == "" {
		spec.Port = port
	}
	env := filepath.Join(dir, registry.EnvFile)
	if _, err := os.Stat(env); err == nil {
		spec.EnvFile = env
	}
	return spec
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

// cleanupStandby removes the standby. Caddy lists the standby port next to
// the permanent one and fails over between them by itself (no reload); once
// the permanent service was recreated, the standby stays up for caddy.Settle
// so Caddy is back on the permanent port before the standby goes away.
func (s *Service) cleanupStandby(ctx context.Context, name string, drain bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if drain {
		if err := s.Sleep(ctx, caddy.Settle); err != nil {
			s.Log.Warn("standby drain cut short", "standby", name, "error", err.Error())
		}
	}
	s.Docker.Remove(ctx, name)
	s.Log.Info("standby cleaned up", "standby", name)
}
