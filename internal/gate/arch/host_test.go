package arch

import (
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
	embedded "github.com/DreamsAreReal/deploy-hub/policy"
)

const (
	socketHead  = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	anyV6       = "00000000000000000000000000000000"
	runnerUnit  = "runner.service"
	runnerNetns = "/run/netns/runner"
	ciTemplate  = "ci@.service"
	ciNetns     = "/run/netns/ci-%i"
	netnsLine   = "NetworkNamespacePath="
	shipUnit    = "ship.service"
	dirUnit     = "dir.service"
	ciANetns    = "/run/netns/ci-a"
	hubUser     = "deploy"
	spareRoot   = "toor"
	ciAUnit     = "ci@a.service"
	viaDocker   = "group docker"
	ciBUnit     = "ci@b.service"
	viaSudo     = "group sudo"
	cloudUser   = "ubuntu"
)

func socketLine(local, remote, state string) string {
	return "   0: " + local + " " + remote + " " + state + " 00000000:00000000 00:00000000 00000000     0        0 1 1\n"
}

func TestListeningNamesPortsOpenBeyondLoopback(t *testing.T) {
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, "tcp"), socketHead+
		socketLine("00000000:0016", "00000000:0000", tcpListen)+
		"   1: 00000000:0050 00000000:0000 0A\n"+
		socketLine("0100007F:20EC", "00000000:0000", tcpListen)+
		socketLine("3500007F:0035", "00000000:0000", tcpListen)+
		socketLine("FD988C0D:2328", "00000000:0000", tcpListen)+
		socketLine("FD988C0D:01BB", "0A0B0C0D:D431", "01")+
		socketLine("ZZ:0050", "00000000:0000", tcpListen)+
		socketLine("00000000:XYZ", "00000000:0000", tcpListen)+
		socketLine("0000:0050", "00000000:0000", tcpListen)+
		"garbage\n")
	testutil.Write(t, filepath.Join(dir, "tcp6"), socketHead+
		socketLine(anyV6+":0016", anyV6+":0000", tcpListen)+
		socketLine("00000000000000000000000001000000:2382", anyV6+":0000", tcpListen)+
		socketLine("0000000000000000FFFF00000100007F:1F90", anyV6+":0000", tcpListen)+
		socketLine(anyV6+":0050", anyV6+":0000", "06"))
	testutil.Write(t, filepath.Join(dir, "udp"), socketHead+
		socketLine("00000000:CA6C", "00000000:0000", udpUnbound)+
		socketLine("00000000:1F40", "0A0B0C0D:0035", udpUnbound)+
		socketLine("00000000:1F41", "0A0B0C0D:0035", "01")+
		socketLine("3500007F:0035", "00000000:0000", udpUnbound))
	testutil.Write(t, filepath.Join(dir, "udp6"), socketHead+socketLine(anyV6+":01BB", anyV6+":0000", udpUnbound))
	ports, err := Listening(dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, ports, []int{22, 80, 443, 9000, 51820})
	for _, quiet := range []string{"", filepath.Join(dir, "missing")} {
		none, quietErr := Listening(quiet)
		testutil.NoErr(t, quietErr)
		testutil.Equal(t, none, []int(nil))
	}
	broken := t.TempDir()
	testutil.Mkdir(t, filepath.Join(broken, "udp"))
	_, err = Listening(broken)
	testutil.Fails(t, err)
}

func TestRootUsersNamesEveryWayToRoot(t *testing.T) {
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, "passwd"), "root:x:0:0:root:/root:/bin/bash\n"+
		"toor:x:0:100:spare root:/root:/bin/sh\n"+
		"deploy:x:1000:1000::/home/deploy:/bin/bash\n"+
		"builder:x:1001:988::/home/builder:/bin/sh\n"+
		"ghost:x:1002:4242::/home/ghost:/bin/sh\n"+
		"short:x\n"+
		"packer:x:1003:988::/home/packer:/bin/sh\n")
	testutil.Write(t, filepath.Join(dir, "group"), "root:x:0:\n"+
		"sudo:x:27:ubuntu,root,\n"+
		"docker:x:988:deploy,builder\n"+
		"users:x:100:\n"+
		"adm:x:4:syslog,ubuntu\n"+
		"lxd:x:105:ubuntu\n")
	grants, err := RootUsers(dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, grants, []Grant{
		{User: cloudUser, Via: viaSudo}, {User: hubUser, Via: viaDocker}, {User: "builder", Via: viaDocker},
		{User: cloudUser, Via: "group lxd"}, {User: spareRoot, Via: viaUID}, {User: "packer", Via: viaDocker},
	})
	none, err := RootUsers("")
	testutil.NoErr(t, err)
	testutil.Equal(t, none, []Grant(nil))
	for _, broken := range []string{"passwd", "group"} {
		bad := t.TempDir()
		testutil.Mkdir(t, filepath.Join(bad, broken))
		_, err := RootUsers(bad)
		testutil.Fails(t, err)
	}
}

func unitsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, runnerUnit), "[Service]\nUser=runner\n  "+netnsLine+runnerNetns+"\n")
	testutil.Write(t, filepath.Join(dir, runnerUnit+".d", "10-other.conf"), netnsLine+"/run/netns/other\n")
	testutil.Write(t, filepath.Join(dir, runnerUnit+".d", "20-reset.conf"), "[Service]\n"+netnsLine+" \n")
	testutil.Write(t, filepath.Join(dir, runnerUnit+".d", "notes.txt"), netnsLine+"/run/netns/ignored\n")
	testutil.Write(t, filepath.Join(dir, shipUnit), netnsLine+runnerNetns)
	testutil.Write(t, filepath.Join(dir, ciTemplate), "[Service]\n"+netnsLine+ciNetns+"\n")
	testutil.Write(t, filepath.Join(dir, ciTemplate+".d", "limits.conf"), "MemoryMax=1G\n")
	testutil.Write(t, filepath.Join(dir, "ci@a.service.d", "50-user.conf"), "User=ci-a\n")
	testutil.Write(t, filepath.Join(dir, "ci@b.service.d", "50-user.conf"), netnsLine+"\n")
	testutil.Write(t, filepath.Join(dir, "cd@a.service.d", "50-user.conf"), netnsLine+"/run/netns/cd\n")
	return dir
}

func TestNamespacesFollowDropInsAndInstances(t *testing.T) {
	dir := unitsDir(t)
	found, err := Namespaces(dir, []string{runnerUnit, shipUnit, ciTemplate, "missing.service", "missing@.service"})
	testutil.NoErr(t, err)
	testutil.Equal(t, found, map[string]string{
		runnerUnit: "", shipUnit: runnerNetns, ciTemplate: ciNetns, ciAUnit: ciANetns, ciBUnit: "",
	})
	none, err := Namespaces("", []string{runnerUnit})
	testutil.NoErr(t, err)
	testutil.Equal(t, none, map[string]string{})
	for unit, broken := range map[string]string{dirUnit: dirUnit, runnerUnit: filepath.Join(runnerUnit+".d", "30-dir.conf"), ciTemplate: filepath.Join("ci@c.service.d", "50-dir.conf")} {
		bad := unitsDir(t)
		testutil.Mkdir(t, filepath.Join(bad, broken))
		_, err := Namespaces(bad, []string{unit})
		testutil.Fails(t, err)
	}
}

func TestHostDirsGatherEveryFact(t *testing.T) {
	units, sites, net, accounts := unitsDir(t), t.TempDir(), t.TempDir(), t.TempDir()
	testutil.Write(t, filepath.Join(units, "tidy.timer"), "[Timer]\n")
	testutil.Write(t, filepath.Join(sites, "talk.caddy"), "talk {}\n")
	testutil.Write(t, filepath.Join(net, "tcp"), socketHead+socketLine("00000000:0016", "00000000:0000", tcpListen))
	testutil.Write(t, filepath.Join(accounts, "group"), "docker:x:988:deploy\n")
	dirs := HostDirs{Units: units, Sites: sites, Net: net, Accounts: accounts}
	facts, err := dirs.Facts([]string{shipUnit})
	testutil.NoErr(t, err)
	testutil.Equal(t, facts, Facts{Timers: []string{"tidy.timer"}, Sites: []string{"talk"}, Listening: []int{22},
		Grants: []Grant{{User: hubUser, Via: viaDocker}}, Namespaces: map[string]string{shipUnit: runnerNetns}})
	testutil.Mkdir(t, filepath.Join(units, dirUnit))
	for _, broken := range []HostDirs{
		{Units: filepath.Join(units, "tidy.timer")},
		{Units: units, Net: filepath.Join(net, "tcp")},
		{Units: units, Accounts: filepath.Join(accounts, "group")},
		{Units: units},
	} {
		_, err := broken.Facts([]string{dirUnit})
		testutil.Fails(t, err)
	}
}

