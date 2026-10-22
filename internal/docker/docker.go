package docker

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	Binary           = "docker"
	ImageEnv         = "DEPLOY_IMAGE"
	PlaceholderImage = "placeholder"
	MainService      = "app"
	StateMissing     = "missing"
	NoHealth         = "-"
	PullNever        = "never"
	PullMissing      = "missing"

	formatTag       = "{{.Tag}}"
	formatState     = "{{.State.Status}}"
	formatHealth    = "{{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}"
	formatComposePs = "{{.Name}}\t{{.Labels}}"
	formatLabel     = "{{index .Config.Labels %q}}"
	formatRWMounts  = "{{range .Mounts}}{{if .RW}}{{.Destination}}\n{{end}}{{end}}"
	oneOffLabel     = "com.docker.compose.oneoff=True"
	formatPorts     = "{{range $p,$c := .NetworkSettings.Ports}}{{range $c}}{{$p}} {{.HostPort}}\n{{end}}{{end}}"
	flagDetach      = "-d"
	flagFile        = "-f"
	verbCompose     = "compose"
)

var leadingDigits = regexp.MustCompile(`^[0-9]+`)

type Client struct {
	Exec procexec.Runner
}

func (c Client) run(ctx context.Context, env []string, stdin, dir string, args ...string) (procexec.Result, error) {
	return c.Exec.Run(ctx, procexec.Command{Name: Binary, Args: args, Env: env, Stdin: stdin, Dir: dir})
}

func imageEnv(image string) []string {
	return []string{ImageEnv + "=" + image}
}

func (c Client) Login(ctx context.Context, registry, user, token string) error {
	_, err := c.run(ctx, nil, token, "", "login", registry, "-u", user, "--password-stdin")
	return err
}

func (c Client) Logout(ctx context.Context, registry string) error {
	_, err := c.run(ctx, nil, "", "", "logout", registry)
	return err
}

func (c Client) Pull(ctx context.Context, ref string) (string, error) {
	res, err := c.run(ctx, nil, "", "", "pull", "-q", ref)
	return res.Combined(), err
}

func (c Client) Tag(ctx context.Context, source, target string) error {
	_, err := c.run(ctx, nil, "", "", "tag", source, target)
	return err
}

type UpOptions struct {
	File       string
	Image      string
	Pull       string
	NoRecreate bool
	Services   []string
}

func (c Client) ComposeUp(ctx context.Context, o UpOptions) error {
	args := []string{verbCompose, flagFile, o.File, "up", flagDetach, "--pull", o.Pull}
	if o.NoRecreate {
		args = append(args, "--no-recreate")
	}
	args = append(args, o.Services...)
	_, err := c.run(ctx, imageEnv(o.Image), "", "", args...)
	return err
}

func (c Client) ComposeStop(ctx context.Context, file, image, service string) error {
	_, err := c.run(ctx, imageEnv(image), "", "", "compose", "-f", file, "stop", service)
	return err
}

func (c Client) ComposeValidate(ctx context.Context, dir, file, image string) error {
	_, err := c.run(ctx, imageEnv(image), "", dir, "compose", "-f", file, "config", "-q")
	return err
}

func (c Client) ComposeContainer(ctx context.Context, file, service string) string {
	res, err := c.run(ctx, imageEnv(PlaceholderImage), "", "", "compose", "-f", file, "ps", "-a", "--format", formatComposePs, service)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		name, labels, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if name != "" && !strings.Contains(labels, oneOffLabel) {
			return name
		}
	}
	return ""
}

func (c Client) inspect(ctx context.Context, container, format string) (string, error) {
	res, err := c.run(ctx, nil, "", "", "inspect", "--format", format, container)
	return strings.TrimSpace(res.Stdout), err
}

func (c Client) State(ctx context.Context, container string) string {
	v, err := c.inspect(ctx, container, formatState)
	if err != nil || v == "" {
		return StateMissing
	}
	return v
}

func (c Client) HealthStatus(ctx context.Context, container string) string {
	v, err := c.inspect(ctx, container, formatHealth)
	if err != nil || v == "" {
		return NoHealth
	}
	return v
}

func (c Client) ContainerPort(ctx context.Context, container, hostPort string) string {
	v, err := c.inspect(ctx, container, formatPorts)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(v, "\n") {
		spec, host, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && host == hostPort {
			return leadingDigits.FindString(spec)
		}
	}
	return ""
}

func (c Client) WritableMounts(ctx context.Context, container string) ([]string, error) {
	v, err := c.inspect(ctx, container, formatRWMounts)
	if err != nil {
		return nil, err
	}
	return strings.Fields(v), nil
}

func (c Client) Running(ctx context.Context, container string) bool {
	res, err := c.run(ctx, nil, "", "", "ps", "-q", "-f", "name=^"+container+"$")
	return err == nil && strings.TrimSpace(res.Stdout) != ""
}

func (c Client) ImageLabel(ctx context.Context, ref, key string) string {
	res, err := c.run(ctx, nil, "", "", "image", "inspect", "--format", fmt.Sprintf(formatLabel, key), ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

type Standby struct {
	Name     string
	File     string
	Service  string
	Image    string
	HostPort string
	Port     string
}

func (c Client) RunStandby(ctx context.Context, s Standby) error {
	args := []string{verbCompose, flagFile, s.File, "run", flagDetach, "--no-deps", "--name", s.Name}
	if s.HostPort != "" {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%s:%s", s.HostPort, s.Port))
	}
	_, err := c.run(ctx, imageEnv(s.Image), "", "", append(args, s.Service)...)
	return err
}

func (c Client) Stop(ctx context.Context, container string) error {
	_, err := c.run(ctx, nil, "", "", "stop", container)
	return err
}

func (c Client) Remove(ctx context.Context, container string) error {
	_, err := c.run(ctx, nil, "", "", "rm", "-f", container)
	return err
}

func (c Client) ImageTags(ctx context.Context, image string) ([]string, error) {
	res, err := c.run(ctx, nil, "", "", "images", image, "--format", formatTag)
	if err != nil {
		return nil, err
	}
	return strings.Fields(res.Stdout), nil
}

func (c Client) RemoveImage(ctx context.Context, ref string) error {
	_, err := c.run(ctx, nil, "", "", "rmi", ref)
	return err
}

func (c Client) PruneDangling(ctx context.Context) error {
	_, err := c.run(ctx, nil, "", "", "image", "prune", "-f")
	return err
}
