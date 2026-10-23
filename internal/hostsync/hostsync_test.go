package hostsync

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	app       = "obs"
	tag       = "sha-abcdef1234567"
	sha7      = "abcdef1"
	image     = "ghcr.io/owner/obs"
	digest    = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	script    = "/usr/local/sbin/obs-metrics"
	service   = "/etc/systemd/system/obs-metrics.service"
	timer     = "/etc/systemd/system/obs-metrics.timer"
	site      = "/etc/caddy/extra/obs.caddy"
	textfile  = "/var/lib/node_exporter/textfile"
	inspect   = "docker image inspect"
	cpPrefix  = "docker cp"
	verify    = "systemd-analyze verify"
	reload    = "systemctl daemon-reload"
	enable    = "systemctl enable --now obs-metrics.timer"
	check     = "systemctl start obs-metrics.service"
	validateC = "caddy validate"
	reloadC   = "systemctl reload caddy"
	gateOK    = "[2026-10-23T00:00:00Z] obs@abcdef1 gate ok 60s\n"
	siteTmpl  = "@HOST@ {\n\trespond \"@TOKEN@\"\n}\n"
	envFile   = "ingest.env"
	envBody   = "HOST=obs.example.org\nTOKEN=0f0f\n"
	srcPy     = "obs_metrics.py"
	srcUnit   = "units/obs-metrics.service"
	srcTimer  = "units/obs-metrics.timer"
	srcSite   = "obs.caddy"
	plainApp  = "plain"
	root      = "root"
)

type recorder struct{ samples []obs.Sample }

func (r *recorder) Push(_ context.Context, s []obs.Sample) { r.samples = append(r.samples, s...) }

func (r *recorder) value(name string) float64 {
	for _, s := range r.samples {
		if s.Name == name {
			return s.Value
		}
	}
	return -1
}

type fixture struct {
	syncer  Syncer
	exec    *procexectest.Fake
	script  procexectest.Script
	root    string
	appDir  string
	files   map[string]string
	extra   []func(t *testing.T, dir string)
	metrics *recorder
}

func spec() *policy.Host {
	return &policy.Host{
		Files: []policy.HostFile{
			{Src: srcPy, Dst: script, Owner: root, Group: root, Mode: "0755"},
			{Src: srcUnit, Dst: service, Owner: root, Group: root, Mode: "0644"},
			{Src: srcTimer, Dst: timer, Owner: root, Group: root, Mode: "0644"},
			{Src: srcSite, Dst: site, Owner: root, Group: "caddy", Mode: "0640", Env: envFile},
		},
		Dirs:   []policy.HostDir{{Path: textfile, Owner: root, Group: root, Mode: "0755"}},
		Timers: []string{"obs-metrics.timer"},
		Checks: []string{"obs-metrics.service"},
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{root: filepath.Join(base, "host"), appDir: filepath.Join(base, "opt", app), metrics: &recorder{}}
	for _, d := range []string{"/usr/local/sbin", "/etc/systemd/system", "/etc/caddy/extra", "/var/lib"} {
		testutil.Mkdir(t, filepath.Join(f.root, d))
	}
	f.files = map[string]string{
		srcPy: "print(1)\n", srcUnit: "[Service]\nExecStart=" + script + "\n",
		srcTimer: "[Timer]\nOnCalendar=minutely\n", srcSite: siteTmpl,
	}
	testutil.Write(t, filepath.Join(f.appDir, "app.conf"), "image="+image+"\n")
	testutil.Write(t, filepath.Join(f.appDir, envFile), envBody)
	testutil.Write(t, filepath.Join(base, "apps.list"), app+" "+f.appDir+"\n")
	testutil.Write(t, filepath.Join(base, "deploys.log"), gateOK)
	f.script = procexectest.Script{
		inspect:          {Res: procexec.Result{Stdout: `["` + image + `@` + digest + `"]` + "\n"}},
		"docker create":  {Res: procexec.Result{Stdout: "cid\n"}},
		cpPrefix:         {Run: func(c procexec.Command) { f.populate(t, c.Args[len(c.Args)-1]) }},
		"docker rm":      {},
		verify:           {},
		reload:           {},
		enable:           {},
		check:            {},
		validateC:        {},
		reloadC:          {},
		"systemctl stop": {},
	}
	f.exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) { return f.script.Handle(c) }}
	pol := policy.Policy{Apps: map[string]policy.App{app: {Host: spec()}, plainApp: {}}}
	f.syncer = Syncer{
		Policy: pol, Exec: f.exec, Root: f.root, Backups: filepath.Join(base, "backups"),
		Accounts: func(string, string) (int, int, error) { return os.Getuid(), os.Getgid(), nil },
		Chown:    os.Chown,
		Now:      func() time.Time { return time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC) },
		Out:      console.New(&bytes.Buffer{}), Log: slog.New(slog.DiscardHandler), Metrics: f.metrics,
	}
	f.syncer.Apps.Path, f.syncer.Apps.Root = filepath.Join(base, "apps.list"), base
	f.syncer.Journal = journal.Journal{Path: filepath.Join(base, "deploys.log"), Now: f.syncer.Now}
	return f
}

