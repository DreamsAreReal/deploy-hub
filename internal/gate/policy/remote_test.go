package policy

import (
	"errors"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	initPath  = "/etc/init.d/routerbot"
	agentPath = "/usr/bin/routerbot-agent"
	initName  = "routerbot"
)

func goodRemote() *Remote {
	return &Remote{
		Target: "ax23",
		Files: []RemoteFile{
			{Src: "agent/routerbot-agent", Dst: agentPath, Mode: "0755"},
			{Src: "agent/routerbot.init", Dst: initPath, Mode: "0755"},
			{Src: "router/wgru-watchdog", Dst: "/usr/bin/wgru-watchdog", Mode: "0755"},
			{Src: "router/uci.d/010-dns.sh", Dst: "/etc/routerbot/uci.d/010-dns.sh", Mode: "0644"},
		},
		Restart: []string{initName},
	}
}

func TestRemoteSpecUnderTheRouterRootsIsValid(t *testing.T) {
	testutil.NoErr(t, validateRemote("router-bot", goodRemote()))
	var none *Remote
	testutil.NoErr(t, validateRemote("router-bot", none))
	testutil.Equal(t, goodRemote().Inits(), []string{initName})
}

func TestRemoteSpecRefusesWhatTheRouterWouldRefuse(t *testing.T) {
	cases := map[string]func(r *Remote){
		"dropbear keys":         func(r *Remote) { r.Files[0].Dst = "/etc/dropbear/authorized_keys" },
		"other init script":     func(r *Remote) { r.Files[1].Dst = "/etc/init.d/firewall" },
		"prefix alone":          func(r *Remote) { r.Files[0].Dst = "/usr/bin/routerbot-" },
		"nested under prefix":   func(r *Remote) { r.Files[0].Dst = "/usr/bin/routerbot-x/y" },
		"exact root extended":   func(r *Remote) { r.Files[2].Dst = "/usr/bin/wgru-watchdog2" },
		"directory root alone":  func(r *Remote) { r.Files[3].Dst = "/etc/routerbot/uci.d/" },
		"traversal":             func(r *Remote) { r.Files[3].Dst = "/etc/routerbot/uci.d/../../shadow" },
		"rollback store":        func(r *Remote) { r.Files[3].Dst = "/etc/routerbot/rollback/pending" },
		"remote source escapes": func(r *Remote) { r.Files[0].Src = "../x" },
		"remote group writable": func(r *Remote) { r.Files[0].Mode = "0775" },
		"remote setuid":         func(r *Remote) { r.Files[0].Mode = "4755" },
		"remote listed twice":   func(r *Remote) { r.Files[2].Dst = agentPath },
		"remote without files":  func(r *Remote) { r.Files = nil; r.Restart = nil },
		"bad target":            func(r *Remote) { r.Target = "AX 23" },
		"restart not shipped":   func(r *Remote) { r.Restart = []string{"firewall"} },
	}
	for name, mutate := range cases {
		r := goodRemote()
		mutate(r)
		err := validateRemote("router-bot", r)
		testutil.True(t, errors.Is(err, ErrRemote) || errors.Is(err, ErrHost), name)
	}
}

func TestRemoteSpecIsReadFromThePolicy(t *testing.T) {
	p, err := Parse([]byte(valid + "    remote:\n      target: ax23\n      files:\n        - {src: agent/routerbot-agent, dst: /usr/bin/routerbot-agent, mode: \"0755\"}\n"))
	testutil.NoErr(t, err)
	a, err := p.App("bot")
	testutil.NoErr(t, err)
	testutil.Equal(t, a.Remote.Files[0].Dst, agentPath)
	_, err = Parse([]byte(valid + "    remote:\n      target: ax23\n"))
	testutil.True(t, errors.Is(err, ErrRemote), "an empty remote spec is refused")
}
