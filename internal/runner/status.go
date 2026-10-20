package runner

import (
	"context"
	"fmt"

	"github.com/DreamsAreReal/deploy-hub/internal/docker"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/netprobe"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

type appStatus struct {
	name   string
	sha7   string
	state  string
	health string
	last   string
}

func (s appStatus) line() string {
	return fmt.Sprintf("%s | %s | %s/%s | %s", s.name, s.sha7, s.state, s.health, s.last)
}

func (s appStatus) up() bool {
	return s.state == health.StatusRunning
}

func (s appStatus) healthy() bool {
	return s.health == health.StatusHealthy || s.health == probeOK
}

func (s *Service) status(ctx context.Context) int {
	apps, err := s.Apps.All()
	if err != nil {
		s.Err.Println(msgRegistryError)
		s.Log.Error("apps.list unreadable", "error", err.Error())
		return ExitFail
	}
	entries, err := s.Journal.Entries()
	if err != nil {
		s.Log.Warn("journal read failed", "error", err.Error())
	}
	s.Out.Println(statusHeader)
	for _, a := range apps {
		st := s.appStatus(ctx, a, journal.ForApp(entries, a.Name))
		s.Out.Println(st.line())
		labels := obs.Labels{obs.LabelApp: a.Name}
		s.samples = append(s.samples,
			obs.Sample{Name: obs.MetricAppUp, Labels: labels, Value: obs.Bool(st.up())},
			obs.Sample{Name: obs.MetricAppHealthy, Labels: labels, Value: obs.Bool(st.healthy())},
		)
	}
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricAppsRegistered, Labels: obs.Labels{}, Value: float64(len(apps))})
	return ExitOK
}

func (s *Service) appStatus(ctx context.Context, a registry.Entry, entries []journal.Entry) appStatus {
	st := appStatus{name: a.Name, sha7: statusNone, state: docker.StateMissing, health: docker.NoHealth, last: statusNever}
	if cur := registry.LoadState(a.Dir).Current; cur != "" {
		st.sha7 = registry.Sha7(cur)
	}
	if op, ok := journal.LastContainerOp(entries); ok {
		st.last = op.Stamp()
	}
	conf, confErr := registry.LoadAppConf(a.Dir)
	cname := s.container(ctx, a.Name, a.Dir, conf)
	if cname == "" {
		return st
	}
	st.state = s.Docker.State(ctx, cname)
	st.health = s.Docker.HealthStatus(ctx, cname)
	if st.health == docker.NoHealth && st.up() && confErr == nil && conf.HealthMode() == registry.HealthHTTP {
		st.health = probeFail
		if s.HTTP.HTTPOK(ctx, netprobe.HTTPURL(conf.Port, conf.HealthPath), statusProbeTime) {
			st.health = probeOK
		}
	}
	return st
}

func (s *Service) history(app string) int {
	entries, err := s.Journal.Entries()
	if err != nil {
		s.Log.Warn("journal read failed", "error", err.Error())
	}
	tail := journal.Tail(journal.ForApp(entries, app), historyLines)
	if len(tail) == 0 {
		s.Out.Printf(msgNoHistory, app)
		return ExitOK
	}
	for _, e := range tail {
		s.Out.Println(e.Raw)
	}
	return ExitOK
}
