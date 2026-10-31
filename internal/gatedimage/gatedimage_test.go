package gatedimage

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	img     = "ghcr.io/owner/app"
	tag     = "sha-abcdef1234567"
	digest  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cid     = "cid\n"
	ax      = "a/x"
	dCreate = "docker create"
	dCopy   = "docker cp"
	dRemove = "docker rm"
)

func source(t *testing.T, log string, script procexectest.Script) Source {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploys.log")
	testutil.Write(t, path, log)
	return Source{
		Exec:    &procexectest.Fake{Handler: script.Handle},
		Journal: journal.Journal{Path: path},
		Log:     slog.New(slog.DiscardHandler),
	}
}

func TestOnlyAPassingGateOfThatAppAndCommitCounts(t *testing.T) {
	ok := "[2026-10-23T00:00:00Z] app@abcdef1 gate ok 60s\n"
	testutil.NoErr(t, source(t, ok, nil).Gated("app", tag))
	for name, log := range map[string]string{
		"failed":    "[2026-10-23T00:00:00Z] app@abcdef1 gate fail 60s\n",
		"other app": "[2026-10-23T00:00:00Z] web@abcdef1 gate ok 60s\n",
		"other sha": "[2026-10-23T00:00:00Z] app@1234567 gate ok 60s\n",
		"a deploy":  "[2026-10-23T00:00:00Z] app@abcdef1 deploy ok 60s\n",
		"empty":     "",
	} {
		testutil.True(t, errors.Is(source(t, log, nil).Gated("app", tag), ErrNotGated), name)
	}
	s := source(t, ok, nil)
	s.Journal.Path = t.TempDir()
	testutil.True(t, s.Gated("app", tag) != nil, "unreadable journal")
}

func TestOnlyADigestOfTheAppsImageIsPinned(t *testing.T) {
	inspect := "docker image inspect"
	good := procexectest.Script{inspect: {Res: procexec.Result{Stdout: `["other@` + digest + `","` + img + `@` + digest + `"]`}}}
	ref, err := source(t, "", good).Pinned(context.Background(), img, tag)
	testutil.NoErr(t, err)
	testutil.Equal(t, ref, img+"@"+digest)
	for name, reply := range map[string]procexectest.Reply{
		"built locally":    {Res: procexec.Result{Stdout: "[]"}},
		"malformed digest": {Res: procexec.Result{Stdout: `["` + img + `@sha256:12"]`}},
		"not json":         {Res: procexec.Result{Stdout: "<no value>"}},
		"missing":          {Err: errors.New("no such image")},
	} {
		_, err := source(t, "", procexectest.Script{inspect: reply}).Pinned(context.Background(), img, tag)
		testutil.True(t, errors.Is(err, ErrNoDigest), name)
	}
}

func TestExtractCopiesOneDirectoryOutOfAContainerThatNeverStarts(t *testing.T) {
	to := filepath.Join(t.TempDir(), "bundle")
	s := source(t, "", procexectest.Script{dCreate: {Res: procexec.Result{Stdout: cid}}, dCopy: {}, dRemove: {Err: errors.New("busy")}})
	testutil.NoErr(t, s.Extract(context.Background(), img+"@"+digest, "/deploy-hub/remote", to))
	testutil.Equal(t, procexectest.LinesOf(s.Exec), []string{
		"docker create --pull never " + img + "@" + digest + " deploy-hub-bundle",
		"docker cp cid:/deploy-hub/remote/. " + to, "docker rm -f cid",
	})
	testutil.Perm(t, to, BundlePerm)
	testutil.True(t, s.Extract(context.Background(), "x", "/d", to) != nil, "the bundle directory exists already")
	failCreate := source(t, "", procexectest.Script{dCreate: {Err: errors.New("no image")}})
	testutil.Contains(t, failCreate.Extract(context.Background(), "ref", "/d", filepath.Join(t.TempDir(), "b")).Error(), "cannot create a container from ref")
	failCopy := source(t, "", procexectest.Script{dCreate: {Res: procexec.Result{Stdout: cid}}, dCopy: {Err: errors.New("no path")}, dRemove: {}})
	testutil.Contains(t, failCopy.Extract(context.Background(), "ref", "/d", filepath.Join(t.TempDir(), "b")).Error(), "cannot copy /d out of ref")
}

func TestABundleHoldsExactlyTheListedRegularFiles(t *testing.T) {
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, ax), "x")
	testutil.NoErr(t, CheckBundle(dir, []string{ax}))
	testutil.True(t, errors.Is(CheckBundle(dir, []string{ax, "b"}), ErrMissing), "missing")
	testutil.True(t, errors.Is(CheckBundle(dir, nil), ErrUnlisted), "unlisted")
	testutil.NoErr(t, os.Symlink("x", filepath.Join(dir, "a/link")))
	testutil.True(t, errors.Is(CheckBundle(dir, []string{ax, "a/link"}), ErrSymlink), "symlink")
	fifo := t.TempDir()
	testutil.NoErr(t, syscall.Mkfifo(filepath.Join(fifo, "p"), 0o600))
	testutil.True(t, errors.Is(CheckBundle(fifo, []string{"p"}), ErrNotRegular), "fifo")
	testutil.True(t, CheckBundle(filepath.Join(dir, "absent"), nil) != nil, "no bundle")
}
