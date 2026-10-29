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
	"github.com/DreamsAreReal/deploy-hub/internal/gate/crawler"
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
	inspectFormat  = "{{.State.Running}} {{.State.OOMKilled}} {{.RestartCount}} {{.State.ExitCode}}"
	stateFields    = 4
	logsTail       = "50"
	diagTimeout    = 10 * time.Second
	diagFormat     = "%w; container: %s; logs: %s"
	stateFormat    = "running=%t oom=%t restarts=%d exit=%d"
	stateTrue      = "true"
	flagNetwork    = "--network"
	schemeHTTP     = "http://"
	portFormat     = "%d/tcp"
	publishFormat  = "%s::%d"
	mountFormat    = "type=volume,src=%s,dst=%s"
	volumeFormat   = "%s-data-%d"
	reasonNoImage  = "no image was provided for an app with an HTTP surface"
	isolated       = "none"
	flagEnv        = "-e"
	flagPids       = "--pids-limit"
	flagSecurity   = "--security-opt"
	noPrivileges   = "no-new-privileges"
	stopSeconds    = "10"
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
	crawler  []crawler.Module
}

type State struct {
	Running  bool
	OOM      bool
	Restarts int
	ExitCode int
}

func Start(ctx context.Context, ws gate.Workspace, purpose string) (*Stage, error) {
	return StartSeeded(ctx, ws, purpose, nil)
}

func StartSeeded(ctx context.Context, ws gate.Workspace, purpose string, seed []string) (*Stage, error) {
	s := &Stage{ws: ws, Name: namePrefix + ws.AppName + "-" + purpose}
	s.Network = s.Name + networkSuffix
	s.remove(ctx)
	if err := s.docker(ctx, "network", "create", s.Network); err != nil {
		return nil, err
	}
	if err := s.seed(ctx, seed); err != nil {
		s.Stop(ctx)
		return nil, err
	}
	if err := s.docker(ctx, s.runArgs(s.ws.Image)...); err != nil {
		s.Stop(ctx)
		return nil, err
	}
	if err := s.resolve(ctx); err != nil {
		return nil, s.abort(ctx, err)
	}
	if err := s.waitReady(ctx); err != nil {
		return nil, s.abort(ctx, err)
	}
	ws.Log.Info("gate stage ready", "stage", s.Name, "addr", s.HostAddr, "cpus", ws.App.CPUs, "memory", ws.App.Memory)
	return s, nil
}

func (s *Stage) Swap(ctx context.Context, image string) error {
	_, _ = s.ws.Run(ctx, DockerBinary, "stop", "--time", stopSeconds, s.Name)
	_, _ = s.ws.Run(ctx, DockerBinary, "rm", "-f", s.Name)
	if err := s.docker(ctx, s.runArgs(image)...); err != nil {
		return err
	}
	if err := s.resolve(ctx); err != nil {
		return err
	}
	return s.waitReady(ctx)
}

func (s *Stage) runArgs(image string) []string {
	lim := s.ws.App
	args := []string{
		"run", "-d", "--name", s.Name, flagNetwork, s.Network, "--network-alias", s.Name,
		"--cpus", lim.CPUs, "--memory", lim.Memory, "--memory-swap", lim.Memory, flagPids, pidsLimit,
		flagSecurity, noPrivileges, "-p", fmt.Sprintf(publishFormat, loopback, s.ws.App.Port),
	}
	for _, e := range s.ws.App.Env {
		args = append(args, flagEnv, e)
	}
	for i, dst := range s.ws.App.Data {
		args = append(args, "--mount", fmt.Sprintf(mountFormat, s.volume(i), dst))
	}
	return append(args, image)
}

func (s *Stage) seed(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return nil
	}
	run := []string{"run", "--rm", flagNetwork, isolated, flagSecurity, noPrivileges}
	for _, e := range s.ws.App.Env {
		run = append(run, flagEnv, e)
	}
	for i, dst := range s.ws.App.Data {
		run = append(run, "--mount", fmt.Sprintf(mountFormat, s.volume(i), dst))
	}
	return s.docker(ctx, append(append(run, s.ws.Image), args...)...)
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
	code := status(ctx, client, url)
	return code != 0 && code < firstBadStatus
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
	restarts, rerr := strconv.Atoi(f[2])
	exit, eerr := strconv.Atoi(f[3])
	if rerr != nil || eerr != nil {
		return State{}, fmt.Errorf("%w: %q", ErrState, res.Stdout)
	}
	return State{Running: f[0] == stateTrue, OOM: f[1] == stateTrue, Restarts: restarts, ExitCode: exit}, nil
}

func (s *Stage) Stop(ctx context.Context) {
	s.remove(context.WithoutCancel(ctx))
}

func (s *Stage) remove(ctx context.Context) {
	_, _ = s.ws.Run(ctx, DockerBinary, "rm", "-f", s.Name)
	for i := range s.ws.App.Data {
		_, _ = s.ws.Run(ctx, DockerBinary, "volume", "rm", "-f", s.volume(i))
	}
	_, _ = s.ws.Run(ctx, DockerBinary, "network", "rm", s.Network)
}

func (s *Stage) volume(i int) string {
	return fmt.Sprintf(volumeFormat, s.Name, i)
}

func (s *Stage) abort(ctx context.Context, cause error) error {
	err := s.diagnose(ctx, cause)
	s.Stop(ctx)
	return err
}

func (s *Stage) diagnose(ctx context.Context, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), diagTimeout)
	defer cancel()
	container := s.describeState(ctx)
	logs := s.logs(ctx)
	s.ws.Log.Warn("gate stage failed", "stage", s.Name, "error", cause.Error(), "container", container, "logs", logs)
	return fmt.Errorf(diagFormat, cause, container, gate.Tail(logs))
}

func (s *Stage) describeState(ctx context.Context) string {
	st, err := s.State(ctx)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf(stateFormat, st.Running, st.OOM, st.Restarts, st.ExitCode)
}

func (s *Stage) logs(ctx context.Context) string {
	res, err := s.ws.Run(ctx, DockerBinary, "logs", "--tail", logsTail, s.Name)
	if err != nil {
		return fmt.Sprintf("docker logs: %v: %s", err, gate.Tail(procexec.Output(err)))
	}
	return res.Combined()
}
