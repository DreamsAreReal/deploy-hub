package docker

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

const (
	formatEnv      = "{{json .Config.Env}}"
	formatImage    = "{{.Image}}"
	envAssign      = "="
	formatNetworks = `{{.Label "com.docker.compose.project"}}` + "\t{{.Networks}}"
	fieldSeparator = "\t"
	listSeparator  = ","
)

func (c Client) RunEnv(ctx context.Context, container string) ([]string, error) {
	image, err := c.inspect(ctx, container, formatImage)
	if err != nil {
		return nil, err
	}
	running, err := c.envOf(ctx, "inspect", container)
	if err != nil {
		return nil, err
	}
	baked, err := c.envOf(ctx, "image", "inspect", image)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range running {
		if name, _, _ := strings.Cut(entry, envAssign); !slices.Contains(baked, entry) && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

func (c Client) envOf(ctx context.Context, args ...string) ([]string, error) {
	res, err := c.run(ctx, nil, "", "", append(args[:len(args)-1:len(args)-1], "--format", formatEnv, args[len(args)-1])...)
	if err != nil {
		return nil, err
	}
	var env []string
	return env, json.Unmarshal([]byte(res.Stdout), &env)
}

func (c Client) BoundPorts(ctx context.Context, container string) ([]int, error) {
	v, err := c.inspect(ctx, container, formatPorts)
	if err != nil {
		return nil, err
	}
	var ports []int
	for _, line := range strings.Split(v, "\n") {
		port, convErr := strconv.Atoi(leadingDigits.FindString(strings.TrimSpace(line)))
		if convErr == nil && !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	return ports, nil
}

func (c Client) ProjectNetworks(ctx context.Context) (map[string][]string, error) {
	res, err := c.run(ctx, nil, "", "", "ps", "--format", formatNetworks)
	if err != nil {
		return nil, err
	}
	networks := map[string][]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		project, names, found := strings.Cut(strings.TrimSpace(line), fieldSeparator)
		if found && project != "" {
			networks[project] = joinNew(networks[project], strings.Split(names, listSeparator))
		}
	}
	return networks, nil
}

func joinNew(list, names []string) []string {
	for _, name := range names {
		if name != "" && !slices.Contains(list, name) {
			list = append(list, name)
		}
	}
	return list
}
