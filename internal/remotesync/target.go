package remotesync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	SSHBinary     = "ssh"
	confFile      = "target.conf"
	sshConfigFile = "ssh_config"
	keyHosts      = "hosts"
	reachCommand  = "true"
	sshOption     = "-o"
)

var (
	ErrTarget      = errors.New("remote target is not set up on this server")
	ErrUnreachable = errors.New("remote target is unreachable on every path")
	hostAlias      = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
)

type targetConf struct {
	dir   string
	hosts []string
}

func loadTarget(root, name string) (targetConf, error) {
	dir := filepath.Join(root, name)
	kv, err := registry.ReadKV(filepath.Join(dir, confFile))
	if err != nil {
		return targetConf{}, fmt.Errorf("%w: %s: %w", ErrTarget, name, err)
	}
	c := targetConf{dir: dir, hosts: strings.Fields(kv[keyHosts])}
	if len(c.hosts) == 0 {
		return c, fmt.Errorf("%w: %s: no %s= in %s", ErrTarget, name, keyHosts, confFile)
	}
	for _, h := range c.hosts {
		if !hostAlias.MatchString(h) {
			return c, fmt.Errorf("%w: %s: host alias %q", ErrTarget, name, h)
		}
	}
	info, err := os.Lstat(filepath.Join(dir, sshConfigFile))
	if err != nil || !info.Mode().IsRegular() {
		return c, fmt.Errorf("%w: %s: %s must be a regular file", ErrTarget, name, sshConfigFile)
	}
	return c, nil
}

func (c targetConf) command(host, remote, stdin string) procexec.Command {
	args := []string{
		"-F", filepath.Join(c.dir, sshConfigFile), sshOption, "BatchMode=yes", sshOption, "ConnectTimeout=15",
		sshOption, "ControlMaster=no", sshOption, "ControlPath=none", sshOption, "StrictHostKeyChecking=yes", host, remote,
	}
	return procexec.Command{Name: SSHBinary, Args: args, Stdin: stdin}
}

func (s Courier) on(ctx context.Context, c targetConf, host, remote, stdin string) (string, error) {
	res, err := s.Exec.Run(ctx, c.command(host, remote, stdin))
	if err != nil {
		return procexec.Output(err), err
	}
	return res.Stdout, nil
}

func (s Courier) reach(ctx context.Context, c targetConf) (string, error) {
	var errs []error
	for _, h := range c.hosts {
		if _, err := s.on(ctx, c, h, reachCommand, ""); err == nil {
			return h, nil
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", h, err))
		}
	}
	return noHost, fmt.Errorf("%w: %w", ErrUnreachable, errors.Join(errs...))
}
