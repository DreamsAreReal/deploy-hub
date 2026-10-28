package obs

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

type recordingPusher struct {
	got []Sample
}

func (r *recordingPusher) Push(_ context.Context, samples []Sample) {
	r.got = append(r.got, samples...)
}

func TestTextfileKeepsEverySampleOfTheRunUnderItsSource(t *testing.T) {
	dir := t.TempDir()
	at := time.Unix(1800000000, 0)
	file := &Textfile{Dir: dir, Source: "runner-status", Now: func() time.Time { return at }, Log: discardLogger()}
	file.Push(context.Background(), []Sample{{Name: MetricAppUp, Labels: Labels{LabelApp: "tasks"}, Value: 1}})
	file.Push(context.Background(), []Sample{{Name: MetricAppUp, Labels: Labels{LabelApp: "tasks"}, Value: 0}, {Name: MetricCommandDuration, Value: 2.5}})
	testutil.Equal(t, file.Path(), filepath.Join(dir, "deploy-hub-runner-status.prom"))
	testutil.Equal(t, testutil.Read(t, file.Path()), `deploy_hub_app_up{app="tasks",source="runner-status"} 0
deploy_hub_command_duration_seconds{source="runner-status"} 2.5
deploy_hub_textfile_written_timestamp_seconds{source="runner-status"} 1800000000
`)
	entries, err := os.ReadDir(dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(entries), 1)
	var logs bytes.Buffer
	missing := &Textfile{Dir: filepath.Join(dir, "missing"), Source: "prune", Now: time.Now, Log: slog.New(slog.NewJSONHandler(&logs, nil))}
	missing.Push(context.Background(), []Sample{{Name: MetricPruneKept, Value: 3}})
	testutil.Contains(t, logs.String(), "metrics textfile not written")
}

func TestFanoutPushesToEveryPusher(t *testing.T) {
	first, second := &recordingPusher{}, &recordingPusher{}
	Fanout{first, second}.Push(context.Background(), []Sample{{Name: MetricAppsRegistered, Value: 4}})
	testutil.Equal(t, len(first.got)+len(second.got), 2)
}