func (f *fixture) writeBundle(_ *testing.T, files map[string]string) {
	for rel, body := range files {
		f.files[rel] = body
	}
}

func (f *fixture) populate(t *testing.T, dir string) {
	t.Helper()
	for rel, body := range f.files {
		testutil.Write(t, filepath.Join(dir, rel), body)
	}
	for _, add := range f.extra {
		add(t, dir)
	}
}

func (f *fixture) run(t *testing.T) error {
	t.Helper()
	return f.syncer.Run(context.Background(), app, tag)
}

func (f *fixture) host(p string) string { return filepath.Join(f.root, p) }

func (f *fixture) journal(t *testing.T) string {
	t.Helper()
	return testutil.Read(t, f.syncer.Journal.Path)
}

func TestSyncInstallsTheListedFilesAndActivatesThem(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.run(t))
	testutil.Equal(t, testutil.Read(t, f.host(script)), "print(1)\n")
	testutil.Perm(t, f.host(script), 0o755)
	testutil.Perm(t, f.host(site), 0o640)
	testutil.Perm(t, f.host(textfile), 0o755)
	testutil.Equal(t, testutil.Read(t, f.host(site)), "obs.example.org {\n\trespond \"0f0f\"\n}\n")
	lines := strings.Join(f.exec.Lines(), "\n")
	testutil.Contains(t, lines, "docker create --pull never "+image+"@"+digest, "docker cp cid:/deploy-hub/host/.", "docker rm -f cid",
		verify+" "+f.host(service)+" "+f.host(timer), reload, validateC+" --config "+f.host(Caddyfile), reloadC, enable, check)
	testutil.Contains(t, f.journal(t), "obs@abcdef1 host-sync ok", "changed=4")
	testutil.Equal(t, f.metrics.value(obs.MetricHostSyncSuccess), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricHostFilesChanged), 4.0)
	manifest := testutil.Read(t, filepath.Join(f.syncer.Backups, app, "20261023T010000Z-"+sha7, manifestName))
	testutil.Contains(t, manifest, "- - - - "+script, "- - - - "+site)
}

func TestSecondSyncChangesNothingButStillChecksHealth(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.run(t))
	before := len(f.exec.Lines())
	testutil.NoErr(t, f.run(t))
	second := strings.Join(f.exec.Lines()[before:], "\n")
	testutil.Excludes(t, second, verify, reload, validateC, reloadC)
	testutil.Contains(t, second, enable, check)
	testutil.Contains(t, f.journal(t), "host-sync ok 0s changed=0")
	entries, err := os.ReadDir(filepath.Join(f.syncer.Backups, app))
	testutil.NoErr(t, err)
	testutil.Equal(t, len(entries), 1)
}

func TestOnlyTheChangedUnitIsVerified(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.run(t))
	f.writeBundle(t, map[string]string{srcTimer: "[Timer]\nOnCalendar=hourly\n"})
	before := len(f.exec.Lines())
	testutil.NoErr(t, f.run(t))
	second := strings.Join(f.exec.Lines()[before:], "\n")
	testutil.Contains(t, second, verify+" "+f.host(timer)+"\n", reload)
	testutil.Excludes(t, second, f.host(service), validateC)
}

