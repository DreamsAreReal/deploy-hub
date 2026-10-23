package selfupdate

import (
	"context"
	"errors"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const host = "vpn"

type hostFixture struct {
	*fixture
	b      Bootstrap
	remote procexectest.Script
	ssh    *procexectest.Fake
}

func newHost(t *testing.T) *hostFixture {
	t.Helper()
	h := &hostFixture{fixture: newFixture(t)}
	line := func(cmd string) string { return SSHBinary + " " + host + " " + cmd }
	h.remote = procexectest.Script{
		line(installedNot): {},
		line(statToken):    {Res: procexec.Result{Stdout: "root:600\n"}},
		line(readToken):    {Res: procexec.Result{Stdout: pat + "\n"}},
		line(stageBinary):  {},
		line(candidateRun): {Res: procexec.Result{Stdout: newCommit + "\n"}},
		line(placeBinary):  {},
		line(dropBinary):   {},
	}
	h.ssh = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) { return h.remote.Handle(c) }}
	src := h.u.Source
	src.Token = ""
	h.b = Bootstrap{Source: src, Host: host, Exec: h.ssh}
	return h
}

func (h *hostFixture) set(cmd string, r procexectest.Reply) {
	h.remote[SSHBinary+" "+host+" "+cmd] = r
}

func TestBootstrapInstallsTheGatedRelease(t *testing.T) {
	h := newHost(t)
	r, err := h.b.Run(context.Background(), "")
	testutil.NoErr(t, err)
	testutil.Equal(t, r.Tag, newTag)
	staged, ok := h.ssh.Find(stageBinary)
	testutil.True(t, ok, "binary staged")
	testutil.Equal(t, staged.Stdin, newBinary)
	testutil.Equal(t, h.ssh.Count(SSHBinary+" "+host+" "+placeBinary), 1)
	testutil.Equal(t, h.ssh.Count(SSHBinary+" "+host+" "+dropBinary), 0)
	for _, a := range h.hub.auth {
		testutil.Equal(t, a, "Bearer "+pat)
	}
}

func TestBootstrapRefusesWithoutAUsableToken(t *testing.T) {
	cases := map[string]struct {
		cmd   string
		reply procexectest.Reply
		want  error
	}{
		"missing file":  {statToken, procexectest.Reply{Err: errors.New("No such file")}, ErrNoToken},
		"empty file":    {readToken, procexectest.Reply{Res: procexec.Result{Stdout: "\n"}}, ErrNoToken},
		"http header":   {readToken, procexectest.Reply{Res: procexec.Result{Stdout: "Authorization: Bearer x"}}, ErrNoToken},
		"open file":     {statToken, procexectest.Reply{Res: procexec.Result{Stdout: "root:644"}}, ErrHostToken},
		"not root":      {statToken, procexectest.Reply{Res: procexec.Result{Stdout: "deploy:600"}}, ErrHostToken},
		"hub installed": {installedNot, procexectest.Reply{Err: errors.New("exit status 1")}, ErrInstalled},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.set(tc.cmd, tc.reply)
			_, err := h.b.Run(context.Background(), "")
			testutil.Is(t, err, tc.want)
			testutil.Equal(t, len(h.hub.auth), 0)
			_, staged := h.ssh.Find(stageBinary)
			testutil.True(t, !staged, "nothing staged")
		})
	}
}

func TestBootstrapReadOnlyTokenFileIsFine(t *testing.T) {
	h := newHost(t)
	h.set(statToken, procexectest.Reply{Res: procexec.Result{Stdout: "root:400"}})
	_, err := h.b.Run(context.Background(), "")
	testutil.NoErr(t, err)
}

func TestBootstrapRefusesWithAWrongToken(t *testing.T) {
	h := newHost(t)
	h.set(readToken, procexectest.Reply{Res: procexec.Result{Stdout: wrongPAT}})
	_, err := h.b.Run(context.Background(), "")
	testutil.Is(t, err, ErrHTTP)
	testutil.Contains(t, err.Error(), "401")
	_, staged := h.ssh.Find(stageBinary)
	testutil.True(t, !staged, "nothing staged")
}

func TestBootstrapRefusesUngatedOrForgedReleases(t *testing.T) {
	cases := map[string]struct {
		mutate func(h *hostFixture)
		want   error
	}{
		"no green run":    {func(h *hostFixture) { h.hub.routes[runsPath] = noRuns }, ErrNotGated},
		"forged binary":   {func(h *hostFixture) { h.hub.routes[binaryPath] = forged }, ErrChecksum},
		"tag not commit":  {func(h *hostFixture) { h.hub.routes[latest] = releaseJSON(h.hub, "v1", newCommit) }, ErrTag},
		"other candidate": {func(h *hostFixture) { h.set(candidateRun, procexectest.Reply{Res: procexec.Result{Stdout: oldCommit}}) }, ErrVersion},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			tc.mutate(h)
			_, err := h.b.Run(context.Background(), "")
			testutil.Is(t, err, tc.want)
			testutil.Equal(t, h.ssh.Count(SSHBinary+" "+host+" "+placeBinary), 0)
		})
	}
}

func TestBootstrapDropsTheCandidateWhenItCannotBePlaced(t *testing.T) {
	for _, cmd := range []string{candidateRun, placeBinary} {
		t.Run(cmd, func(t *testing.T) {
			h := newHost(t)
			h.set(cmd, procexectest.Reply{Err: errors.New("exit status 1")})
			_, err := h.b.Run(context.Background(), "")
			testutil.Fails(t, err)
			testutil.Equal(t, h.ssh.Count(SSHBinary+" "+host+" "+dropBinary), 1)
		})
	}
	h := newHost(t)
	h.set(stageBinary, procexectest.Reply{Err: errors.New("disk full")})
	_, err := h.b.Run(context.Background(), "")
	testutil.Fails(t, err)
	testutil.Equal(t, h.ssh.Count(SSHBinary+" "+host+" "+candidateRun), 0)
}

func TestBootstrapPinsATag(t *testing.T) {
	h := newHost(t)
	h.hub.routes["/repos/"+repo+"/releases/tags/"+newTag] = h.hub.routes[latest]
	delete(h.hub.routes, latest)
	r, err := h.b.Run(context.Background(), newTag)
	testutil.NoErr(t, err)
	testutil.Equal(t, r.Commit, newCommit)
}

func TestBootstrapStopsWhenTheTokenCannotBeRead(t *testing.T) {
	h := newHost(t)
	h.set(readToken, procexectest.Reply{Err: errors.New("Permission denied")})
	_, err := h.b.Run(context.Background(), "")
	testutil.Fails(t, err)
	testutil.Equal(t, len(h.hub.auth), 0)
}
