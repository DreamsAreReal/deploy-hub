package stage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	DockerBinary   = "docker"
	namePrefix     = "deploy-hub-gate-"
	networkSuffix  = "-net"
	loopback       = "127.0.0.1"
	pidsLimit      = "256"
	readyInterval  = time.Second
	probeTimeout   = 5 * time.Second
	firstBadStatus = 400
	inspectFormat  = "{{.State.Running}} {{.State.OOMKilled}} {{.RestartCount}}"
	stateFields    = 3
	stateTrue      = "true"
	flagNetwork    = "--network"
	schemeHTTP     = "http://"
	portFormat     = "%d/tcp"
	publishFormat  = "%s::%d"
)

var (
	ErrNotReady = errors.New("the app did not become healthy in the isolated stage")
	ErrState    = errors.New("unexpected container state")
)

type Stage struct {
	ws       gate.Workspace
	Name     string
	Network  string
	HostAddr string
}

type State struct {
	Running  bool
	OOM      bool
	Restarts int
}

func Start(ctx context.Context, ws gate.Workspace, purpose string) (*Stage, error) {
	s := &Stage{ws: ws, Name: namePrefix + ws.AppName + "-" + purpose}
	s.Network = s.Name + networkSuffix
	s.remove(ctx)
	if err := s.docker(ctx, "network", "create", s.Network); err != nil {
		return nil, err
	}
	if err := s.docker(ctx, s.runArgs()...); err != nil {
		s.Stop(ctx)
		return nil, err
	}
	if err := s.resolve(ctx); err != nil {
		s.Stop(ctx)
		return nil, err
	}
	if err := s.waitReady(ctx); err != nil {
		s.Stop(ctx)
		return nil, err
	}
	ws.Log.Info("gate stage ready", "stage", s.Name, "addr", s.HostAddr, "cpus", ws.App.CPUs, "memory", ws.App.Memory)
	return s, nil
}

func (s *Stage) runArgs() []string {
	lim := s.ws.App
	args := []string{
		"run", "-d", "--name", s.Name, flagNetwork, s.Network, "--network-alias", s.Name,
		"--cpus", lim.CPUs, "--memory", lim.Memory, "--memory-swap", lim.Memory, "--pids-limit", pidsLimit,
		"--security-opt", "no-new-privileges", "-p", fmt.Sprintf(publishFormat, loopback, s.ws.App.Port),
	}
	for _, e := range s.ws.App.Env {
		args = append(args, "-e", e)
	}
	return append(args, s.ws.Image)
}

func (s *Stage) docker(ctx context.Context, args ...string) error {
	_, err := s.ws.Run(ctx, DockerBinary, args...)
	if err != nil {
		return fmt.Errorf("docker %s: %w: %s", args[0], err, gate.Tail(procexec.Output(err)))
	}
	return nil
}

func (s *Stage) resolve(ctx context.Context) error {
	res, err := s.ws.Run(ctx, DockerBinary, "port", s.Name, fmt.Sprintf(portFormat, s.ws.App.Port))
	if err != nil {
		return fmt.Errorf("docker port: %w: %s", err, gate.Tail(procexec.Output(err)))
	}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		host, port, perr := net.SplitHostPort(strings.TrimSpace(line))
		if perr == nil && host == loopback {
			s.HostAddr = net.JoinHostPort(host, port)
			return nil
		}
	}
	return fmt.Errorf("%w: no loopback port in %q", ErrState, res.Stdout)
}

func (s *Stage) URL(path string) string {
	return schemeHTTP + s.HostAddr + path
}

func (s *Stage) InternalURL(path string) string {
	return schemeHTTP + net.JoinHostPort(s.Name, strconv.Itoa(s.ws.App.Port)) + path
}

func (s *Stage) waitReady(ctx context.Context) error {
	client := &http.Client{Timeout: probeTimeout}
	deadline := time.Now().Add(s.ws.Policy.Limits.StageReady)
	for time.Now().Before(deadline) {
		if healthy(ctx, client, s.URL(s.ws.App.Health)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyInterval):
		}
	}
	return ErrNotReady
}

func healthy(ctx context.Context, client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < firstBadStatus
}

func (s *Stage) State(ctx context.Context) (State, error) {
	res, err := s.ws.Run(ctx, DockerBinary, "inspect", "--format", inspectFormat, s.Name)
	if err != nil {
		return State{}, fmt.Errorf("docker inspect: %w: %s", err, gate.Tail(procexec.Output(err)))
	}
	f := strings.Fields(res.Stdout)
	if len(f) != stateFields {
		return State{}, fmt.Errorf("%w: %q", ErrState, res.Stdout)
	}
	restarts, err := strconv.Atoi(f[2])
	if err != nil {
		return State{}, fmt.Errorf("%w: %q", ErrState, res.Stdout)
	}
	return State{Running: f[0] == stateTrue, OOM: f[1] == stateTrue, Restarts: restarts}, nil
}

func (s *Stage) Stop(ctx context.Context) {
	s.remove(context.WithoutCancel(ctx))
}

func (s *Stage) remove(ctx context.Context) {
	_, _ = s.ws.Run(ctx, DockerBinary, "rm", "-f", s.Name)
	_, _ = s.ws.Run(ctx, DockerBinary, "network", "rm", s.Network)
}
