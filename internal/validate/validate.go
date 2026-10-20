package validate

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

var (
	appName    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,40}$`)
	shaTag     = regexp.MustCompile(`^sha-[0-9a-f]{7,40}$`)
	port       = regexp.MustCompile(`^[0-9]{2,5}$`)
	memLimit   = regexp.MustCompile(`^[0-9]{1,5}[mg]$`)
	healthPath = regexp.MustCompile(`^/[A-Za-z0-9._/-]{0,60}$`)
	ghcrImage  = regexp.MustCompile(`^ghcr\.io/[a-z0-9._/-]{1,100}$`)
	service    = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)
	repoSlug   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,39}/[A-Za-z0-9_.-]{1,100}$`)
	startEpoch = regexp.MustCompile(`^[0-9]{1,12}$`)
	relPath    = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	commit     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digest     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	host       = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

var ErrInvalid = errors.New("invalid value")

const maxPort = 65535

func check(re *regexp.Regexp, kind, value string) error {
	if re.MatchString(value) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalid, kind)
}

func AppName(v string) error    { return check(appName, "app name", v) }
func Tag(v string) error        { return check(shaTag, "tag", v) }
func MemLimit(v string) error   { return check(memLimit, "mem limit", v) }
func HealthPath(v string) error { return check(healthPath, "health path", v) }
func Image(v string) error      { return check(ghcrImage, "image", v) }
func Service(v string) error    { return check(service, "service", v) }
func RepoSlug(v string) error   { return check(repoSlug, "repo", v) }
func StartEpoch(v string) error { return check(startEpoch, "start", v) }
func Commit(v string) error     { return check(commit, "commit", v) }
func Digest(v string) error     { return check(digest, "digest", v) }
func Host(v string) error       { return check(host, "host", v) }

func Port(v string) error {
	if err := check(port, "port", v); err != nil {
		return err
	}
	var n int
	if _, err := fmt.Sscan(v, &n); err != nil || n > maxPort {
		return fmt.Errorf("%w: port", ErrInvalid)
	}
	return nil
}

func OptionalPort(v string) error {
	if v == "" {
		return nil
	}
	return Port(v)
}

func RelPath(v string) error {
	if err := check(relPath, "path", v); err != nil {
		return err
	}
	clean := path.Clean(v)
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: path", ErrInvalid)
	}
	return nil
}
