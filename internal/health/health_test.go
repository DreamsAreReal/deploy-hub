package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	healthyName   = "ok"
	unhealthyName = "bad"
	deadName      = "dead"
)

type fakeDocker struct {
	health map[string]string
	state  map[string]string
}

func (f fakeDocker) HealthStatus(_ context.Context, c string) string { return f.health[c] }
func (f fakeDocker) State(_ context.Context, c string) string        { return f.state[c] }

type fakeHTTP struct {
	okAfter int
	calls   int
	urls    []string
}

func (f *fakeHTTP) HTTPOK(_ context.Context, url string, _ time.Duration) bool {
	f.calls++
	f.urls = append(f.urls, url)
	return f.okAfter > 0 && f.calls >= f.okAfter
}

func gate(d fakeDocker, h *fakeHTTP) (Gate, *int) {
	sleeps := 0
	return Gate{Docker: d, HTTP: h, Timeout: 3 * Interval, Interval: Interval, Sleep: func(ctx context.Context, _ time.Duration) error {
		sleeps++
		return ctx.Err()
	}}, &sleeps
}

func TestHTTPModeRetriesUntilHealthy(t *testing.T) {
	h := &fakeHTTP{okAfter: 2}
	g, sleeps := gate(fakeDocker{}, h)
	if !g.Wait(context.Background(), Target{Mode: registry.HealthHTTP, Port: "9001", Path: "/h"}) {
		t.Fatal("expected healthy")
	}
	if *sleeps != 1 || h.urls[0] != "http://127.0.0.1:9001/h" {
		t.Fatalf("sleeps=%d urls=%v", *sleeps, h.urls)
	}
}

func TestHTTPModeTimesOut(t *testing.T) {
	h := &fakeHTTP{}
	g, sleeps := gate(fakeDocker{}, h)
	if g.Wait(context.Background(), Target{Mode: registry.HealthHTTP, Port: "1"}) {
		t.Fatal("expected unhealthy")
	}
	if h.calls != 3 || *sleeps != 3 {
		t.Fatalf("calls=%d sleeps=%d", h.calls, *sleeps)
	}
}

func TestComposeMode(t *testing.T) {
	g, _ := gate(fakeDocker{health: map[string]string{healthyName: StatusHealthy, unhealthyName: "unhealthy"}}, &fakeHTTP{})
	if !g.Wait(context.Background(), Target{Mode: registry.HealthCompose, Container: healthyName}) {
		t.Fatal("healthy container")
	}
	if g.Wait(context.Background(), Target{Mode: registry.HealthCompose, Container: unhealthyName}) {
		t.Fatal("unhealthy container")
	}
}

func TestProcessModeNeedsOneTick(t *testing.T) {
	g, sleeps := gate(fakeDocker{state: map[string]string{"c": StatusRunning, deadName: "exited"}}, &fakeHTTP{})
	if !g.Wait(context.Background(), Target{Mode: registry.HealthProcess, Container: "c"}) || *sleeps != 1 {
		t.Fatalf("process mode must pass after exactly one interval, sleeps=%d", *sleeps)
	}
	if g.Wait(context.Background(), Target{Mode: registry.HealthProcess, Container: deadName}) {
		t.Fatal("exited container")
	}
}

func TestUnknownModeAndCancel(t *testing.T) {
	g, _ := gate(fakeDocker{}, &fakeHTTP{okAfter: 1})
	if g.Wait(context.Background(), Target{Mode: "weird"}) {
		t.Fatal("unknown mode must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g.Wait(ctx, Target{Mode: registry.HealthCompose}) {
		t.Fatal("cancelled gate must fail")
	}
}

func TestSleep(t *testing.T) {
	if err := Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancel, got %v", err)
	}
}
