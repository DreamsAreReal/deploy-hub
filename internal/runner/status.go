package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/DreamsAreReal/deploy-hub/internal/caddy"
	"github.com/DreamsAreReal/deploy-hub/internal/docker"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/arch"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/netprobe"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

type appStatus struct {
	name      string
	dir       string
	sha7      string
	state     string
	health    string
	last      string
	container string
	public    bool
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
	shared := s.networkDrift(ctx, apps)
	for _, a := range apps {
		st := s.appStatus(ctx, a, journal.ForApp(entries, a.Name))
		s.Out.Println(st.line())
		labels := obs.Labels{obs.LabelApp: a.Name}
		s.samples = append(s.samples,
			obs.Sample{Name: obs.MetricAppUp, Labels: labels, Value: obs.Bool(st.up())},
			obs.Sample{Name: obs.MetricAppHealthy, Labels: labels, Value: obs.Bool(st.healthy())},
		)
		s.liveDrift(ctx, st, shared[a.Name])
	}
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricAppsRegistered, Labels: obs.Labels{}, Value: float64(len(apps))})
	return ExitOK
}

func (s *Service) appStatus(ctx context.Context, a registry.Entry, entries []journal.Entry) appStatus {
	st := appStatus{name: a.Name, dir: a.Dir, sha7: statusNone, state: docker.StateMissing, health: docker.NoHealth, last: statusNever}
	if cur := registry.LoadState(a.Dir).Current; cur != "" {
		st.sha7 = registry.Sha7(cur)
	}
	if op, ok := journal.LastContainerOp(entries); ok {
		st.last = op.Stamp()
	}
	conf, confErr := registry.LoadAppConf(a.Dir)
	st.public = confErr == nil && caddy.Public(conf)
	cname := s.container(ctx, a.Name, a.Dir, conf)
	if cname == "" {
		return st
	}
	st.container = cname
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

func (s *Service) networkDrift(ctx context.Context, apps []registry.Entry) map[string][]string {
	if s.Catalog == nil {
		return nil
	}
	networks, err := s.Docker.ProjectNetworks(ctx)
	if err != nil {
		s.Log.Warn("docker networks unreadable", "error", err.Error())
		return nil
	}
	registered := map[string][]string{}
	for _, a := range apps {
		if names, ok := networks[a.Name]; ok {
			registered[a.Name] = names
		}
	}
	return s.Catalog.NetworkDrift(registered)
}

func secretsDrift(dir string) []string {
	info, err := os.Stat(filepath.Join(dir, registry.EnvFile))
	if err != nil || info.Mode().Perm()&groupOtherBits == 0 {
		return nil
	}
	return []string{fmt.Sprintf(msgEnvReadable, registry.EnvFile, info.Mode().Perm())}
}

func (s *Service) liveDrift(ctx context.Context, st appStatus, shared []string) {
	if s.Catalog == nil || !st.up() {
		return
	}
	env, err := s.Docker.RunEnv(ctx, st.container)
	var ports []int
	if err == nil {
		ports, err = s.Docker.BoundPorts(ctx, st.container)
	}
	if err != nil {
		s.Log.Warn("live facts unreadable", "app", st.name, "error", err.Error())
		return
	}
	facts := arch.Facts{Env: env, Ports: ports}
	if st.public {
		facts.Sites = []string{st.name}
	}
	service, known := s.Catalog.Services[st.name]
	drift := append(service.LiveDrift(facts), shared...)
	drift = append(drift, secretsDrift(st.dir)...)
	if !known {
		drift = append(drift, msgNotInCatalog)
	}
	for _, item := range drift {
		s.Log.Warn("live drift", "app", st.name, "item", item)
	}
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricArchLiveDrift, Labels: obs.Labels{obs.LabelApp: st.name}, Value: float64(len(drift))})
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
