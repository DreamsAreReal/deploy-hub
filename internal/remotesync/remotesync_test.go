package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/gatedimage"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	app       = "router-bot"
	tag       = "sha-abcdef1234567"
	sha7      = "abcdef1"
	image     = "ghcr.io/owner/router-bot"
	digest    = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	primary   = "ax23"
	fallback  = "ax23-oob"
	target    = "ax23"
	agentSrc  = "agent/routerbot-agent"
	applySrc  = "agent/routerbot-apply"
	initSrc   = "agent/routerbot.init"
	agentDst  = "/usr/bin/routerbot-agent"
	initDst   = "/etc/init.d/routerbot"
	relDir    = "/tmp/dh-release/" + sha7
	engine    = "sh " + relDir + "/root/usr/bin/routerbot-apply"
	gateOK    = "[2026-10-23T00:00:00Z] router-bot@abcdef1 gate ok 60s\n"
	dfHeader  = "Filesystem 1K-blocks Used Available Use% Mounted on\n"
	dfOK      = dfHeader + "/dev/mtdblock4 6656 2428 4228 36% /overlay\ntmpfs 59384 6960 52424 12% /tmp\n"
	pending   = "pending: 1000 release " + relDir
	confirmed = "confirmed"
	plainApp  = "plain"
)

type router struct {
	down       map[string]bool
	busy       bool
	df         string
	backup     string
	applyOut   string
	applyFails bool
	healthyAt  int
	statuses   int
	result     string
	confirmErr bool
	uploaded   []byte
	commands   []string
}

func (r *router) handle(c procexec.Command) (procexec.Result, error) {
	host, remote := c.Args[len(c.Args)-2], c.Args[len(c.Args)-1]
	if r.down[host] {
		return procexec.Result{Stderr: "ssh: connect to host " + host + ": timed out"}, errors.New("exit status 255")
	}
	r.commands = append(r.commands, host+": "+remote)
	switch {
	case remote == reachCommand:
		return procexec.Result{}, nil
	case strings.Contains(remote, " status; df -k"):
		return r.preflight()
	case strings.HasPrefix(remote, "sysupgrade -b"):
		return procexec.Result{Stdout: r.backup}, nil
	case strings.HasPrefix(remote, "rm -rf "+relDir+" && "):
		r.uploaded = []byte(c.Stdin)
		return procexec.Result{}, nil
	case strings.HasSuffix(remote, "--release "+relDir):
		return r.apply()
	}
	return r.answer(remote)
}

func (r *router) preflight() (procexec.Result, error) {
	out := r.df
	if r.busy {
		out = "pending: 900 manual /tmp/x.sh\n" + out
	}
	return procexec.Result{Stdout: out}, nil
}

func (r *router) apply() (procexec.Result, error) {
	if r.applyFails {
		return procexec.Result{Stdout: "migration 010.sh\nrolled back (script failed): dhcp\n"}, errors.New("exit status 1")
	}
	return procexec.Result{Stdout: r.applyOut}, nil
}

func (r *router) answer(remote string) (procexec.Result, error) {
	switch {
	case strings.HasSuffix(remote, " status"):
		return r.status()
	case strings.HasSuffix(remote, " confirm"):
		if r.confirmErr {
			r.down[primary], r.down[fallback] = true, true
			return procexec.Result{}, errors.New("exit status 255")
		}
		r.result = confirmed
		return procexec.Result{Stdout: confirmed + "\n"}, nil
	case strings.HasSuffix(remote, " rollback"):
		r.result = "rolled back (asked): /usr/bin/routerbot-agent"
		return procexec.Result{Stdout: r.result + "\n"}, nil
	case strings.HasPrefix(remote, "rm -rf "+relDir):
		return procexec.Result{}, nil
	}
	return procexec.Result{}, errors.New("unexpected remote command: " + remote)
}

func (r *router) status() (procexec.Result, error) {
	r.statuses++
	if r.result != "" {
		return procexec.Result{Stdout: r.result + "\n"}, nil
	}
	if r.healthyAt > 0 && r.statuses >= r.healthyAt {
		return procexec.Result{Stdout: pending + " (healthy)\n"}, nil
	}
	return procexec.Result{Stdout: pending + "\n"}, nil
}

