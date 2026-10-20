package hub

import "path/filepath"

const (
	DefaultDir    = "/opt/deploy-hub"
	BinaryName    = "deploy-hub"
	RegistryHost  = "ghcr.io"
	appsListFile  = "apps.list"
	journalFile   = "deploys.log"
	lockFile      = "deploy.lock"
	smokeFile     = "smoke.conf"
	lastErrorFile = "last-error.log"
	hubConfFile   = "hub.conf"
	binDir        = "bin"
	logsDir       = "logs"
	logExt        = ".log"
)

type Paths struct {
	Dir string
}

func (p Paths) AppsList() string  { return filepath.Join(p.Dir, appsListFile) }
func (p Paths) Journal() string   { return filepath.Join(p.Dir, journalFile) }
func (p Paths) Lock() string      { return filepath.Join(p.Dir, lockFile) }
func (p Paths) Smoke() string     { return filepath.Join(p.Dir, smokeFile) }
func (p Paths) LastError() string { return filepath.Join(p.Dir, lastErrorFile) }
func (p Paths) Binary() string    { return filepath.Join(p.Dir, binDir, BinaryName) }
func (p Paths) HubConf() string   { return filepath.Join(p.Dir, hubConfFile) }
func (p Paths) AppsRoot() string  { return filepath.Dir(p.Dir) }

func (p Paths) LogFile(command string) string {
	return filepath.Join(p.Dir, logsDir, command+logExt)
}
