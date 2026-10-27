package tools

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	busyboxURL    = "https://www.busybox.net/downloads/busybox-1.37.0.tar.bz2"
	busyboxConfig = "busybox.config"
	settings      = "CONFIG_AWK=y\n"
	failMsg       = "boom"
	tarStep       = "tar -xf"
	awkLink       = "/bin/awk\n"
	batsName      = "bats"
)

type busyboxMake struct {
	t      *testing.T
	failOn string
	links  string
	tree   string
}

func (b *busyboxMake) run(c procexec.Command) (procexec.Result, error) {
	line := procexectest.Line(c)
	if b.failOn != "" && strings.Contains(line, b.failOn) {
		return procexec.Result{Stderr: failMsg}, errors.New("exit 2")
	}
	switch {
	case c.Name == TarBinary:
		b.tree = filepath.Join(c.Args[3], srcTree)
		testutil.NoErr(b.t, os.MkdirAll(b.tree, dirPerm))
	case strings.HasSuffix(line, " "+sourceBusybox):
		testutil.Equal(b.t, testutil.Read(b.t, filepath.Join(b.tree, dotConfig)), settings)
		testutil.Write(b.t, filepath.Join(b.tree, sourceBusybox), "elf")
	case strings.HasSuffix(line, linksFile):
		testutil.Write(b.t, filepath.Join(b.tree, linksFile), b.links)
	}
	return procexec.Result{}, nil
}

func busyboxManifest(t *testing.T, data []byte) Manifest {
	t.Helper()
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, busyboxConfig), settings)
	return Manifest{Sources: []Source{{Name: sourceBusybox, URL: busyboxURL, SHA256: digest(data), Tree: srcTree, Config: busyboxConfig}}, dir: dir}
}

func fakeHost(t *testing.T) {
	t.Helper()
	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(name string) (string, error) { return "/host/bin/" + name, nil }
}

func TestBusyboxIsBuiltWithTheRouterConfig(t *testing.T) {
	fakeHost(t)
	data := []byte(payload)
	b := &busyboxMake{t: t, links: "/bin/awk\n/bin/[\n/usr/bin/sed\n/bin/busybox\n\n"}
	inst := installer(t, transport{busyboxURL: respond(http.StatusOK, data)}, &procexectest.Fake{Handler: b.run})
	testutil.NoErr(t, os.MkdirAll(inst.Dir, dirPerm))
	testutil.Write(t, filepath.Join(inst.Dir, batsName), batsName)
	testutil.Write(t, filepath.Join(inst.Dir, filepath.Join(BusyboxDir, "stale")), "old")
	m := busyboxManifest(t, data)
	testutil.NoErr(t, inst.Install(context.Background(), m))
	dir := filepath.Join(inst.Dir, BusyboxDir)
	entries, err := os.ReadDir(dir)
	testutil.NoErr(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	testutil.Equal(t, names, []string{"awk", "bash", batsName, sourceBusybox, "nl", "sed"})
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, sourceBusybox)), "elf")
	target, err := os.Readlink(filepath.Join(dir, "sed"))
	testutil.NoErr(t, err)
	testutil.Equal(t, target, sourceBusybox)
	host, err := os.Readlink(filepath.Join(dir, "bash"))
	testutil.NoErr(t, err)
	testutil.Equal(t, host, "/host/bin/bash")
	tool, err := os.Readlink(filepath.Join(dir, batsName))
	testutil.NoErr(t, err)
	testutil.Equal(t, tool, filepath.Join("..", batsName))
	testutil.Equal(t, m.Count(), 1)
}

