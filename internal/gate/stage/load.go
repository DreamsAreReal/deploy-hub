package stage

import (
	"context"
	"io"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
)

const (
	ValueRequests   = "requests"
	ValueRPS        = "achieved_rps"
	ValueP95        = "p95_ms"
	ValueP99        = "p99_ms"
	ValueErrors     = "error_percent"
	ValueDropped    = "dropped"
	ValueRestarts   = "restarts"
	purposeLoad     = "load"
	requestTimeout  = 10 * time.Second
	p95             = 0.95
	p99             = 0.99
	percentScale    = 100
	reasonLoad      = "the app does not hold the load profile within its production limits"
	reasonNoTraffic = "no request completed"
	reasonUnstable  = "the container crashed, restarted or ran out of memory under load"
	reasonSlow      = "p95 latency is above the profile limit"
	reasonErrors    = "error rate is above the profile limit"
)

type sample struct {
	latency time.Duration
	ok      bool
}

type LoadResult struct {
	Requests int
	Failed   int
	Dropped  int
	P95      time.Duration
	P99      time.Duration
	Elapsed  time.Duration
}

func (r LoadResult) ErrorPercent() float64 {
	total := r.Requests + r.Dropped
	if total == 0 {
		return percentScale
	}
	return float64(r.Failed+r.Dropped) * percentScale / float64(total)
}

func (r LoadResult) RPS() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Requests) / r.Elapsed.Seconds()
}

type Generator struct {
	Client      *http.Client
	URLs        []string
	Rate        int
	Duration    time.Duration
	Concurrency int
	mu          sync.Mutex
	samples     []sample
	dropped     int
}

func Load(ctx context.Context, ws gate.Workspace) gate.Outcome {
	if ws.Image == "" {
		return gate.Fail("no image was provided for an app with an HTTP surface")
	}
	st, err := Start(ctx, ws, purposeLoad)
	if err != nil {
		return gate.Fail(err.Error())
	}
	defer st.Stop(ctx)
	prof := ws.App.Load
	urls := make([]string, 0, len(prof.Paths))
	for _, p := range prof.Paths {
		urls = append(urls, st.URL(p))
	}
	g := &Generator{Client: &http.Client{Timeout: requestTimeout}, URLs: urls, Rate: prof.Rate, Duration: prof.Duration, Concurrency: prof.Concurrency}
	res := g.Run(ctx)
	state, err := st.State(ctx)
	if err != nil {
		return gate.Fail(err.Error())
	}
	return Evaluate(res, state, prof)
}

func Evaluate(res LoadResult, state State, prof policy.LoadProfile) gate.Outcome {
	values := map[string]float64{
		ValueRequests: float64(res.Requests), ValueRPS: res.RPS(), ValueDropped: float64(res.Dropped),
		ValueP95: ms(res.P95), ValueP99: ms(res.P99), ValueErrors: res.ErrorPercent(), ValueRestarts: float64(state.Restarts),
	}
	var findings []string
	if res.Requests == 0 {
		findings = append(findings, reasonNoTraffic)
	}
	if !state.Running || state.OOM || state.Restarts > 0 {
		findings = append(findings, reasonUnstable)
	}
	if res.P95 > prof.P95 {
		findings = append(findings, reasonSlow)
	}
	if res.ErrorPercent() > prof.MaxErrors {
		findings = append(findings, reasonErrors)
	}
	return gate.Judge(len(findings) == 0, reasonLoad, values, findings)
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func (g *Generator) Run(ctx context.Context) LoadResult {
	started := time.Now()
	ticker := time.NewTicker(time.Second / time.Duration(g.Rate))
	defer ticker.Stop()
	stop := time.NewTimer(g.Duration)
	defer stop.Stop()
	sem := make(chan struct{}, g.Concurrency)
	var wg sync.WaitGroup
	for i := 0; g.tick(ctx, ticker, stop); i++ {
		g.fire(ctx, sem, &wg, g.URLs[i%len(g.URLs)])
	}
	wg.Wait()
	return summarize(g.samples, g.dropped, time.Since(started))
}

func (g *Generator) tick(ctx context.Context, ticker *time.Ticker, stop *time.Timer) bool {
	select {
	case <-ctx.Done():
		return false
	case <-stop.C:
		return false
	case <-ticker.C:
		return true
	}
}

func (g *Generator) fire(ctx context.Context, sem chan struct{}, wg *sync.WaitGroup, url string) {
	select {
	case sem <- struct{}{}:
	default:
		g.mu.Lock()
		g.dropped++
		g.mu.Unlock()
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s := g.hit(ctx, url)
		<-sem
		g.mu.Lock()
		g.samples = append(g.samples, s)
		g.mu.Unlock()
	}()
}

func (g *Generator) hit(ctx context.Context, url string) sample {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return sample{latency: time.Since(started)}
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return sample{latency: time.Since(started)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return sample{latency: time.Since(started), ok: resp.StatusCode < firstBadStatus}
}

func summarize(samples []sample, dropped int, elapsed time.Duration) LoadResult {
	res := LoadResult{Requests: len(samples), Dropped: dropped, Elapsed: elapsed}
	lat := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		lat = append(lat, s.latency)
		if !s.ok {
			res.Failed++
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	res.P95, res.P99 = percentile(lat, p95), percentile(lat, p99)
	return res
}

func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[max(0, idx)]
}
