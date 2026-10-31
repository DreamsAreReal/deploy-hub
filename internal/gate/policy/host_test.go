package policy

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	root       = "root"
	deployUser = "deploy"
	obsTimer   = "obs.timer"
	obsUnit    = "obs.service"
	unitPath   = "/etc/systemd/system/obs.service"
	timerPath  = "/etc/systemd/system/obs.timer"
	runnerUser = "runner"
)

func goodHost() *Host {
	return &Host{
		Files: []HostFile{
			{Src: "obs.py", Dst: "/usr/local/sbin/obs", Owner: root, Group: root, Mode: "0755"},
			{Src: "units/obs.service", Dst: unitPath, Owner: root, Group: root, Mode: "0644"},
			{Src: "units/obs.timer", Dst: timerPath, Owner: root, Group: root, Mode: "0644"},
			{Src: "ingest.caddy", Dst: "/etc/caddy/extra/obs.caddy", Owner: root, Group: "caddy", Mode: "0640", Env: "ingest.env"},
		},
		Dirs:   []HostDir{{Path: "/var/lib/node_exporter/textfile", Owner: root, Group: root, Mode: "0755"}},
		Timers: []string{obsTimer},
		Checks: []string{obsUnit},
	}
}

func TestHostSpecInsideTheAllowedRootsIsValid(t *testing.T) {
	testutil.NoErr(t, validateHost("obs", goodHost()))
	var none *Host
	testutil.NoErr(t, validateHost("obs", none))
	testutil.Equal(t, len(goodHost().Units()), 2)
}

func TestHostSpecRefusesPathsOutsideTheList(t *testing.T) {
	cases := map[string]func(h *Host){
		"sudoers":               func(h *Host) { h.Files[0].Dst = "/etc/sudoers.d/deploy" },
		"authorized keys":       func(h *Host) { h.Files[0].Dst = "/root/.ssh/authorized_keys" },
		"nested below a root":   func(h *Host) { h.Files[0].Dst = "/usr/local/sbin/sub/obs" },
		"parent traversal":      func(h *Host) { h.Files[0].Dst = "/usr/local/sbin/../../../etc/passwd" },
		"root itself":           func(h *Host) { h.Files[0].Dst = "/usr/local/sbin/" },
		"hidden name":           func(h *Host) { h.Files[0].Dst = "/usr/local/sbin/.obs" },
		"relative destination":  func(h *Host) { h.Files[0].Dst = "usr/local/sbin/obs" },
		"absolute source":       func(h *Host) { h.Files[0].Src = "/etc/shadow" },
		"source escapes":        func(h *Host) { h.Files[0].Src = "../obs.py" },
		"unclean source":        func(h *Host) { h.Files[0].Src = "units//obs.py" },
		"empty source":          func(h *Host) { h.Files[0].Src = "" },
		"setuid":                func(h *Host) { h.Files[0].Mode = "4755" },
		"group writable":        func(h *Host) { h.Files[0].Mode = "0775" },
		"world writable":        func(h *Host) { h.Files[0].Mode = "0777" },
		"three digit mode":      func(h *Host) { h.Files[0].Mode = "755" },
		"bad owner":             func(h *Host) { h.Files[0].Owner = "Root" },
		"bad group":             func(h *Host) { h.Files[0].Group = "" },
		"env path":              func(h *Host) { h.Files[3].Env = "../secrets.env" },
		"env not an env file":   func(h *Host) { h.Files[3].Env = "app.conf" },
		"src listed twice":      func(h *Host) { h.Files[1].Src = h.Files[0].Src },
		"same destination":      func(h *Host) { h.Files[1].Dst = h.Files[0].Dst },
		"dir outside roots":     func(h *Host) { h.Dirs[0].Path = "/etc/cron.d" },
		"dir mode":              func(h *Host) { h.Dirs[0].Mode = "1777" },
		"dir owner":             func(h *Host) { h.Dirs[0].Owner = "-x" },
		"foreign timer":         func(h *Host) { h.Timers = []string{"apt-daily.timer"} },
		"service as timer":      func(h *Host) { h.Timers = []string{obsUnit} },
		"foreign check":         func(h *Host) { h.Checks = []string{"ssh.service"} },
		"timer as check":        func(h *Host) { h.Checks = []string{obsTimer} },
		"no files":              func(h *Host) { h.Files = nil; h.Timers = nil; h.Checks = nil },
		"unit outside unit dir": func(h *Host) { h.Files[1].Dst = "/usr/local/sbin/obs.service" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := goodHost()
			mutate(h)
			testutil.True(t, errors.Is(validateHost("obs", h), ErrHost), "want ErrHost")
		})
	}
}

func TestParseModeAllowsOnlyPlainModes(t *testing.T) {
	for mode, want := range map[string]uint32{"0755": 0o755, "0644": 0o644, "0640": 0o640, "0700": 0o700, "0000": 0} {
		got, err := ParseMode(mode)
		testutil.NoErr(t, err)
		testutil.Equal(t, got, want)
	}
	for _, bad := range []string{"0756", "0765", "2755", "0855", "", "0o755", "07555"} {
		_, err := ParseMode(bad)
		testutil.Is(t, err, ErrHost)
	}
}

