package obs

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestCountingHandlerCountsEveryWarningAndError(t *testing.T) {
	var buf bytes.Buffer
	counts := &LevelCounts{}
	log := NewLogger(&buf, slog.LevelError, "runner", counts)
	log.Debug("debug")
	log.Info("info")
	log.Warn("hidden warning")
	log.With("app", "x").WithGroup("g").Warn("grouped warning")
	log.Error("error")
	log.Log(t.Context(), slog.LevelError+4, "fatal")
	testutil.Equal(t, counts.Warnings(), int64(2))
	testutil.Equal(t, counts.Errors(), int64(2))
	testutil.Excludes(t, buf.String(), "hidden warning", "grouped warning", `"msg":"info"`)
	testutil.Contains(t, buf.String(), `"msg":"error"`, `"msg":"fatal"`)
	testutil.Equal(t, strings.Count(buf.String(), "\n"), 2)
}

func TestCountingHandlerKeepsLowerLevelsWhenEnabled(t *testing.T) {
	var buf bytes.Buffer
	counts := &LevelCounts{}
	log := NewLogger(&buf, slog.LevelDebug, "gate", counts)
	log.Debug("debug")
	log.Info("info")
	testutil.Contains(t, buf.String(), `"msg":"debug"`, `"msg":"info"`)
	testutil.Equal(t, counts.Warnings()+counts.Errors(), int64(0))
	h := Counting(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}), counts)
	testutil.True(t, h.Enabled(t.Context(), slog.LevelWarn), "warnings reach the counter")
	testutil.True(t, !h.Enabled(t.Context(), slog.LevelInfo), "info stays filtered")
}

func TestCommandRunResult(t *testing.T) {
	cases := []struct {
		run  CommandRun
		want string
	}{
		{CommandRun{ExitCode: 0}, ResultOK},
		{CommandRun{ExitCode: 1}, ResultFail},
		{CommandRun{ExitCode: 2}, ResultUsage},
		{CommandRun{ExitCode: 3}, ResultFail},
		{CommandRun{ExitCode: 1, Panicked: true}, ResultPanic},
		{CommandRun{ExitCode: 0, Panicked: true}, ResultPanic},
	}
	for _, c := range cases {
		testutil.Equal(t, c.run.Result(), c.want)
	}
}

func TestCommandRunSamples(t *testing.T) {
	counts := &LevelCounts{}
	log := NewLogger(&bytes.Buffer{}, slog.LevelInfo, "prune", counts)
	log.Warn("w")
	log.Error("e1")
	log.Error("e2")
	run := CommandRun{Command: "prune", ExitCode: 1, Panicked: true, Seconds: 2.5, Logs: counts}
	samples := run.Samples()
	lines := make([]string, 0, len(samples))
	for _, s := range samples {
		lines = append(lines, s.Line(ts))
	}
	want := []string{
		`deploy_hub_command_exit_code{command="prune"} 1 1760000000000`,
		`deploy_hub_command_duration_seconds{command="prune",result="panic"} 2.5 1760000000000`,
		`deploy_hub_command_panicked{command="prune"} 1 1760000000000`,
		`deploy_hub_command_log_records{command="prune",level="warn"} 1 1760000000000`,
		`deploy_hub_command_log_records{command="prune",level="error"} 2 1760000000000`,
	}
	testutil.Equal(t, strings.Join(lines, "\n"), strings.Join(want, "\n"))
}

func TestBool(t *testing.T) {
	testutil.Equal(t, Bool(true), 1.0)
	testutil.Equal(t, Bool(false), 0.0)
}
