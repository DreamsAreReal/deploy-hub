package registry

import (
	"path/filepath"
	"strings"
)

const (
	AppConfFile   = "app.conf"
	StateFile     = ".deploy-state"
	ComposeFile   = "docker-compose.yml"
	EnvFile       = ".env"
	CaddyPortFile = ".caddy-port"

	ModeBYOC = "byoc"

	ProfileStatic  = "static"
	ProfileService = "service"
	ProfileBot     = "bot"

	HealthHTTP    = "http"
	HealthCompose = "compose"
	HealthProcess = "process"

	DefaultHealthPath = "/"

	TagPrefix   = "sha-"
	shortShaLen = 7
)

type AppConf struct {
	Mode       string
	Main       string
	Profile    string
	Port       string
	HealthPath string
	HMode      string
	Image      string
	Repo       string
}

func LoadAppConf(dir string) (AppConf, error) {
	kv, err := ReadKV(filepath.Join(dir, AppConfFile))
	if err != nil {
		return AppConf{}, err
	}
	return AppConf{
		Mode:       kv["mode"],
		Main:       kv["main"],
		Profile:    kv["profile"],
		Port:       kv["port"],
		HealthPath: kv["health_path"],
		HMode:      kv["hmode"],
		Image:      kv["image"],
		Repo:       kv["repo"],
	}, nil
}

func (c AppConf) Encode() string {
	return EncodeKV([]KV{
		{"mode", c.Mode},
		{"main", c.Main},
		{"profile", c.Profile},
		{"port", c.Port},
		{"health_path", c.HealthPath},
		{"hmode", c.HMode},
		{"image", c.Image},
		{"repo", c.Repo},
	})
}

func (c AppConf) Save(dir string) error {
	return WriteFile(filepath.Join(dir, AppConfFile), c.Encode())
}

func (c AppConf) IsBYOC() bool {
	return c.Mode == ModeBYOC
}

func (c AppConf) HealthMode() string {
	if c.IsBYOC() {
		return c.HMode
	}
	return NormalizeHealthMode(c.Profile)
}

func NormalizeHealthMode(v string) string {
	switch v {
	case ProfileStatic, ProfileService:
		return HealthHTTP
	case ProfileBot:
		return HealthCompose
	default:
		return v
	}
}

type DeployState struct {
	Current  string
	Previous string
}

func LoadState(dir string) DeployState {
	kv, err := ReadKV(filepath.Join(dir, StateFile))
	if err != nil {
		return DeployState{}
	}
	return DeployState{Current: kv["current"], Previous: kv["previous"]}
}

func (s DeployState) Save(dir string) error {
	content := "current=" + s.Current + "\nprevious=" + s.Previous + "\n"
	return WriteFile(filepath.Join(dir, StateFile), content)
}

func Sha7(tag string) string {
	v := strings.TrimPrefix(tag, TagPrefix)
	if len(v) > shortShaLen {
		return v[:shortShaLen]
	}
	return v
}
