package compose

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	loopback       = "127.0.0.1"
	keyHostIP      = "host_ip"
	keyNetworkMode = "network_mode"
	networkHost    = "host"
	tagString      = "!!str"
	portSeparator  = ":"
)

var (
	ErrPublicBind  = errors.New("ports must bind 127.0.0.1 or omit the host IP — services are never published on public interfaces")
	ErrHostNetwork = errors.New("network_mode: host exposes every port of the service on all interfaces")
)

func bindLoopback(name string, svc *yaml.Node) error {
	if mode := get(svc, keyNetworkMode); mode != nil && mode.Value == networkHost {
		return fmt.Errorf("%w: service %s", ErrHostNetwork, name)
	}
	ports := get(svc, keyPorts)
	if ports == nil {
		return nil
	}
	for _, p := range ports.Content {
		if err := bindPort(p); err != nil {
			return fmt.Errorf("%w: service %s", err, name)
		}
	}
	return nil
}

func bindPort(p *yaml.Node) error {
	if p.Kind == yaml.MappingNode {
		return bindLongPort(p)
	}
	v := p.Value
	switch {
	case strings.HasPrefix(v, loopback+portSeparator):
		return nil
	case strings.Count(v, portSeparator) == 0:
		v = loopback + portSeparator + portSeparator + v
	case strings.Count(v, portSeparator) == 1:
		v = loopback + portSeparator + v
	default:
		return ErrPublicBind
	}
	p.Kind, p.Tag, p.Value, p.Style = yaml.ScalarNode, tagString, v, yaml.DoubleQuotedStyle
	return nil
}

func bindLongPort(p *yaml.Node) error {
	ip := get(p, keyHostIP)
	if ip == nil {
		set(p, keyHostIP, str(loopback))
		return nil
	}
	if ip.Value != loopback {
		return ErrPublicBind
	}
	return nil
}
