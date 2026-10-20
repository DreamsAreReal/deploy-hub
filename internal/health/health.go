package health

import (
	"context"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/netprobe"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	Timeout       = 90 * time.Second
	Interval      = 5 * time.Second
	ProbeTimeout  = 5 * time.Second
	StatusHealthy = "healthy"
	StatusRunning = "running"
)

type Target struct {
	Mode      string
	Port      string
	Path      string
	Container string
}

type Inspector interface {
	HealthStatus(ctx context.Context, container string) string
	State(ctx context.Context, container string) string
}

type HTTPChecker interface {
	HTTPOK(ctx context.Context, url string, timeout time.Duration) bool
}

type Gate struct {
	Docker   Inspector
	HTTP     HTTPChecker
	Timeout  time.Duration
	Interval time.Duration
	Sleep    func(ctx context.Context, d time.Duration) error
}

func (g Gate) Wait(ctx context.Context, t Target) bool {
	for elapsed := time.Duration(0); elapsed < g.Timeout; elapsed += g.Interval {
		if g.probe(ctx, t, elapsed) {
			return true
		}
		if err := g.Sleep(ctx, g.Interval); err != nil {
			return false
		}
	}
	return false
}

func (g Gate) probe(ctx context.Context, t Target, elapsed time.Duration) bool {
	switch t.Mode {
	case registry.HealthHTTP:
		return g.HTTP.HTTPOK(ctx, netprobe.HTTPURL(t.Port, t.Path), ProbeTimeout)
	case registry.HealthCompose:
		return g.Docker.HealthStatus(ctx, t.Container) == StatusHealthy
	case registry.HealthProcess:
		return g.Docker.State(ctx, t.Container) == StatusRunning && elapsed >= g.Interval
	default:
		return false
	}
}

func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
