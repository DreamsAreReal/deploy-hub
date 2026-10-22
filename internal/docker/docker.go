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
	StandbyMemory    = "256m"
	PullNever        = "never"
	PullMissing      = "missing"

	formatName         = "{{.Name}}"
	formatTag          = "{{.Tag}}"
	formatState        = "{{.State.Status}}"
	formatHealth       = "{{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}"
	formatNetworks     = "{{range $k,$v := .NetworkSettings.Networks}}{{$k}}\n{{end}}"
	formatRWMounts     = "{{range .Mounts}}{{if .RW}}{{.Destination}}\n{{end}}{{end}}"
	formatPorts        = "{{range $p,$c := .NetworkSettings.Ports}}{{range $c}}{{$p}} {{.HostPort}}\n{{end}}{{end}}"
	standbyRestartMode = "no"
	flagDetach         = "-d"
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
	args := []string{"compose", "-f", o.File, "up", flagDetach, "--pull", o.Pull}
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
	res, err := c.run(ctx, imageEnv(PlaceholderImage), "", "", "compose", "-f", file, "ps", "-a", "--format", formatName, service)
	if err != nil {
		return ""
	}
	return firstLine(res.Stdout)
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

func (c Client) Network(ctx context.Context, container string) string {
	v, err := c.inspect(ctx, container, formatNetworks)
	if err != nil {
		return ""
	}
	return firstLine(v)
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

type Standby struct {
	Name     string
	Network  string
	EnvFile  string
	HostPort string
	Port     string
	Image    string
}

func (c Client) RunStandby(ctx context.Context, s Standby) error {
	args := []string{"run", flagDetach, "--name", s.Name, "--restart", standbyRestartMode}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	if s.EnvFile != "" {
		args = append(args, "--env-file", s.EnvFile)
	}
	publish := fmt.Sprintf("127.0.0.1:%s:%s", s.HostPort, s.Port)
	args = append(args, "--memory", StandbyMemory, "-p", publish, s.Image)
	_, err := c.run(ctx, nil, "", "", args...)
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

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