func TestBusyboxBuildFailures(t *testing.T) {
	fakeHost(t)
	data := []byte(payload)
	for _, step := range []string{"oldconfig", "-j2", "busybox.links", tarStep} {
		b := &busyboxMake{t: t, failOn: step, links: awkLink}
		err := installer(t, transport{busyboxURL: respond(http.StatusOK, data)}, &procexectest.Fake{Handler: b.run}).Install(context.Background(), busyboxManifest(t, data))
		testutil.Contains(t, err.Error(), sourceBusybox+": ")
	}
	missing := busyboxManifest(t, data)
	missing.Sources[0].Config = "absent.config"
	b := &busyboxMake{t: t}
	testutil.Fails(t, installer(t, transport{busyboxURL: respond(http.StatusOK, data)}, &procexectest.Fake{Handler: b.run}).Install(context.Background(), missing))
	blocked := installer(t, transport{busyboxURL: respond(http.StatusOK, data)}, &procexectest.Fake{Handler: (&busyboxMake{t: t}).run})
	testutil.NoErr(t, os.MkdirAll(blocked.Dir, 0o500))
	testutil.Fails(t, blocked.Install(context.Background(), busyboxManifest(t, data)))
	lookPath = func(string) (string, error) { return "", errors.New("no bash") }
	b = &busyboxMake{t: t, links: awkLink}
	err := installer(t, transport{busyboxURL: respond(http.StatusOK, data)}, &procexectest.Fake{Handler: b.run}).Install(context.Background(), busyboxManifest(t, data))
	testutil.Contains(t, err.Error(), "no bash")
}

func TestBusyboxPlacementFailures(t *testing.T) {
	tree := t.TempDir()
	testutil.Fails(t, placeBusybox(tree, filepath.Join(t.TempDir(), "d")))
	testutil.Write(t, filepath.Join(tree, linksFile), awkLink)
	testutil.Fails(t, placeBusybox(tree, filepath.Join(t.TempDir(), "d")))
	testutil.Write(t, filepath.Join(tree, sourceBusybox), "elf")
	file := filepath.Join(t.TempDir(), "file")
	testutil.Write(t, file, "")
	testutil.Fails(t, placeBusybox(tree, filepath.Join(file, "d")))
	testutil.Write(t, filepath.Join(tree, linksFile), awkLink+awkLink)
	testutil.Fails(t, placeBusybox(tree, filepath.Join(t.TempDir(), "d")))
	fakeHost(t)
	dest := t.TempDir()
	testutil.Write(t, filepath.Join(dest, "bash"), "")
	testutil.Fails(t, harness(t.TempDir(), dest))
	tools := t.TempDir()
	testutil.Write(t, filepath.Join(tools, batsName), "")
	clash := t.TempDir()
	testutil.Write(t, filepath.Join(clash, batsName), "")
	testutil.Fails(t, harness(tools, clash))
}

func TestBusyboxSourceValidation(t *testing.T) {
	sum := strings.Repeat("c", 64)
	good := Source{Name: sourceBusybox, URL: busyboxURL, SHA256: sum, Tree: "busybox-1.37.0", Config: busyboxConfig}
	testutil.NoErr(t, Manifest{Sources: []Source{good}}.Validate())
	both := jsonfilterManifest(sum)
	both.Sources = append(both.Sources, good)
	testutil.NoErr(t, both.Validate())
	testutil.Equal(t, both.Count(), 3)
	bad := []Source{
		{Name: sourceBusybox, URL: busyboxURL, SHA256: sum, Tree: good.Tree},
		{Name: sourceBusybox, URL: busyboxURL, SHA256: sum, Tree: good.Tree, Config: "../busybox.config"},
		{Name: sourceBusybox, URL: "https://busybox.net/downloads/busybox-1.37.0.tar.bz2", SHA256: sum, Tree: good.Tree, Config: busyboxConfig},
		{Name: sourceJSONC, URL: jsoncURL, SHA256: sum, Tree: srcTree, Config: busyboxConfig},
	}
	for _, s := range bad {
		testutil.Is(t, Manifest{Sources: []Source{s}}.Validate(), ErrManifest)
	}
	testutil.Is(t, Manifest{Sources: []Source{good, good}}.Validate(), ErrManifest)
}