type fixture struct {
	syncer  Syncer
	courier Courier
	router  *router
	exec    *procexectest.Fake
	script  procexectest.Script
	files   map[string]string
	base    string
	slept   time.Duration
	spec    policy.Remote
	targets string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{base: base, targets: filepath.Join(base, "remotes")}
	f.router = &router{down: map[string]bool{}, df: dfOK, backup: "\x1f\x8bbackup", applyOut: "migration 010.sh\napplied; run 'routerbot-apply confirm' within 180s or it rolls back\n", healthyAt: 3}
	f.files = map[string]string{agentSrc: "#!/bin/sh\necho agent\n", applySrc: "#!/bin/sh\necho apply\n", initSrc: "#!/bin/sh /etc/rc.common\n"}
	f.spec = policy.Remote{Target: target, Restart: []string{"routerbot"}, Files: []policy.RemoteFile{
		{Src: agentSrc, Dst: agentDst, Mode: "0755"},
		{Src: applySrc, Dst: InstalledEngine, Mode: "0755"},
		{Src: initSrc, Dst: initDst, Mode: "0755"},
	}}
	appDir := filepath.Join(base, "opt", app)
	testutil.Write(t, filepath.Join(appDir, "app.conf"), "image="+image+"\n")
	testutil.Write(t, filepath.Join(base, "apps.list"), app+" "+appDir+"\n")
	testutil.Write(t, filepath.Join(base, "deploys.log"), gateOK)
	testutil.Write(t, filepath.Join(f.targets, target, confFile), "hosts="+primary+" "+fallback+"\n")
	testutil.Write(t, filepath.Join(f.targets, target, sshConfigFile), "Host ax23\n")
	f.script = procexectest.Script{
		"docker image inspect": {Res: procexec.Result{Stdout: `["` + image + `@` + digest + `"]` + "\n"}},
		"docker create":        {Res: procexec.Result{Stdout: "cid\n"}},
		"docker cp":            {Run: func(c procexec.Command) { f.populate(t, c.Args[len(c.Args)-1]) }},
		"docker rm":            {},
	}
	f.exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		if c.Name == SSHBinary {
			return f.router.handle(c)
		}
		return f.script.Handle(c)
	}}
	now := func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }
	f.courier = Courier{
		Exec: f.exec, Dir: f.targets, Backups: filepath.Join(base, "backups"), Now: now, Log: slog.New(slog.DiscardHandler),
		HealthWait: 30 * time.Second, VerdictWait: 20 * time.Second, Poll: 5 * time.Second,
		Sleep: func(d time.Duration) { f.slept += d },
	}
	f.syncer = Syncer{Exec: f.exec, Now: now, Out: console.New(&bytes.Buffer{}), Log: slog.New(slog.DiscardHandler)}
	f.syncer.Apps.Path, f.syncer.Apps.Root = filepath.Join(base, "apps.list"), base
	f.syncer.Journal = journal.Journal{Path: filepath.Join(base, "deploys.log"), Now: f.syncer.Now}
	return f
}

func (f *fixture) populate(t *testing.T, dir string) {
	t.Helper()
	for rel, body := range f.files {
		testutil.Write(t, filepath.Join(dir, rel), body)
	}
}

func (f *fixture) run(t *testing.T) error {
	t.Helper()
	f.syncer.Policy = policy.Policy{Apps: map[string]policy.App{app: {Remote: &f.spec}, plainApp: {}}}
	f.syncer.Courier = f.courier
	return f.syncer.Run(context.Background(), app, tag)
}

func (f *fixture) lastJournal(t *testing.T) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(testutil.Read(t, f.syncer.Journal.Path)), "\n")
	return lines[len(lines)-1]
}

