package provision

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/netprobe"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	flagTrue  = "1"
	flagFalse = "0"
)

var ProcNetFiles = []string{"/proc/net/tcp", "/proc/net/tcp6"}

type Facts struct {
	Dir           string
	InList        bool
	DirExists     bool
	ConfExists    bool
	ComposeExists bool
	EnvExists     bool
	Port          string
	PortsTaken    []int
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func Gather(apps registry.Apps, appsRoot, app string, procFiles []string) Facts {
	_, lookupErr := apps.Lookup(app)
	dir := apps.DirFor(app)
	f := Facts{
		Dir:           dir,
		InList:        lookupErr == nil,
		DirExists:     exists(dir),
		ConfExists:    exists(filepath.Join(dir, registry.AppConfFile)),
		ComposeExists: exists(filepath.Join(dir, registry.ComposeFile)),
		EnvExists:     exists(filepath.Join(dir, registry.EnvFile)),
	}
	if conf, err := registry.LoadAppConf(dir); err == nil {
		f.Port = conf.Port
	}
	f.PortsTaken = takenPorts(apps, appsRoot, procFiles)
	return f
}

func appDirs(apps registry.Apps, appsRoot string) []string {
	confs, _ := filepath.Glob(filepath.Join(appsRoot, "*", registry.AppConfFile))
	dirs := make([]string, 0, len(confs))
	for _, c := range confs {
		dirs = append(dirs, filepath.Dir(c))
	}
	entries, _ := apps.All()
	for _, e := range entries {
		dirs = append(dirs, e.Dir)
	}
	return dirs
}

func confPort(dir string) (int, bool) {
	conf, err := registry.LoadAppConf(dir)
	if err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(conf.Port)
	return p, err == nil
}

func takenPorts(apps registry.Apps, appsRoot string, procFiles []string) []int {
	set := map[int]bool{}
	for _, p := range netprobe.ListeningPorts(procFiles) {
		set[p] = true
	}
	for _, dir := range appDirs(apps, appsRoot) {
		if p, ok := confPort(dir); ok {
			set[p] = true
		}
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func flag(v bool) string {
	if v {
		return flagTrue
	}
	return flagFalse
}

func (f Facts) Encode() string {
	ports := make([]string, 0, len(f.PortsTaken))
	for _, p := range f.PortsTaken {
		ports = append(ports, strconv.Itoa(p))
	}
	return registry.EncodeKV([]registry.KV{
		{Key: "dir", Value: f.Dir},
		{Key: "in_list", Value: flag(f.InList)},
		{Key: "dir_exists", Value: flag(f.DirExists)},
		{Key: "conf_exists", Value: flag(f.ConfExists)},
		{Key: "compose_exists", Value: flag(f.ComposeExists)},
		{Key: "env_exists", Value: flag(f.EnvExists)},
		{Key: "port", Value: f.Port},
		{Key: "ports_taken", Value: strings.Join(ports, " ")},
	})
}

func ParseFacts(text string) Facts {
	kv := registry.ParseKV(text)
	f := Facts{
		Dir:           kv["dir"],
		InList:        kv["in_list"] == flagTrue,
		DirExists:     kv["dir_exists"] == flagTrue,
		ConfExists:    kv["conf_exists"] == flagTrue,
		ComposeExists: kv["compose_exists"] == flagTrue,
		EnvExists:     kv["env_exists"] == flagTrue,
		Port:          kv["port"],
	}
	for _, tok := range strings.Fields(kv["ports_taken"]) {
		if p, err := strconv.Atoi(tok); err == nil {
			f.PortsTaken = append(f.PortsTaken, p)
		}
	}
	return f
}