func TestHostSpecIsPartOfTheAppPolicy(t *testing.T) {
	spec := `
    host:
      files:
        - {src: obs.py, dst: /usr/local/sbin/obs, owner: root, group: root, mode: "0755"}
`
	p, err := Parse([]byte(valid + spec))
	testutil.NoErr(t, err)
	app, err := p.App("bot")
	testutil.NoErr(t, err)
	testutil.Equal(t, app.Host.Files[0].Dst, "/usr/local/sbin/obs")
	_, err = Parse([]byte(valid + `
    host:
      files:
        - {src: obs.py, dst: /etc/passwd, owner: root, group: root, mode: "0644"}
`))
	testutil.Is(t, err, ErrHost)
}

func TestHubSudoCommandsComeFromAFixedList(t *testing.T) {
	testutil.NoErr(t, Hub{}.validate())
	testutil.NoErr(t, Hub{SudoUser: deployUser, SudoCommands: []string{SudoCaddySync, SudoHostSync, SudoSelfUpdate}}.validate())
	for name, h := range map[string]Hub{
		"root":         {SudoUser: root, SudoCommands: []string{SudoCaddySync}},
		"no user":      {SudoCommands: []string{SudoCaddySync}},
		"bad user":     {SudoUser: "Deploy", SudoCommands: []string{SudoCaddySync}},
		"any command":  {SudoUser: deployUser, SudoCommands: []string{"prune"}},
		"shell":        {SudoUser: deployUser, SudoCommands: []string{"/bin/sh"}},
		"listed twice": {SudoUser: deployUser, SudoCommands: []string{SudoHostSync, SudoHostSync}},
	} {
		testutil.True(t, errors.Is(h.validate(), ErrHub), name)
	}
}

func TestHostKindNeedsAHostSpec(t *testing.T) {
	spec := `
  ops:
    repo: Owner/ops
    kind: host
`
	_, err := Parse([]byte(valid + spec))
	testutil.Is(t, err, ErrPolicy)
	testutil.True(t, err != nil && strings.Contains(err.Error(), "kind host needs a host spec"), "names the missing spec")
	p, err := Parse([]byte(valid + spec + `    host:
      files:
        - {src: ops, dst: /usr/local/sbin/ops, owner: root, group: root, mode: "0755"}
`))
	testutil.NoErr(t, err)
	app, err := p.App("ops")
	testutil.NoErr(t, err)
	testutil.True(t, app.HostOnly() && app.HasImage() && !app.HasHTTP(), "a host app ships an image, has no HTTP surface")
	bot, _ := p.App("bot")
	testutil.True(t, !bot.HostOnly(), "a worker runs a container")
}

func runnerHost() *Host {
	return &Host{
		Files:   []HostFile{{Src: runnerUser, Dst: RunnerDir + runnerUser, Owner: runnerUser, Group: runnerUser, Mode: "0755", KeepPrev: true}},
		Restart: &HostRestart{Unit: "runner.service", Busy: "tasks_runner_active_runs", Version: "tasks_runner_info", Within: 2 * time.Minute},
	}
}

func TestRestartSpecKeepsThePreviousBinary(t *testing.T) {
	testutil.NoErr(t, validateHost("tasks", runnerHost()))
	testutil.Equal(t, len(runnerHost().Kept()), 1)
	testutil.Equal(t, len(goodHost().Kept()), 0)
}

func TestRestartSpecRefusesLooseValues(t *testing.T) {
	cases := map[string]func(h *Host){
		"no restart":          func(h *Host) { h.Restart = nil },
		"nothing kept":        func(h *Host) { h.Files[0].KeepPrev = false },
		"timer unit":          func(h *Host) { h.Restart.Unit = "runner.timer" },
		"unit path":           func(h *Host) { h.Restart.Unit = "../runner.service" },
		"query as busy":       func(h *Host) { h.Restart.Busy = "sum(tasks_runner_active_runs)" },
		"selector as version": func(h *Host) { h.Restart.Version = `tasks_runner_info{version="x"}` },
		"no wait":             func(h *Host) { h.Restart.Within = 0 },
		"endless wait":        func(h *Host) { h.Restart.Within = time.Hour },
		"nested runner path":  func(h *Host) { h.Files[0].Dst = RunnerDir + "bin/runner" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := runnerHost()
			mutate(h)
			testutil.Is(t, validateHost("tasks", h), ErrHost)
		})
	}
}

func TestRestartSpecParsesFromThePolicy(t *testing.T) {
	p, err := Parse([]byte(valid + `
    host:
      files:
        - {src: runner, dst: /home/runner/runner/runner, owner: runner, group: runner, mode: "0755", keep_prev: true}
      restart: {unit: runner.service, busy: tasks_runner_active_runs, version: tasks_runner_info, within: 2m}
`))
	testutil.NoErr(t, err)
	app, err := p.App("bot")
	testutil.NoErr(t, err)
	testutil.Equal(t, app.Host.Restart.Within, 2*time.Minute)
	testutil.True(t, app.Host.Files[0].KeepPrev, "keep_prev parsed")
}
