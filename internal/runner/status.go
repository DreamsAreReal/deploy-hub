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

type AppStatus struct {
	Name     string `json:"name"`
	Sha7     string `json:"sha7"`
	Current  string `json:"current"`
	Previous string `json:"previous"`
	State    string `json:"state"`
	Health   string `json:"health"`
	Last     string `json:"last"`
}

func (s AppStatus) line() string {
	return fmt.Sprintf("%s | %s | %s/%s | %s", s.Name, s.Sha7, s.State, s.Health, s.Last)
}

func (s AppStatus) Up() bool {
	return s.State == health.StatusRunning
}

func (s AppStatus) Healthy() bool {
	return s.Health == health.StatusHealthy || s.Health == probeOK
}

func (s *Service) Snapshot(ctx context.Context) ([]AppStatus, error) {
	apps, err := s.Apps.All()
	if err != nil {
		return nil, err
	}
	entries, err := s.Journal.Entries()
	if err != nil {
		s.Log.Warn("journal read failed", "error", err.Error())
	}
	out := make([]AppStatus, 0, len(apps))
	for _, a := range apps {
		out = append(out, s.appStatus(ctx, a, journal.ForApp(entries, a.Name)))
	}
	return out, nil
}

func (s *Service) status(ctx context.Context) int {
	apps, err := s.Snapshot(ctx)
	if err != nil {
		s.Err.Println(msgRegistryError)
		s.Log.Error("apps.list unreadable", "error", err.Error())
		return ExitFail
	}
	s.Out.Println(statusHeader)
	for _, st := range apps {
		s.Out.Println(st.line())
		labels := obs.Labels{obs.LabelApp: st.Name}
		s.samples = append(s.samples,
			obs.Sample{Name: obs.MetricAppUp, Labels: labels, Value: obs.Bool(st.Up())},
			obs.Sample{Name: obs.MetricAppHealthy, Labels: labels, Value: obs.Bool(st.Healthy())},
		)
	}
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricAppsRegistered, Labels: obs.Labels{}, Value: float64(len(apps))})
	return ExitOK
}

func (s *Service) appStatus(ctx context.Context, a registry.Entry, entries []journal.Entry) AppStatus {
	st := AppStatus{Name: a.Name, Sha7: statusNone, State: docker.StateMissing, Health: docker.NoHealth, Last: statusNever}
	state := registry.LoadState(a.Dir)
	st.Current, st.Previous = state.Current, state.Previous
	if state.Current != "" {
		st.Sha7 = registry.Sha7(state.Current)
	}
	if op, ok := journal.LastContainerOp(entries); ok {
		st.Last = op.Stamp()
	}
	conf, confErr := registry.LoadAppConf(a.Dir)
	cname := s.container(ctx, a.Name, a.Dir, conf)
	if cname == "" {
		return st
	}
	st.State = s.Docker.State(ctx, cname)
	st.Health = s.Docker.HealthStatus(ctx, cname)
	if st.Health == docker.NoHealth && st.Up() && confErr == nil && conf.HealthMode() == registry.HealthHTTP {
		st.Health = probeFail
		if s.HTTP.HTTPOK(ctx, netprobe.HTTPURL(conf.Port, conf.HealthPath), statusProbeTime) {
			st.Health = probeOK
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
