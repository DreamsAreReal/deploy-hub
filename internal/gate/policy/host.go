package policy

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	HostBundleDir = "/deploy-hub/host"
	UnitDir       = "/etc/systemd/system/"
	CaddySiteDir  = "/etc/caddy/extra/"
	RunnerDir     = "/home/runner/runner/"
	PrevInfix     = ".prev-"
	suffixService = ".service"
	suffixTimer   = ".timer"
	maxHostMode   = 0o755
	modeBase      = 8
	modeBits      = 32

	minRestartWait = 30 * time.Second
	maxRestartWait = 10 * time.Minute
)

var HostRoots = []string{"/usr/local/sbin/", UnitDir, CaddySiteDir, "/var/lib/node_exporter/", RunnerDir}

var (
	hostAccount = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	hostMode    = regexp.MustCompile(`^0[0-7]{3}$`)
	hostName    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@-]{0,99}$`)
	hostEnvFile = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,40}\.env$`)
	metricName  = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,99}$`)
)

var ErrHost = errors.New("invalid host spec")

type Host struct {
	Files   []HostFile   `yaml:"files"`
	Dirs    []HostDir    `yaml:"dirs"`
	Timers  []string     `yaml:"timers"`
	Checks  []string     `yaml:"checks"`
	Restart *HostRestart `yaml:"restart"`
}

type HostRestart struct {
	Unit    string        `yaml:"unit"`
	Busy    string        `yaml:"busy"`
	Version string        `yaml:"version"`
	Within  time.Duration `yaml:"within"`
}

type HostFile struct {
	Src      string `yaml:"src"`
	Dst      string `yaml:"dst"`
	Owner    string `yaml:"owner"`
	Group    string `yaml:"group"`
	Mode     string `yaml:"mode"`
	Env      string `yaml:"env"`
	KeepPrev bool   `yaml:"keep_prev"`
}

type HostDir struct {
	Path  string `yaml:"path"`
	Owner string `yaml:"owner"`
	Group string `yaml:"group"`
	Mode  string `yaml:"mode"`
}

func ParseMode(v string) (uint32, error) {
	if !hostMode.MatchString(v) {
		return 0, fmt.Errorf("%w: mode %q must be four octal digits", ErrHost, v)
	}
	m, err := strconv.ParseUint(v, modeBase, modeBits)
	if err != nil || m&^maxHostMode != 0 {
		return 0, fmt.Errorf("%w: mode %s may not set special bits or let group and others write", ErrHost, v)
	}
	return uint32(m), nil
}

func InHostRoot(dst string) bool {
	return slices.Contains(HostRoots, path.Dir(dst)+"/")
}

func (h Host) Units() []string {
	var out []string
	for _, f := range h.Files {
		if strings.HasPrefix(f.Dst, UnitDir) {
			out = append(out, path.Base(f.Dst))
		}
	}
	return out
}

func validateHost(app string, h *Host) error {
	if h == nil {
		return nil
	}
	errs := []error{nonEmptyHost(app, "files", len(h.Files))}
	srcs, dsts := map[string]bool{}, map[string]bool{}
	for _, f := range h.Files {
		errs = append(errs, f.validate(app, srcs, dsts))
	}
	for _, d := range h.Dirs {
		errs = append(errs, d.validate(app))
	}
	units := h.Units()
	errs = append(errs, validUnits(app, "timers", suffixTimer, h.Timers, units), validUnits(app, "checks", suffixService, h.Checks, units), h.validateRestart(app))
	return errors.Join(errs...)
}

func (h Host) Kept() []HostFile {
	var out []HostFile
	for _, f := range h.Files {
		if f.KeepPrev {
			out = append(out, f)
		}
	}
	return out
}

func (h Host) validateRestart(app string) error {
	kept := len(h.Kept())
	r := h.Restart
	switch {
	case r == nil && kept == 0:
		return nil
	case r == nil || kept == 0:
		return fmt.Errorf("%w: app %s: host.restart and keep_prev files go together", ErrHost, app)
	}
	unit := hostName.MatchString(r.Unit) && strings.HasSuffix(r.Unit, suffixService)
	if !unit || !metricName.MatchString(r.Busy) || !metricName.MatchString(r.Version) || r.Within < minRestartWait || r.Within > maxRestartWait {
		return fmt.Errorf("%w: app %s: host.restart needs a .service unit, busy and version metric names and within %s..%s", ErrHost, app, minRestartWait, maxRestartWait)
	}
	return nil
}

func nonEmptyHost(app, field string, n int) error {
	if n == 0 {
		return fmt.Errorf("%w: app %s: host.%s must not be empty", ErrHost, app, field)
	}
	return nil
}

func (f HostFile) validate(app string, srcs, dsts map[string]bool) error {
	errs := []error{validSrc(app, f.Src), validDst(app, f.Dst), validAccount(app, f.Owner, f.Group), validMode(app, f.Mode)}
	if f.Env != "" && !hostEnvFile.MatchString(f.Env) {
		errs = append(errs, fmt.Errorf("%w: app %s: env %q must be a *.env file name in the app directory", ErrHost, app, f.Env))
	}
	if srcs[f.Src] || dsts[f.Dst] {
		errs = append(errs, fmt.Errorf("%w: app %s: %s -> %s is listed twice", ErrHost, app, f.Src, f.Dst))
	}
	srcs[f.Src], dsts[f.Dst] = true, true
	return errors.Join(errs...)
}

func (d HostDir) validate(app string) error {
	return errors.Join(validDst(app, d.Path), validAccount(app, d.Owner, d.Group), validMode(app, d.Mode))
}

func validSrc(app, src string) error {
	clean := path.Clean(src)
	if src == "" || clean != src || path.IsAbs(src) || strings.HasPrefix(clean, "..") || !hostName.MatchString(path.Base(src)) {
		return fmt.Errorf("%w: app %s: src %q must be a clean relative path inside %s", ErrHost, app, src, HostBundleDir)
	}
	return nil
}

func validDst(app, dst string) error {
	if !InHostRoot(dst) || path.Clean(dst) != dst || !hostName.MatchString(path.Base(dst)) {
		return fmt.Errorf("%w: app %s: %q must be a file name directly inside one of %v", ErrHost, app, dst, HostRoots)
	}
	return nil
}

func validAccount(app, owner, group string) error {
	if !hostAccount.MatchString(owner) || !hostAccount.MatchString(group) {
		return fmt.Errorf("%w: app %s: owner %q and group %q must be account names", ErrHost, app, owner, group)
	}
	return nil
}

func validMode(app, mode string) error {
	if _, err := ParseMode(mode); err != nil {
		return fmt.Errorf("app %s: %w", app, err)
	}
	return nil
}

func validUnits(app, field, suffix string, names, installed []string) error {
	var errs []error
	for _, n := range names {
		if !strings.HasSuffix(n, suffix) || !slices.Contains(installed, n) {
			errs = append(errs, fmt.Errorf("%w: app %s: host.%s entry %q must be a %s unit this spec installs", ErrHost, app, field, n, suffix))
		}
	}
	return errors.Join(errs...)
}