func TestChangedOwnershipOrModeIsReinstalled(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.run(t))
	testutil.NoErr(t, os.Chmod(f.host(script), 0o600))
	testutil.NoErr(t, f.run(t))
	testutil.Perm(t, f.host(script), 0o755)
	testutil.Contains(t, f.journal(t), "changed=1")
	other := false
	f.syncer.Accounts = func(string, string) (int, int, error) { return os.Getuid(), os.Getgid() + 1, nil }
	f.syncer.Chown = func(string, int, int) error { other = true; return nil }
	testutil.NoErr(t, f.run(t))
	testutil.True(t, other, "a different group must be applied")
	testutil.Contains(t, f.journal(t), "changed=4")
}

func TestFailedHealthCheckRestoresThePreviousFiles(t *testing.T) {
	f := newFixture(t)
	testutil.Write(t, f.host(script), "old\n")
	testutil.NoErr(t, os.Chmod(f.host(script), 0o600))
	f.script[check] = procexectest.Reply{Err: errors.New("exit status 1")}
	err := f.run(t)
	testutil.Is(t, err, ErrActivate)
	testutil.Equal(t, testutil.Read(t, f.host(script)), "old\n")
	testutil.Perm(t, f.host(script), 0o600)
	testutil.Missing(t, f.host(service))
	testutil.Missing(t, f.host(site))
	j := f.journal(t)
	testutil.Contains(t, j, "obs@abcdef1 host-sync fail", "obs@abcdef1 host-restore ok 0s files=4")
	testutil.Equal(t, f.metrics.value(obs.MetricHostSyncSuccess), 0.0)
	testutil.Equal(t, f.exec.Count(reload), 2)
	testutil.Equal(t, f.exec.Count(reloadC), 2)
	backup := filepath.Join(f.syncer.Backups, app, "20261023T010000Z-"+sha7)
	testutil.Equal(t, testutil.Read(t, filepath.Join(backup, "0")), "old\n")
	testutil.Contains(t, testutil.Read(t, filepath.Join(backup, manifestName)), "0 600 ")
}

func TestEveryActivationStepFailureRestores(t *testing.T) {
	for _, step := range []string{verify, reload, validateC, reloadC, enable} {
		t.Run(step, func(t *testing.T) {
			f := newFixture(t)
			f.script[step] = procexectest.Reply{Err: errors.New("exit status 1")}
			testutil.Is(t, f.run(t), ErrActivate)
			testutil.Missing(t, f.host(script))
			testutil.Contains(t, f.journal(t), "host-restore")
		})
	}
}

func TestFailedRestoreIsJournaled(t *testing.T) {
	f := newFixture(t)
	f.script[check] = procexectest.Reply{Err: errors.New("exit status 1")}
	f.script[reloadC] = procexectest.Reply{Err: errors.New("caddy down")}
	testutil.Is(t, f.run(t), ErrActivate)
	testutil.Contains(t, f.journal(t), "host-restore fail")
}

func TestRefusesASubstitutedOrUngatedImage(t *testing.T) {
	cases := map[string]struct {
		journal string
		inspect procexectest.Reply
		want    error
	}{
		"no gate verdict":       {journal: "", want: ErrNotGated},
		"failed gate":           {journal: strings.Replace(gateOK, "gate ok", "gate fail", 1), want: ErrNotGated},
		"gate of another app":   {journal: strings.Replace(gateOK, "obs@", "web@", 1), want: ErrNotGated},
		"gate of another sha":   {journal: strings.Replace(gateOK, sha7, "1234567", 1), want: ErrNotGated},
		"deploy is not a gate":  {journal: strings.Replace(gateOK, "gate ok", "deploy ok", 1), want: ErrNotGated},
		"built locally":         {inspect: procexectest.Reply{Res: procexec.Result{Stdout: "[]"}}, want: ErrNoDigest},
		"digest of other image": {inspect: procexectest.Reply{Res: procexec.Result{Stdout: `["ghcr.io/evil/obs@` + digest + `"]`}}, want: ErrNoDigest},
		"malformed digest":      {inspect: procexectest.Reply{Res: procexec.Result{Stdout: `["` + image + `@sha256:12"]`}}, want: ErrNoDigest},
		"not json":              {inspect: procexectest.Reply{Res: procexec.Result{Stdout: "<no value>"}}, want: ErrNoDigest},
		"image missing":         {inspect: procexectest.Reply{Err: errors.New("no such image")}, want: ErrNoDigest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if errors.Is(tc.want, ErrNotGated) {
				testutil.Write(t, f.syncer.Journal.Path, tc.journal)
			} else {
				f.script[inspect] = tc.inspect
			}
			testutil.Is(t, f.run(t), tc.want)
			testutil.Equal(t, f.exec.Count("docker create"), 0)
			testutil.Missing(t, f.host(script))
			testutil.Contains(t, f.journal(t), "host-sync fail 0s changed=0")
		})
	}
}

