package provision

import (
	"errors"

	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

var (
	ErrBadProfile = errors.New("invalid value: profile")
	ErrPortNeeded = errors.New("invalid value: static and service profiles need port and cport")
)

type Request struct {
	App        string
	Profile    string
	Port       string
	CPort      string
	Mem        string
	HealthPath string
	Image      string
	Repo       string
}

func ValidProfile(p string) bool {
	switch p {
	case registry.ProfileStatic, registry.ProfileService, registry.ProfileBot:
		return true
	default:
		return false
	}
}

func (r Request) Validate() error {
	if !ValidProfile(r.Profile) {
		return ErrBadProfile
	}
	errs := errors.Join(
		validate.AppName(r.App),
		validate.OptionalPort(r.Port),
		validate.OptionalPort(r.CPort),
		validate.MemLimit(r.Mem),
		validate.HealthPath(r.HealthPath),
		validate.Image(r.Image),
		validate.RepoSlug(r.Repo),
	)
	if errs != nil {
		return errs
	}
	if r.Profile != registry.ProfileBot && (r.Port == "" || r.CPort == "") {
		return ErrPortNeeded
	}
	return nil
}

func (r Request) Args() []string {
	pairs := []registry.KV{
		{Key: "--app", Value: r.App}, {Key: "--profile", Value: r.Profile}, {Key: "--port", Value: r.Port},
		{Key: "--cport", Value: r.CPort}, {Key: "--mem", Value: r.Mem}, {Key: "--health-path", Value: r.HealthPath},
		{Key: "--image", Value: r.Image}, {Key: "--repo", Value: r.Repo},
	}
	var out []string
	for _, p := range pairs {
		if p.Value != "" {
			out = append(out, p.Key, p.Value)
		}
	}
	return out
}

func (r Request) conf() registry.AppConf {
	return registry.AppConf{Profile: r.Profile, Port: r.Port, HealthPath: r.HealthPath, Image: r.Image, Repo: r.Repo}
}

func (r Request) templateData() Request {
	if r.Profile == registry.ProfileBot {
		r.Port, r.CPort = "", ""
	}
	return r
}