func TestHostDriftNamesPortsAdminsAndNamespaces(t *testing.T) {
	catalog := Catalog{Admins: []Admin{{User: hubUser, Why: "docker"}}, Services: map[string]Service{
		"caddy":  {Exposes: []Expose{{Port: 443, Public: true}, {Port: 8080}, {Site: "app"}}},
		"runner": {Isolated: []Isolation{{Unit: runnerUnit, Netns: runnerNetns}}},
		"hub":    {Isolated: []Isolation{{Unit: ciTemplate, Netns: ciNetns}}},
	}}
	testutil.Equal(t, catalog.IsolatedUnits(), []string{ciTemplate, runnerUnit})
	drift := catalog.HostDrift(Facts{
		Listening: []int{443, 8080},
		Grants:    []Grant{{User: hubUser, Via: viaDocker}, {User: "eve", Via: viaSudo}, {User: spareRoot, Via: viaUID}},
		Namespaces: map[string]string{runnerUnit: "", ciTemplate: ciNetns, ciAUnit: ciANetns,
			ciBUnit: "/run/netns/other", "cd@a.service": "", "other.service": ""},
	})
	testutil.Equal(t, drift, []string{
		"port 8080 listens beyond loopback on the host, and no service declares it public in exposes",
		"user eve can act as root through group sudo, and admins in the catalog does not list it",
		"user toor can act as root through uid 0, and admins in the catalog does not list it",
		"unit ci@b.service runs in /run/netns/other, and the catalog declares /run/netns/ci-b",
		"unit runner.service runs in the host network namespace, and the catalog declares /run/netns/runner",
	})
	clean := catalog.HostDrift(Facts{Listening: []int{443}, Grants: []Grant{{User: hubUser, Via: viaDocker}}, Namespaces: map[string]string{runnerUnit: runnerNetns}})
	testutil.Equal(t, clean, []string(nil))
}

func TestHostEntriesNeedAReason(t *testing.T) {
	catalog := mustParse(t, "admins: [{user: deploy, why: docker}, {user: eve}, {why: nobody}]\nservices:\n  app:\n    critical: normal\n"+
		"    exposes: [{port: 22, public: true}, {port: 443, public: true, why: web}, {port: 9000}]\n"+
		"    isolated: [{unit: a.service, netns: /run/netns/a, why: sandbox}, {unit: b.service, why: sandbox}, {unit: c.service, netns: /run/netns/c}, {netns: /run/netns/d, why: sandbox}]\n")
	testutil.Equal(t, catalog.adminProblems(), []string{
		`admin "eve" without a user or a why: root on the server needs a reason`,
		`admin "" without a user or a why: root on the server needs a reason`,
	})
	testutil.Equal(t, catalog.Problems(appName, day), []string{
		"app: public port 22 without a why: a port open to the internet needs a reason",
		`app: isolated unit "b.service" needs a unit, a netns and a why`,
		`app: isolated unit "c.service" needs a unit, a netns and a why`,
		`app: isolated unit "" needs a unit, a netns and a why`,
	})
}

func TestTheShippedCatalogDeclaresTheHubNamespacesAndAdmins(t *testing.T) {
	catalog, err := Parse(embedded.Architecture())
	testutil.NoErr(t, err)
	testutil.Equal(t, catalog.adminProblems(), []string(nil))
	found, err := Namespaces(filepath.Join("..", "..", "..", "server", "ci-runner"), catalog.IsolatedUnits())
	testutil.NoErr(t, err)
	testutil.Equal(t, len(found), 3)
	testutil.Equal(t, catalog.HostDrift(Facts{Namespaces: found}), []string(nil))
}
