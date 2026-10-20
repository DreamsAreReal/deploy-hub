package obs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	pushTimeout     = 3 * time.Second
	contentType     = "text/plain"
	firstBadStatus  = 300
	labelEscapeFrom = `\`
)

var ErrPushRejected = errors.New("metrics push rejected")

type Labels map[string]string

type Sample struct {
	Name   string
	Labels Labels
	Value  float64
}

func (s Sample) Line(ts time.Time) string {
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+`="`+escape(s.Labels[k])+`"`)
	}
	value := strconv.FormatFloat(s.Value, 'f', -1, 64)
	return fmt.Sprintf("%s{%s} %s %d", s.Name, strings.Join(parts, ","), value, ts.UnixMilli())
}

func escape(v string) string {
	r := strings.NewReplacer(labelEscapeFrom, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

type Pusher interface {
	Push(ctx context.Context, samples []Sample)
}

type HTTPPusher struct {
	URL    string
	Client *http.Client
	Now    func() time.Time
	Log    *slog.Logger
}

func (p HTTPPusher) Push(ctx context.Context, samples []Sample) {
	if p.URL == "" || len(samples) == 0 {
		return
	}
	if err := p.send(ctx, samples); err != nil {
		p.Log.Warn("metrics push failed", "error", err.Error(), "samples", len(samples))
		return
	}
	p.Log.Debug("metrics pushed", "samples", len(samples))
}

func (p HTTPPusher) send(ctx context.Context, samples []Sample) error {
	ts := p.Now()
	var body bytes.Buffer
	for _, s := range samples {
		body.WriteString(s.Line(ts) + "\n")
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= firstBadStatus {
		return fmt.Errorf("%w: status %d", ErrPushRejected, resp.StatusCode)
	}
	return nil
}

type Discard struct{}

func (Discard) Push(context.Context, []Sample) {}