func TestRefusesBundlesOutsideTheList(t *testing.T) {
	cases := map[string]struct {
		prepare func(t *testing.T, f *fixture)
		want    error
	}{
		"unlisted file": {prepare: func(t *testing.T, f *fixture) {
			f.writeBundle(t, map[string]string{"sudoers": "deploy ALL=(ALL) ALL\n"})
		}, want: ErrUnlisted},
		"symlink in bundle": {prepare: func(_ *testing.T, f *fixture) {
			delete(f.files, srcPy)
			f.extra = append(f.extra, func(t *testing.T, dir string) {
				testutil.NoErr(t, os.Symlink("/etc/shadow", filepath.Join(dir, srcPy)))
			})
		}, want: ErrSymlink},
		"listed file missing": {prepare: func(_ *testing.T, f *fixture) {
			delete(f.files, srcSite)
		}, want: ErrMissing},
		"fifo in bundle": {prepare: func(_ *testing.T, f *fixture) {
			f.extra = append(f.extra, func(t *testing.T, dir string) {
				testutil.NoErr(t, syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600))
			})
		}, want: ErrNotRegular},
		"file too large": {prepare: func(t *testing.T, f *fixture) {
			f.writeBundle(t, map[string]string{srcPy: strings.Repeat("x", MaxFileSize+1)})
		}, want: ErrTooLarge},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tc.prepare(t, f)
			testutil.Is(t, f.run(t), tc.want)
			testutil.Missing(t, f.host(service))
			testutil.Equal(t, f.exec.Count("docker rm -f cid"), 1)
		})
	}
}

func TestRefusesSymlinkedOrIrregularDestinations(t *testing.T) {
	cases := map[string]struct {
		prepare func(t *testing.T, f *fixture)
		want    error
	}{
		"destination is a symlink": {prepare: func(t *testing.T, f *fixture) {
			testutil.NoErr(t, os.Symlink(filepath.Join(f.root, "elsewhere"), f.host(script)))
		}, want: ErrSymlink},
		"destination is a directory": {prepare: func(t *testing.T, f *fixture) {
			testutil.Mkdir(t, f.host(script))
		}, want: ErrNotRegular},
		"parent is a symlink": {prepare: func(t *testing.T, f *fixture) {
			testutil.NoErr(t, os.RemoveAll(f.host("/etc/caddy/extra")))
			testutil.Mkdir(t, filepath.Join(f.root, "real"))
			testutil.NoErr(t, os.Symlink(filepath.Join(f.root, "real"), f.host("/etc/caddy/extra")))
		}, want: ErrParent},
		"parent is missing": {prepare: func(t *testing.T, f *fixture) {
			testutil.NoErr(t, os.RemoveAll(f.host("/etc/caddy/extra")))
		}, want: os.ErrNotExist},
		"directory spec is a symlink": {prepare: func(t *testing.T, f *fixture) {
			testutil.Mkdir(t, f.host("/var/lib/node_exporter"))
			testutil.NoErr(t, os.Symlink(f.root, f.host(textfile)))
		}, want: ErrParent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tc.prepare(t, f)
			testutil.Is(t, f.run(t), tc.want)
			testutil.Equal(t, f.exec.Count(reload), 0)
		})
	}
}

func TestPlaceholdersNeedSafeValues(t *testing.T) {
	cases := map[string]string{
		"missing key":      "HOST=obs.example.org\n",
		"brace injection":  "HOST=obs.example.org\nTOKEN=x}\n",
		"space injection":  "HOST=a b\nTOKEN=0f\n",
		"empty value":      "HOST=\nTOKEN=0f\n",
		"oversized value":  "HOST=" + strings.Repeat("a", 257) + "\nTOKEN=0f\n",
		"newline smuggled": "HOST=obs\\nimport /etc\nTOKEN=0f\n",
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			testutil.Write(t, filepath.Join(f.appDir, envFile), env)
			testutil.Is(t, f.run(t), ErrPlaceholder)
			testutil.Missing(t, f.host(site))
		})
	}
}

