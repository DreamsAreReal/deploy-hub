package journal

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func TestFormatParseRoundTrip(t *testing.T) {
	e := Entry{Time: t0, App: "web", Sha7: "abcdef0", Action: ActionDeploy, Result: ResultOK, Seconds: 42, Extra: "smoke=skip"}
	line := e.Format()
	if line != "[2026-10-19T12:00:00Z] web@abcdef0 deploy ok 42s smoke=skip" {
		t.Fatalf("format: %q", line)
	}
	got, ok := Parse(line)
	e.Raw = line
	if !ok || !reflect.DeepEqual(got, e) {
		t.Fatalf("parse: %+v %v", got, ok)
	}
	plain := Entry{Time: t0, App: "-", Sha7: "-", Action: ActionRefuse, Result: "denied(x)"}
	if plain.Format() != "[2026-10-19T12:00:00Z] -@- refuse denied(x) 0s" || plain.Stamp() != "2026-10-19T12:00:00Z" {
		t.Fatalf("plain: %q", plain.Format())
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, line := range []string{"", "garbage", "[notatime] a@b deploy ok 1s", "[2026-10-19T12:00:00Z] a@b deploy ok xs", "[2026-10-19T12:00:00Z] a@b deploy ok 99999999999999999999s"} {
		if _, ok := Parse(line); ok {
			t.Errorf("accepted %q", line)
		}
	}
}

func TestAppendAndEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploys.log")
	j := Journal{Path: path, Now: func() time.Time { return t0 }}
	got, err := j.Entries()
	testutil.NoErr(t, err)
	testutil.True(t, got == nil, "missing journal yields no entries")
	testutil.NoErr(t, j.Append(Entry{App: "a", Sha7: "1111111", Action: ActionDeploy, Result: ResultOK}))
	testutil.Write(t, path, testutil.Read(t, path)+"legacy line\n")
	testutil.NoErr(t, j.Append(Entry{App: "b", Sha7: "2222222", Action: ActionStop, Result: ResultFail}))
	got, err = j.Entries()
	testutil.NoErr(t, err)
	testutil.Equal(t, len(got), 2)
	testutil.Equal(t, got[1].App, "b")
	testutil.True(t, got[1].Time.Equal(t0), "time comes from the clock")
}

func TestJournalIOErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploys.log")
	testutil.Write(t, path, "")
	testutil.Fails(t, (Journal{Path: filepath.Join(path, "x"), Now: time.Now}).Append(Entry{}))
	_, err := (Journal{Path: t.TempDir()}).Entries()
	testutil.Fails(t, err)
	locked := filepath.Join(t.TempDir(), "locked.log")
	testutil.Write(t, locked, "")
	testutil.NoErr(t, os.Chmod(locked, 0))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	_, err = (Journal{Path: locked}).Entries()
	testutil.True(t, errors.Is(err, os.ErrPermission), "unreadable journal reports the open error")
}

func entry(app, sha, action, result string, minute int) Entry {
	return Entry{Time: t0.Add(time.Duration(minute) * time.Minute), App: app, Sha7: sha, Action: action, Result: result}
}

func queryFixture() []Entry {
	return []Entry{
		entry("a", "1111111", ActionDeploy, ResultOK, 1),
		entry("b", "9999999", ActionDeploy, ResultOK, 2),
		entry("a", "2222222", ActionDeploy, ResultFail, 3),
		entry("a", "1111111", ActionRollback, ResultOK, 4),
		entry("a", NoValue, ActionPrune, ResultOK, 5),
		entry("a", "3333333", ActionRedeploy, ResultOK, 6),
		entry("a", "1111111", ActionDeploy, ResultOK, 7),
	}
}

func TestForAppAndTail(t *testing.T) {
	a := ForApp(queryFixture(), "a")
	testutil.Equal(t, len(a), 6)
	tail := Tail(a, 2)
	testutil.Equal(t, len(tail), 2)
	testutil.True(t, tail[1].Time.Equal(t0.Add(7*time.Minute)), "tail keeps the newest")
	testutil.Equal(t, len(Tail(a, 100)), 6)
	testutil.Equal(t, len(Tail(a, 6)), 6)
	testutil.Equal(t, len(Tail(a, 0)), 0)
}

func TestSuccessfulShas(t *testing.T) {
	testutil.Equal(t, SuccessfulShas(ForApp(queryFixture(), "a")), []string{"1111111", "3333333"})
	testutil.Equal(t, len(SuccessfulShas([]Entry{entry("a", "2222222", ActionDeploy, ResultFail, 1)})), 0)
}

func TestLastContainerOp(t *testing.T) {
	a := ForApp(queryFixture(), "a")
	last, ok := LastContainerOp(append(a, entry("a", NoValue, ActionPrune, ResultOK, 8)))
	testutil.True(t, ok && last.Action == ActionDeploy, "last op is the deploy")
	_, found := LastContainerOp([]Entry{entry("a", NoValue, ActionPrune, ResultOK, 1)})
	testutil.True(t, !found, "prune is not a container op")
}

func TestIsContainerOp(t *testing.T) {
	for _, a := range []string{ActionDeploy, ActionRollback, ActionRedeploy, ActionStop} {
		if !IsContainerOp(a) {
			t.Errorf("%s should be a container op", a)
		}
	}
	for _, a := range []string{ActionPrune, ActionRefuse, "notify"} {
		if IsContainerOp(a) {
			t.Errorf("%s should not be a container op", a)
		}
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize("fix:\tbug\r\n\x00\x7fcafé", 120); got != "fix:bugcafé" {
		t.Fatalf("got %q", got)
	}
	if got := Sanitize("éèêëē", 3); got != "éèê" {
		t.Fatalf("limit: %q", got)
	}
	testutil.Equal(t, Sanitize("a b", 3), "a b")
	testutil.Equal(t, Sanitize("abc", 3), "abc")
}