func (f *fixture) sent(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	r := tar.NewReader(bytes.NewReader(f.router.uploaded))
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		testutil.NoErr(t, err)
		data, _ := io.ReadAll(r)
		out[h.Name] = fmt.Sprintf("%o %s", h.Mode, data)
	}
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestAReleaseIsBackedUpSentAppliedAndConfirmedOverANewConnection(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.run(t))
	sent := f.sent(t)
	testutil.Equal(t, sent["root/usr/bin/routerbot-agent"], "755 #!/bin/sh\necho agent\n")
	testutil.Equal(t, sent["MANIFEST"], "644 "+sum(f.files[agentSrc])+" 0755 "+agentDst+"\n"+
		sum(f.files[applySrc])+" 0755 "+InstalledEngine+"\n"+sum(f.files[initSrc])+" 0755 "+initDst+"\n")
	testutil.Equal(t, sent["RESTART"], "644 routerbot\n")
	testutil.Equal(t, sent["root/usr/"], "755 ")
	backups, _ := filepath.Glob(filepath.Join(f.courier.Backups, target, "*.tar.gz"))
	testutil.Equal(t, backups, []string{filepath.Join(f.courier.Backups, target, "20261028T120000Z-"+sha7+".tar.gz")})
	testutil.Equal(t, testutil.Read(t, backups[0]), "\x1f\x8bbackup")
	testutil.Perm(t, backups[0], backupPerm)
	cmds := strings.Join(f.router.commands, "\n")
	testutil.Contains(t, cmds, "ax23: "+engine+" --release "+relDir, "ax23: "+engine+" confirm")
	testutil.True(t, slices.Contains(f.router.commands, "ax23: rm -rf "+relDir), "the release directory is removed")
	testutil.Equal(t, f.router.statuses, 3)
	testutil.Equal(t, f.slept, 15*time.Second)
	testutil.Contains(t, f.lastJournal(t), "router-bot@abcdef1 remote-sync ok", "target=ax23 via=ax23: confirmed")
	call, _ := f.exec.Find("--release")
	testutil.Equal(t, call.Args[:2], []string{"-F", filepath.Join(f.targets, target, sshConfigFile)})
	testutil.Contains(t, strings.Join(call.Args, " "), "BatchMode=yes", "ControlMaster=no", "ControlPath=none", "StrictHostKeyChecking=yes")
}

func TestTheFallbackPathIsUsedWhenThePrimaryIsDown(t *testing.T) {
	f := newFixture(t)
	f.router.down[primary] = true
	testutil.NoErr(t, f.run(t))
	testutil.Contains(t, f.lastJournal(t), "via=ax23-oob: confirmed")
}

func TestAnUnreachableTargetChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.router.down[primary], f.router.down[fallback] = true, true
	err := f.run(t)
	testutil.True(t, errors.Is(err, ErrUnreachable), "both paths down")
	testutil.Contains(t, err.Error(), "ax23: ", "ax23-oob: ")
	testutil.Contains(t, f.lastJournal(t), "remote-sync fail", "via=-:")
	testutil.Equal(t, len(f.router.uploaded), 0)
}

func TestPreflightRefusesABusyOrFullTarget(t *testing.T) {
	cases := map[string]struct {
		set  func(r *router)
		want error
	}{
		"pending change": {func(r *router) { r.busy = true }, ErrBusy},
		"overlay full": {func(r *router) {
			r.df = dfHeader + "/dev/mtdblock4 6656 6400 256 96% /overlay\ntmpfs 59384 6960 52424 12% /tmp\n"
		}, ErrNoSpace},
		"tmp full": {func(r *router) {
			r.df = dfHeader + "/dev/mtdblock4 6656 2428 4228 36% /overlay\ntmpfs 59384 58000 1384 98% /tmp\n"
		}, ErrNoSpace},
		"df unreadable":    {func(r *router) { r.df = "df: no\n" }, ErrNoSpace},
		"backup not a tar": {func(r *router) { r.backup = "sysupgrade: failed" }, ErrBackup},
		"apply refused":    {func(r *router) { r.applyFails = true }, ErrApply},
	}
	for name, tc := range cases {
		f := newFixture(t)
		tc.set(f.router)
		err := f.run(t)
		testutil.True(t, errors.Is(err, tc.want), name+": "+fmt.Sprint(err))
		if !errors.Is(tc.want, ErrApply) {
			testutil.Equal(t, len(f.router.uploaded), 0)
		}
	}
	f := newFixture(t)
	f.router.applyFails = true
	testutil.Contains(t, f.run(t).Error(), "rolled back (script failed): dhcp")
	testutil.Contains(t, f.lastJournal(t), "via=ax23: rolled back (script failed): dhcp")
}

