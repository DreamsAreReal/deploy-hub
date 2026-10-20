package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var ts = time.UnixMilli(1760000000000)

func TestSampleLineEscapesAndSorts(t *testing.T) {
	s := Sample{Name: "m", Labels: Labels{"b": `x"y`, "a": "p\\q\nr"}, Value: 1.5}
	want := `m{a="p\\q\nr",b="x\"y"} 1.5 1760000000000`
	if got := s.Line(ts); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if got := (Sample{Name: "n", Labels: Labels{}, Value: 3}).Line(ts); got != "n{} 3 1760000000000" {
		t.Fatalf("empty labels: %s", got)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestHTTPPusherSendsPrometheusText(t *testing.T) {
	var body string
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, ctype = string(b), r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	var logs bytes.Buffer
	p := HTTPPusher{URL: srv.URL, Client: srv.Client(), Now: func() time.Time { return ts }, Log: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	p.Push(context.Background(), []Sample{{Name: "a", Labels: Labels{"app": "x"}, Value: 1}, {Name: "b", Labels: Labels{}, Value: 2}})
	if body != "a{app=\"x\"} 1 1760000000000\nb{} 2 1760000000000\n" || ctype != "text/plain" {
		t.Fatalf("body=%q ctype=%q", body, ctype)
	}
	if !strings.Contains(logs.String(), "metrics pushed") {
		t.Fatalf("missing debug log: %s", logs.String())
	}
}

func TestHTTPPusherLogsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	for _, url := range []string{srv.URL, "http://127.0.0.1:1/unreachable", "http://bad host/"} {
		var logs bytes.Buffer
		p := HTTPPusher{URL: url, Client: srv.Client(), Now: time.Now, Log: slog.New(slog.NewJSONHandler(&logs, nil))}
		p.Push(context.Background(), []Sample{{Name: "a", Labels: Labels{}, Value: 1}})
		if !strings.Contains(logs.String(), "metrics push failed") {
			t.Fatalf("%s: no failure log: %s", url, logs.String())
		}
	}
}

func TestHTTPPusherSkipsWhenDisabledOrEmpty(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer srv.Close()
	HTTPPusher{URL: "", Client: srv.Client(), Now: time.Now, Log: discardLogger()}.Push(context.Background(), []Sample{{Name: "a"}})
	HTTPPusher{URL: srv.URL, Client: srv.Client(), Now: time.Now, Log: discardLogger()}.Push(context.Background(), nil)
	Discard{}.Push(context.Background(), []Sample{{Name: "a"}})
	if calls != 0 {
		t.Fatalf("unexpected calls: %d", calls)
	}
}

func TestJournalSamples(t *testing.T) {
	at := func(m int) time.Time { return time.Unix(1000+int64(m)*60, 0) }
	entries := []journal.Entry{
		{Time: at(1), App: "b", Action: journal.ActionDeploy, Result: journal.ResultOK},
		{Time: at(2), App: "a", Action: journal.ActionDeploy, Result: journal.ResultFail},
		{Time: at(3), App: "a", Action: journal.ActionDeploy, Result: journal.ResultOK},
		{Time: at(4), App: "a", Action: journal.ActionDeploy, Result: journal.ResultOK},
		{Time: at(5), App: "-", Action: journal.ActionRefuse, Result: "denied(x)"},
		{Time: at(6), App: "-", Action: journal.ActionRefuse, Result: "denied(y)"},
		{Time: at(7), App: "a", Action: journal.ActionRollback, Result: journal.ResultOK},
		{Time: at(8), App: "a", Action: journal.ActionDeploy, Result: journal.ResultFail},
		{Time: at(9), App: "c", Action: journal.ActionDeploy, Result: journal.ResultFail},
		{Time: at(10), App: "b", Action: journal.ActionDeploy, Result: journal.ResultFail},
	}
	samples := JournalSamples(entries, []string{"a", "c"})
	testutil.Equal(t, cap(samples), len(samples))
	lines := make([]string, 0, len(samples))
	for _, s := range samples {
		lines = append(lines, s.Line(ts))
	}
	want := []string{
		`deploy_hub_operations_total{action="refuse",app="-",result="denied"} 2 1760000000000`,
		`deploy_hub_last_operation_timestamp_seconds{action="refuse",app="-",result="denied"} 1360 1760000000000`,
		`deploy_hub_operations_total{action="deploy",app="a",result="fail"} 2 1760000000000`,
		`deploy_hub_last_operation_timestamp_seconds{action="deploy",app="a",result="fail"} 1480 1760000000000`,
		`deploy_hub_operations_total{action="deploy",app="a",result="ok"} 2 1760000000000`,
		`deploy_hub_last_operation_timestamp_seconds{action="deploy",app="a",result="ok"} 1240 1760000000000`,
		`deploy_hub_operations_total{action="rollback",app="a",result="ok"} 1 1760000000000`,
		`deploy_hub_last_operation_timestamp_seconds{action="rollback",app="a",result="ok"} 1420 1760000000000`,
		`deploy_hub_operations_total{action="deploy",app="b",result="fail"} 1 1760000000000`,
		`deploy_hub_last_operation_timestamp_seconds{action="deploy",app="b",result="fail"} 1600 1760000000000`,
		`deploy_hub_operations_total{action="deploy",app="b",result="ok"} 1 1760000000000`,
		`deploy_hub_last_operation_timestamp_seconds{action="deploy",app="b",result="ok"} 1060 1760000000000`,
		`deploy_hub_operations_total{action="deploy",app="c",result="fail"} 1 1760000000000`,
		`deploy_hub_last_operation_timestamp_seconds{action="deploy",app="c",result="fail"} 1540 1760000000000`,
		`deploy_hub_operations_total{action="deploy",app="c",result="ok"} 0 1760000000000`,
		`deploy_hub_last_incident_timestamp_seconds{app="a"} 1480 1760000000000`,
		`deploy_hub_last_incident_timestamp_seconds{app="b"} 1600 1760000000000`,
		`deploy_hub_last_incident_timestamp_seconds{app="c"} 1540 1760000000000`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestKeyOrdering(t *testing.T) {
	testutil.Equal(t, compareKeys(opKey{app: "a", action: "x", result: "z"}, opKey{app: "b"}), -1)
	testutil.Equal(t, compareKeys(opKey{app: "a", action: "a", result: "z"}, opKey{app: "a", action: "b"}), -1)
	testutil.Equal(t, compareKeys(opKey{app: "a", action: "a", result: "b"}, opKey{app: "a", action: "a", result: "a"}), 1)
	same := opKey{app: "a", action: "b", result: "c"}
	testutil.Equal(t, compareKeys(same, same), 0)
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{{"debug", slog.LevelDebug}, {" WARN ", slog.LevelWarn}, {"error", slog.LevelError}, {"", slog.LevelInfo}, {"nope", slog.LevelInfo}}
	for _, c := range cases {
		if got := ParseLevel(c.in); got != c.want {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}

func TestNewLoggerWritesJSON(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelInfo, "runner", &LevelCounts{}).Info("hello", "app", "x")
	NewLogger(&buf, slog.LevelInfo, "runner", &LevelCounts{}).Debug("hidden")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not one JSON record: %q", buf.String())
	}
	if rec["msg"] != "hello" || rec["service"] != "deploy-hub" || rec["command"] != "runner" || rec["app"] != "x" {
		t.Fatalf("record: %v", rec)
	}
}

func TestOpenLogFileCreatesAndRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "runner.log")
	f, err := OpenLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, werr := f.Write(bytes.Repeat([]byte("x"), maxLogBytes+1))
	testutil.NoErr(t, werr)
	testutil.NoErr(t, f.Close())
	f2, err := OpenLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f2.Close() }()
	if info, err := os.Stat(path + rotatedExt); err != nil || info.Size() != maxLogBytes+1 {
		t.Fatalf("rotated: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 || info.Mode().Perm() != logFilePerm {
		t.Fatalf("fresh file: %v", err)
	}
}

func TestOpenLogFileErrors(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLogFile(filepath.Join(blocker, "x.log")); err == nil {
		t.Fatal("mkdir under a file must fail")
	}
	big := filepath.Join(base, "big.log")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), maxLogBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(big+rotatedExt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(big+rotatedExt, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLogFile(big); err == nil {
		t.Fatal("rotation onto a non-empty dir must fail")
	}
}
