package obs

import (
	"context"
	"log/slog"
	"sync/atomic"
)

const (
	ResultOK    = "ok"
	ResultFail  = "fail"
	ResultUsage = "usage"
	ResultPanic = "panic"

	exitOK    = 0
	exitUsage = 2
)

type LevelCounts struct {
	warn  atomic.Int64
	error atomic.Int64
}

func (c *LevelCounts) Warnings() int64 { return c.warn.Load() }

func (c *LevelCounts) Errors() int64 { return c.error.Load() }

func (c *LevelCounts) add(level slog.Level) {
	switch {
	case level >= slog.LevelError:
		c.error.Add(1)
	case level >= slog.LevelWarn:
		c.warn.Add(1)
	}
}

type countingHandler struct {
	next   slog.Handler
	counts *LevelCounts
}

func Counting(next slog.Handler, counts *LevelCounts) slog.Handler {
	return countingHandler{next: next, counts: counts}
}

func (h countingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn || h.next.Enabled(ctx, level)
}

func (h countingHandler) Handle(ctx context.Context, r slog.Record) error {
	h.counts.add(r.Level)
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h countingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return countingHandler{next: h.next.WithAttrs(attrs), counts: h.counts}
}

func (h countingHandler) WithGroup(name string) slog.Handler {
	return countingHandler{next: h.next.WithGroup(name), counts: h.counts}
}

type CommandRun struct {
	Command  string
	ExitCode int
	Panicked bool
	Seconds  float64
	Logs     *LevelCounts
}

func (r CommandRun) Result() string {
	switch {
	case r.Panicked:
		return ResultPanic
	case r.ExitCode == exitOK:
		return ResultOK
	case r.ExitCode == exitUsage:
		return ResultUsage
	default:
		return ResultFail
	}
}

func (r CommandRun) Samples() []Sample {
	cmd := Labels{LabelCommand: r.Command}
	return []Sample{
		{Name: MetricCommandExitCode, Labels: cmd, Value: float64(r.ExitCode)},
		{Name: MetricCommandDuration, Labels: Labels{LabelCommand: r.Command, LabelResult: r.Result()}, Value: r.Seconds},
		{Name: MetricCommandPanicked, Labels: cmd, Value: Bool(r.Panicked)},
		{Name: MetricCommandLogRecords, Labels: Labels{LabelCommand: r.Command, LabelLevel: LevelWarn}, Value: float64(r.Logs.Warnings())},
		{Name: MetricCommandLogRecords, Labels: Labels{LabelCommand: r.Command, LabelLevel: LevelError}, Value: float64(r.Logs.Errors())},
	}
}

func Bool(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