func TestARollbackOnTheTargetFailsTheSync(t *testing.T) {
	f := newFixture(t)
	f.router.healthyAt = 0
	f.router.statuses = -1
	f.router.result = "rolled back (hostapd: x): wireless"
	err := f.run(t)
	testutil.True(t, errors.Is(err, ErrRolledBack), "the target rolled back by itself")
	testutil.Contains(t, f.lastJournal(t), "remote-sync fail", "rolled back (hostapd: x): wireless")
	testutil.True(t, slices.Contains(f.router.commands, "ax23: rm -rf "+relDir), "cleaned up after a verdict")
}

func TestATargetThatNeverSettlesIsRolledBackOnPurpose(t *testing.T) {
	f := newFixture(t)
	f.router.healthyAt = 0
	err := f.run(t)
	testutil.True(t, errors.Is(err, ErrRolledBack), fmt.Sprint(err))
	testutil.Contains(t, strings.Join(f.router.commands, "\n"), engine+" rollback")
	testutil.Contains(t, f.lastJournal(t), "rolled back (asked)")
	testutil.Equal(t, f.slept, 35*time.Second)
}

func TestALostConfirmLeavesTheVerdictToTheTarget(t *testing.T) {
	f := newFixture(t)
	f.router.confirmErr = true
	f.courier.Sleep = func(d time.Duration) {
		f.slept += d
		if f.slept > 20*time.Second {
			f.router.down = map[string]bool{}
			f.router.result = "rolled back (no confirmation in 180s): /usr/bin/routerbot-agent"
		}
	}
	err := f.run(t)
	testutil.True(t, errors.Is(err, ErrRolledBack), fmt.Sprint(err))
	testutil.Contains(t, f.lastJournal(t), "no confirmation in 180s")
}

func TestAConfirmWhoseAnswerWasLostStillCounts(t *testing.T) {
	f := newFixture(t)
	f.router.confirmErr = true
	f.courier.Sleep = func(d time.Duration) {
		f.slept += d
		if f.slept > 20*time.Second {
			f.router.down = map[string]bool{}
			f.router.result = confirmed
		}
	}
	testutil.NoErr(t, f.run(t))
}

func TestNoVerdictKeepsTheReleaseOnTheTarget(t *testing.T) {
	f := newFixture(t)
	f.router.confirmErr = true
	err := f.run(t)
	testutil.True(t, errors.Is(err, ErrUnreachable), fmt.Sprint(err))
	testutil.Contains(t, f.lastJournal(t), "no verdict")
	testutil.True(t, !slices.Contains(f.router.commands, "ax23: rm -rf "+relDir), "the engine may still run from the release")
}

func TestTheInstalledEngineRunsAReleaseThatDoesNotShipOne(t *testing.T) {
	f := newFixture(t)
	f.spec.Files = f.spec.Files[:1]
	f.spec.Restart = nil
	delete(f.files, applySrc)
	delete(f.files, initSrc)
	testutil.NoErr(t, f.run(t))
	testutil.Contains(t, strings.Join(f.router.commands, "\n"), "ax23: "+InstalledEngine+" --release "+relDir)
	testutil.Equal(t, f.sent(t)["RESTART"], "644 ")
}

