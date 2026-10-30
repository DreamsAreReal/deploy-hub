package obs

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	textfilePrefix = "deploy-hub-"
	textfileExt    = ".prom"
	textfileTemp   = ".tmp"
	textfileMode   = 0o644
)

type Textfile struct {
	Dir     string
	Source  string
	Now     func() time.Time
	Log     *slog.Logger
	mu      sync.Mutex
	samples map[string]Sample
}

func (t *Textfile) Push(_ context.Context, samples []Sample) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.samples == nil {
		t.samples = map[string]Sample{}
	}
	for _, s := range samples {
		t.add(s)
	}
	t.add(Sample{Name: MetricTextfileWritten, Value: float64(t.Now().Unix())})
	if err := t.write(); err != nil {
		t.Log.Warn("metrics textfile not written", "error", err.Error(), "dir", t.Dir)
	}
}

func (t *Textfile) add(s Sample) {
	labels := maps.Clone(s.Labels)
	if labels == nil {
		labels = Labels{}
	}
	labels[LabelSource] = t.Source
	labeled := Sample{Name: s.Name, Labels: labels, Value: s.Value}
	t.samples[labeled.series()] = labeled
}

func (t *Textfile) Path() string {
	return filepath.Join(t.Dir, textfilePrefix+t.Source+textfileExt)
}

func (t *Textfile) write() error {
	lines := make([]string, 0, len(t.samples))
	for series, s := range t.samples {
		lines = append(lines, series+" "+formatValue(s.Value))
	}
	slices.Sort(lines)
	tmp := t.Path() + textfileTemp
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), textfileMode); err != nil {
		return err
	}
	return os.Rename(tmp, t.Path())
}

type Fanout []Pusher

func (f Fanout) Push(ctx context.Context, samples []Sample) {
	for _, p := range f {
		p.Push(ctx, samples)
	}
}
