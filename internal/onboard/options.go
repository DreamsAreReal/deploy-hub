package onboard

import (
	"errors"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/provision"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	Usage = `usage: deploy-hub onboard <repo> [--profile <static|bot|service|host>] [--port N] [--cport N] [--mem LIMIT] [--health-path P] [--app NAME] [--dockerfile PATH] [--compose PATH] [--ref BRANCH] [--dry-run]
       (--profile is optional: omitted => auto-detect from the repo)
       (--ref reads the repo from BRANCH instead of the default one; the caller stub must already be there)
       deploy-hub onboard status | history <app>     # read-only, over the deploy channel`
	defaultDockerfile = "Dockerfile"
	flagPrefix        = "-"
	slugSeparator     = "/"
)

var (
	ErrUsage      = errors.New(Usage)
	ErrHelp       = errors.New("help requested")
	ErrBadProfile = errors.New("error: --profile must be static, bot or service")
)

type Options struct {
	Repo       string
	Profile    string
	Port       string
	CPort      string
	Mem        string
	HealthPath string
	App        string
	Dockerfile string
	Compose    string
	Ref        string
	DryRun     bool
}

func ParseOptions(args []string, owner string) (Options, error) {
	o := Options{HealthPath: "/", Dockerfile: defaultDockerfile}
	values := map[string]*string{
		"--profile": &o.Profile, "--port": &o.Port, "--cport": &o.CPort, "--mem": &o.Mem,
		"--health-path": &o.HealthPath, "--app": &o.App, "--dockerfile": &o.Dockerfile, "--compose": &o.Compose, "--ref": &o.Ref,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		dst, isValue := values[a]
		switch {
		case isValue && i+1 < len(args):
			*dst = args[i+1]
			i++
		case a == "--dry-run":
			o.DryRun = true
		case a == "-h" || a == "--help":
			return o, ErrHelp
		case isValue || strings.HasPrefix(a, flagPrefix):
			return o, ErrUsage
		default:
			o.Repo = a
		}
	}
	return o.normalize(owner)
}

func (o Options) normalize(owner string) (Options, error) {
	if o.Repo == "" {
		return o, ErrUsage
	}
	if o.Profile != "" && !provision.ValidProfile(o.Profile) {
		return o, ErrBadProfile
	}
	if !strings.Contains(o.Repo, slugSeparator) {
		o.Repo = owner + slugSeparator + o.Repo
	}
	if o.App == "" {
		o.App = strings.ToLower(o.Repo[strings.LastIndex(o.Repo, slugSeparator)+1:])
	}
	return o, o.validate()
}

func (o Options) validate() error {
	errs := errors.Join(validate.RepoSlug(o.Repo), validate.AppName(o.App), validate.OptionalPort(o.Port),
		validate.OptionalPort(o.CPort), validate.HealthPath(o.HealthPath), validate.RelPath(o.Dockerfile))
	if o.Mem != "" {
		errs = errors.Join(errs, validate.MemLimit(o.Mem))
	}
	if o.Compose != "" {
		errs = errors.Join(errs, validate.RelPath(o.Compose))
	}
	if o.Ref != "" {
		errs = errors.Join(errs, validate.GitRef(o.Ref))
	}
	return errs
}