func TestOnlyAGatedPinnedListedBundleLeavesTheServer(t *testing.T) {
	cases := map[string]struct {
		set  func(t *testing.T, f *fixture)
		want error
	}{
		"no gate verdict": {func(t *testing.T, f *fixture) { testutil.Write(t, f.syncer.Journal.Path, "") }, gatedimage.ErrNotGated},
		"local image": {func(_ *testing.T, f *fixture) {
			f.script["docker image inspect"] = procexectest.Reply{Res: procexec.Result{Stdout: "[]"}}
		}, gatedimage.ErrNoDigest},
		"unlisted file": {func(_ *testing.T, f *fixture) { f.files["agent/extra"] = "x" }, gatedimage.ErrUnlisted},
		"missing file":  {func(_ *testing.T, f *fixture) { delete(f.files, initSrc) }, gatedimage.ErrMissing},
		"too large":     {func(_ *testing.T, f *fixture) { f.files[agentSrc] = strings.Repeat("x", MaxFileSize+1) }, ErrTooLarge},
		"no target conf": {func(t *testing.T, f *fixture) {
			testutil.NoErr(t, os.Remove(filepath.Join(f.targets, target, confFile)))
		}, ErrTarget},
		"no hosts": {func(t *testing.T, f *fixture) { testutil.Write(t, filepath.Join(f.targets, target, confFile), "x=1\n") }, ErrTarget},
		"bad host alias": {func(t *testing.T, f *fixture) {
			testutil.Write(t, filepath.Join(f.targets, target, confFile), "hosts=-oProxy\n")
		}, ErrTarget},
		"no ssh config": {func(t *testing.T, f *fixture) {
			testutil.NoErr(t, os.Remove(filepath.Join(f.targets, target, sshConfigFile)))
		}, ErrTarget},
		"docker cp fails": {func(_ *testing.T, f *fixture) {
			f.script["docker cp"] = procexectest.Reply{Err: errors.New("no such path")}
		}, nil},
	}
	for name, tc := range cases {
		f := newFixture(t)
		tc.set(t, f)
		err := f.run(t)
		testutil.True(t, err != nil && (tc.want == nil || errors.Is(err, tc.want)), name+": "+fmt.Sprint(err))
		testutil.Equal(t, len(f.router.commands) == 0 || tc.want == nil || errors.Is(tc.want, ErrTarget), true)
		testutil.Equal(t, len(f.router.uploaded), 0)
	}
}

func TestAppsWithoutARemoteSpecAndBadArgumentsAreRefused(t *testing.T) {
	f := newFixture(t)
	f.syncer.Policy = policy.Policy{Apps: map[string]policy.App{plainApp: {}}}
	f.syncer.Courier = f.courier
	testutil.True(t, errors.Is(f.syncer.Run(context.Background(), plainApp, tag), ErrNoSpec), "no remote spec")
	testutil.True(t, f.syncer.Run(context.Background(), "Bad App", tag) != nil, "bad app name")
	testutil.True(t, f.syncer.Run(context.Background(), app, "latest") != nil, "bad tag")
}

func TestOnlyTheLastTenBackupsAreKept(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.courier.Backups, target)
	for i := range 12 {
		testutil.Write(t, filepath.Join(dir, fmt.Sprintf("20261001T0000%02dZ-old.tar.gz", i)), "x")
	}
	testutil.NoErr(t, f.run(t))
	kept, _ := filepath.Glob(filepath.Join(dir, "*.tar.gz"))
	testutil.Equal(t, len(kept), keepBackups)
	testutil.Equal(t, filepath.Base(kept[0]), "20261001T000003Z-old.tar.gz")
}

func TestAFailedUploadStopsTheSync(t *testing.T) {
	f := newFixture(t)
	f.exec.Handler = func(c procexec.Command) (procexec.Result, error) {
		if c.Name == SSHBinary && strings.Contains(c.Args[len(c.Args)-1], "tar -x") {
			return procexec.Result{Stderr: "tar: short read"}, errors.New("exit status 1")
		}
		if c.Name == SSHBinary {
			return f.router.handle(c)
		}
		return f.script.Handle(c)
	}
	err := f.run(t)
	testutil.Contains(t, err.Error(), "upload to ax23 failed: tar: short read")
}