func TestEnvFileMustNotBeASymlink(t *testing.T) {
	f := newFixture(t)
	env := filepath.Join(f.appDir, envFile)
	testutil.NoErr(t, os.Remove(env))
	testutil.NoErr(t, os.Symlink(filepath.Join(f.appDir, "app.conf"), env))
	testutil.Fails(t, f.run(t))
	testutil.Missing(t, f.host(site))
}

func TestRefusesAppsWithoutASpecOrBadArguments(t *testing.T) {
	cases := map[string][2]string{
		"unknown app":  {"ghost", tag},
		"no host spec": {plainApp, tag},
		"bad app":      {"../obs", tag},
		"bad tag":      {app, "latest"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			testutil.Fails(t, f.syncer.Run(context.Background(), args[0], args[1]))
			testutil.Equal(t, len(f.exec.Lines()), 0)
		})
	}
	f := newFixture(t)
	testutil.Is(t, f.syncer.Run(context.Background(), plainApp, tag), ErrNoSpec)
}

func TestRegistryProblemsStopTheSync(t *testing.T) {
	cases := map[string]func(t *testing.T, f *fixture){
		"app not in apps.list": func(t *testing.T, f *fixture) { testutil.Write(t, f.syncer.Apps.Path, "other\n") },
		"no app.conf":          func(t *testing.T, f *fixture) { testutil.NoErr(t, os.Remove(filepath.Join(f.appDir, "app.conf"))) },
		"bad image": func(t *testing.T, f *fixture) {
			testutil.Write(t, filepath.Join(f.appDir, "app.conf"), "image=docker.io/x\n")
		},
		"journal unreadable": func(t *testing.T, f *fixture) { testutil.NoErr(t, os.Chmod(f.syncer.Journal.Path, 0)) },
		"unknown account": func(_ *testing.T, f *fixture) {
			f.syncer.Accounts = func(string, string) (int, int, error) { return 0, 0, errors.New("no such user") }
		},
		"container not created": func(_ *testing.T, f *fixture) {
			f.script["docker create"] = procexectest.Reply{Err: errors.New("pull access denied")}
		},
		"bundle not copied": func(_ *testing.T, f *fixture) {
			f.script[cpPrefix] = procexectest.Reply{Err: errors.New("no such path")}
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			prepare(t, f)
			testutil.Fails(t, f.run(t))
			testutil.Missing(t, f.host(script))
		})
	}
}

func TestBundleContainerIsRemovedEvenWhenRemovalFails(t *testing.T) {
	f := newFixture(t)
	f.script["docker rm"] = procexectest.Reply{Err: errors.New("busy")}
	testutil.NoErr(t, f.run(t))
	testutil.Equal(t, f.exec.Count("docker rm -f cid"), 1)
}

func TestBackupsKeepTheNewestFive(t *testing.T) {
	f := newFixture(t)
	root := filepath.Join(f.syncer.Backups, app)
	for _, old := range []string{"20260101T000000Z-a", "20260102T000000Z-a", "20260103T000000Z-a", "20260104T000000Z-a", "20260105T000000Z-a"} {
		testutil.Mkdir(t, filepath.Join(root, old))
	}
	testutil.NoErr(t, f.run(t))
	entries, err := os.ReadDir(root)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(entries), keepBackups)
	testutil.Equal(t, entries[0].Name(), "20260102T000000Z-a")
	testutil.Equal(t, entries[keepBackups-1].Name(), "20261023T010000Z-"+sha7)
}

func TestUnwritableBackupStopsBeforeAnyChange(t *testing.T) {
	f := newFixture(t)
	testutil.Write(t, f.syncer.Backups, "not a directory")
	testutil.Fails(t, f.run(t))
	testutil.Missing(t, f.host(script))
}

func TestFailedWriteRestoresWhatWasWritten(t *testing.T) {
	f := newFixture(t)
	calls := 0
	f.syncer.Chown = func(p string, uid, gid int) error {
		calls++
		if strings.HasSuffix(p, tempSuffix) && calls > 2 {
			return errors.New("chown refused")
		}
		return os.Chown(p, uid, gid)
	}
	testutil.Is(t, f.run(t), ErrActivate)
	testutil.Missing(t, f.host(script))
	matches, _ := filepath.Glob(f.host("/etc/systemd/system/.*"))
	testutil.Equal(t, len(matches), 0)
}
