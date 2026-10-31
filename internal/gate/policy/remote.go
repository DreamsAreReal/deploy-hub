package policy

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	RemoteBundleDir = "/deploy-hub/remote"
	RemoteInitDir   = "/etc/init.d/"
)

var RemoteRoots = []string{
	"/usr/bin/routerbot-", "/usr/bin/wgru-watchdog", "/usr/bin/wifi-neighbors",
	RemoteInitDir + "routerbot", "/etc/routerbot/uci.d/", "/etc/routerbot/nft/",
}

var (
	ErrRemote    = errors.New("invalid remote spec")
	remoteTarget = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
)

type Remote struct {
	Target  string       `yaml:"target"`
	Files   []RemoteFile `yaml:"files"`
	Restart []string     `yaml:"restart"`
}

type RemoteFile struct {
	Src  string `yaml:"src"`
	Dst  string `yaml:"dst"`
	Mode string `yaml:"mode"`
}

func InRemoteRoot(dst string) bool {
	for _, root := range RemoteRoots {
		if dst == root && !openRoot(root) {
			return true
		}
		rest, ok := strings.CutPrefix(dst, root)
		if ok && openRoot(root) && hostName.MatchString(rest) {
			return true
		}
	}
	return false
}

func openRoot(root string) bool {
	return strings.HasSuffix(root, "/") || strings.HasSuffix(root, "-")
}

func (r Remote) Inits() []string {
	var out []string
	for _, f := range r.Files {
		if name, ok := strings.CutPrefix(f.Dst, RemoteInitDir); ok {
			out = append(out, name)
		}
	}
	return out
}

func validateRemote(app string, r *Remote) error {
	if r == nil {
		return nil
	}
	errs := []error{}
	if !remoteTarget.MatchString(r.Target) {
		errs = append(errs, fmt.Errorf("%w: app %s: target %q must be a short lower-case name", ErrRemote, app, r.Target))
	}
	if len(r.Files) == 0 {
		errs = append(errs, fmt.Errorf("%w: app %s: remote.files must not be empty", ErrRemote, app))
	}
	srcs, dsts := map[string]bool{}, map[string]bool{}
	for _, f := range r.Files {
		errs = append(errs, f.validate(app, srcs, dsts))
	}
	inits := r.Inits()
	for _, s := range r.Restart {
		if !slices.Contains(inits, s) {
			errs = append(errs, fmt.Errorf("%w: app %s: restart %q must be an init script this spec installs", ErrRemote, app, s))
		}
	}
	return errors.Join(errs...)
}

func (f RemoteFile) validate(app string, srcs, dsts map[string]bool) error {
	errs := []error{validSrc(app, f.Src), validMode(app, f.Mode)}
	if !InRemoteRoot(f.Dst) || path.Clean(f.Dst) != f.Dst {
		errs = append(errs, fmt.Errorf("%w: app %s: %q must be a file under one of %v", ErrRemote, app, f.Dst, RemoteRoots))
	}
	if srcs[f.Src] || dsts[f.Dst] {
		errs = append(errs, fmt.Errorf("%w: app %s: %s -> %s is listed twice", ErrRemote, app, f.Src, f.Dst))
	}
	srcs[f.Src], dsts[f.Dst] = true, true
	return errors.Join(errs...)
}