func TestABrokenStatusPollIsRetried(t *testing.T) {
	g := newFixture(t)
	polls := 0
	g.exec.Handler = func(c procexec.Command) (procexec.Result, error) {
		if c.Name == SSHBinary && strings.HasSuffix(c.Args[len(c.Args)-1], " status") && polls == 0 {
			polls++
			return procexec.Result{}, errors.New("exit status 255")
		}
		if c.Name == SSHBinary {
			return g.router.handle(c)
		}
		return g.script.Handle(c)
	}
	testutil.NoErr(t, g.run(t))
	testutil.Equal(t, g.router.statuses, 3)
}

func TestRegistryAndServerProblemsStopTheSync(t *testing.T) {
	f := newFixture(t)
	testutil.Write(t, f.syncer.Apps.Path, "")
	testutil.True(t, f.run(t) != nil, "app not on this server")
	g := newFixture(t)
	testutil.Write(t, filepath.Join(g.base, "opt", app, "app.conf"), "image=Bad Image\n")
	testutil.True(t, g.run(t) != nil, "bad image")
	h := newFixture(t)
	testutil.NoErr(t, os.Remove(filepath.Join(h.base, "opt", app, "app.conf")))
	testutil.True(t, h.run(t) != nil, "no app.conf")
	k := newFixture(t)
	testutil.Write(t, k.courier.Backups, "a file, not a directory")
	testutil.True(t, k.run(t) != nil, "backups unwritable")
	testutil.Equal(t, len(k.router.uploaded), 0)
	m := newFixture(t)
	m.syncer.Journal.Path = filepath.Join(m.base, "missing", "deploys.log")
	testutil.Write(t, filepath.Join(m.base, "missing"), "x")
	testutil.True(t, m.run(t) != nil, "journal unreadable")
}

func TestTheCourierRefusesAMalformedShipment(t *testing.T) {
	f := newFixture(t)
	for name, sh := range map[string]Shipment{
		"target":  {Target: "../x", Sha7: sha7, Engine: EngineBundled},
		"commit":  {Target: target, Sha7: "ABCDEF1", Engine: EngineBundled},
		"engine":  {Target: target, Sha7: sha7, Engine: "sh -c id"},
		"unknown": {Target: "lab", Sha7: sha7, Engine: EngineOnTarget},
	} {
		out, err := f.courier.Deliver(context.Background(), sh)
		testutil.True(t, errors.Is(err, ErrShipment) || errors.Is(err, ErrTarget), name)
		testutil.Equal(t, out.Host, noHost)
	}
	testutil.Equal(t, len(f.router.commands), 0)
}

func TestTheSudoCourierRunsTheDeliveryAsTheRemoteUser(t *testing.T) {
	fake := &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stdout: Outcome{Host: fallback, Result: "confirmed\n"}.Format()}, nil
	}}
	c := SudoCourier{Exec: fake, Binary: "/opt/deploy-hub/bin/deploy-hub", User: DefaultUser}
	out, err := c.Deliver(context.Background(), Shipment{Target: target, Sha7: sha7, Engine: EngineBundled, Archive: []byte("tar")})
	testutil.NoErr(t, err)
	testutil.Equal(t, out, Outcome{Host: fallback, Result: confirmed})
	call := fake.Calls()[0]
	testutil.Equal(t, procexectest.Line(call), "sudo -n -u dh-remote /opt/deploy-hub/bin/deploy-hub remote-deliver ax23 abcdef1 bundled")
	testutil.Equal(t, call.Stdin, "tar")
	failing := &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stdout: "via ax23\nresult rolled back (asked): wireless\n"}, errors.New("exit status 1")
	}}
	out, err = SudoCourier{Exec: failing, Binary: "dh", User: DefaultUser}.Deliver(context.Background(), Shipment{})
	testutil.Equal(t, out, Outcome{Host: primary, Result: "rolled back (asked): wireless"})
	testutil.Contains(t, err.Error(), "remote-deliver: exit status 1: result rolled back (asked): wireless")
	testutil.Equal(t, ParseOutcome("noise\n"), Outcome{Host: noHost})
}
