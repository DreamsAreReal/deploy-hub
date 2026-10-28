package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	appName   = "web"
	storeJSON = `{"web": {".env": {"API_TOKEN": "s3cr3t", "DSN": "https://dsn.example/3?x=1&y=2"}, "ingest.env": {"INGEST_TOKEN": "t0k"}}, "other": {".env": {"X": "y"}}}`
	existing  = "# config\nPORT=8080\nAPI_TOKEN=old\n\nAPI_TOKEN=older\nexport MODE=prod\n"
)

func store(t *testing.T, reply procexec.Result, fail error) (Store, *procexectest.Fake) {
	t.Helper()
	key := filepath.Join(t.TempDir(), "age.key")
	testutil.Write(t, key, "AGE-SECRET-KEY-1TEST")
	fake := &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return reply, fail }}
	return Store{File: "/srv/server-ops/secrets/values.sops.yaml", KeyFile: key, Sops: "/usr/local/bin/sops", Exec: fake}, fake
}

func TestLayWritesOnlyTheAppSecretsAndKeepsTheRest(t *testing.T) {
	s, fake := store(t, procexec.Result{Stdout: storeJSON}, nil)
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, EnvFile), existing)
	keys, err := s.Lay(t.Context(), appName, dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, strings.Join(keys, ","), ".env:API_TOKEN,.env:DSN,ingest.env:INGEST_TOKEN")
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, "ingest.env")), "INGEST_TOKEN=t0k\n")
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, EnvFile)), "# config\nPORT=8080\nAPI_TOKEN=s3cr3t\n\nexport MODE=prod\nDSN='https://dsn.example/3?x=1&y=2'\n")
	info, err := os.Stat(filepath.Join(dir, EnvFile))
	testutil.NoErr(t, err)
	testutil.Equal(t, info.Mode().Perm(), os.FileMode(0o600))
	call := fake.Calls()[0]
	testutil.Equal(t, strings.Join(call.Args, " "), "--decrypt --output-type json "+s.File)
	testutil.Equal(t, call.Env, []string{"SOPS_AGE_KEY_FILE=" + s.KeyFile})
	left, _ := filepath.Glob(filepath.Join(dir, EnvFile+".laying-*"))
	testutil.Equal(t, len(left), 0)
}

func TestLayLeavesAppsWithoutSecretsAlone(t *testing.T) {
	s, _ := store(t, procexec.Result{Stdout: storeJSON}, nil)
	dir := t.TempDir()
	keys, err := s.Lay(t.Context(), "quiet", dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(keys), 0)
	if _, missing := os.Stat(filepath.Join(dir, EnvFile)); !errors.Is(missing, os.ErrNotExist) {
		t.Fatalf("an .env appeared: %v", missing)
	}
	fresh, _ := store(t, procexec.Result{Stdout: storeJSON}, nil)
	_, err = fresh.Lay(t.Context(), appName, dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, EnvFile)), "API_TOKEN=s3cr3t\nDSN='https://dsn.example/3?x=1&y=2'\n")
}

func TestLayRefusesWithoutKeyOrWithABrokenStore(t *testing.T) {
	locked, fake := store(t, procexec.Result{Stdout: storeJSON}, nil)
	locked.KeyFile = filepath.Join(t.TempDir(), "missing")
	_, err := locked.Lay(t.Context(), appName, t.TempDir())
	testutil.True(t, errors.Is(err, ErrLocked), "locked without a key")
	testutil.Equal(t, len(fake.Calls()), 0)
	failing, _ := store(t, procexec.Result{Stderr: "Failed to get the data key"}, errors.New("exit status 128"))
	_, err = failing.Lay(t.Context(), appName, t.TempDir())
	testutil.True(t, errors.Is(err, errDecrypt) && !strings.Contains(err.Error(), "data key"), "decrypt failure without output: "+err.Error())
	for _, body := range []string{"not json", `{"web": {".env": {"PORT": 8080}}}`, `{"web": {"PORT": "8080"}}`} {
		broken, _ := store(t, procexec.Result{Stdout: body}, nil)
		_, err = broken.Lay(t.Context(), appName, t.TempDir())
		testutil.True(t, errors.Is(err, errShape), body)
	}
	unsafe, _ := store(t, procexec.Result{Stdout: `{"web": {".env": {"BAD": "it's $HOME"}}}`}, nil)
	_, err = unsafe.Lay(t.Context(), appName, t.TempDir())
	testutil.True(t, errors.Is(err, errUnsafe) && strings.HasPrefix(err.Error(), "BAD:"), "unsafe value")
	for _, file := range []string{"../escape.env", "notes.txt", "Upper.env"} {
		outside, _ := store(t, procexec.Result{Stdout: `{"web": {"` + file + `": {"A": "1"}}}`}, nil)
		_, err = outside.Lay(t.Context(), appName, t.TempDir())
		testutil.True(t, errors.Is(err, errFile), file)
	}
	folder := t.TempDir()
	testutil.Mkdir(t, filepath.Join(folder, EnvFile))
	good, _ := store(t, procexec.Result{Stdout: storeJSON}, nil)
	_, err = good.Lay(t.Context(), appName, folder)
	testutil.True(t, err != nil, "an .env folder cannot be read")
	_, err = good.Lay(t.Context(), appName, filepath.Join(t.TempDir(), "gone"))
	testutil.True(t, err != nil, "a missing app folder cannot take an .env")
}

func TestFormatQuotesOnlyWhatNeedsIt(t *testing.T) {
	cases := map[string]string{
		"plain-1.2_a:b@c/d+e,f=g": "K=plain-1.2_a:b@c/d+e,f=g",
		"has space $x":            "K='has space $x'",
		"it's":                    `K="it's"`,
		"line\nbreak \\ \"q\"'":   `K="line\nbreak \\ \"q\"'"`,
	}
	for value, want := range cases {
		got, err := format("K", value)
		testutil.NoErr(t, err)
		testutil.Equal(t, got, want)
	}
	if _, err := format("K", "it's `x`"); !errors.Is(err, errUnsafe) {
		t.Fatalf("err %v", err)
	}
}

func TestWriteAtomicallyCleansUpWhenTheTargetIsTaken(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, EnvFile)
	testutil.Mkdir(t, filepath.Join(target, "child"))
	testutil.True(t, writeAtomically(target, []byte("A=1\n")) != nil, "a folder in the way")
	left, _ := filepath.Glob(filepath.Join(dir, EnvFile+".laying-*"))
	testutil.Equal(t, len(left), 0)
	testutil.True(t, writeAtomically(filepath.Join(dir, "missing", EnvFile), nil) != nil, "no folder")
}
