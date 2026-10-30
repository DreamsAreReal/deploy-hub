package gate

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	waitLimit     = 5 * time.Second
	checkBranches = "branches"
	parallelSlots = 2
)

func runParallel(ctx context.Context, t *testing.T, checks ...Check) Verdict {
	t.Helper()
	var logs bytes.Buffer
	ws := workspace(t, &logs)
	ws.Policy.Limits.ParallelChecks = parallelSlots
	return Runner{Now: time.Now, Log: ws.Log, Checks: checks}.Run(ctx, ws, Subject{Own: true})
}

func within(ch <-chan struct{}, failure string) Outcome {
	select {
	case <-ch:
		return Pass(nil)
	case <-time.After(waitLimit):
		return Fail(failure)
	}
}

func TestParallelChecksRunTogetherAndKeepTheirOrder(t *testing.T) {
	a, b := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var order []string
	note := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}
	v := runParallel(context.Background(), t,
		Check{Name: "a", Run: func(context.Context, Workspace) Outcome { defer note("a"); close(a); return within(b, "ran alone") }},
		Check{Name: "b", Run: func(context.Context, Workspace) Outcome { defer note("b"); close(b); return within(a, "ran alone") }},
		Check{Name: "c", After: []string{"a", "b"}, Run: func(context.Context, Workspace) Outcome { note("c"); return Pass(nil) }},
	)
	testutil.True(t, v.Passed, "a and b ran at the same time")
	testutil.Equal(t, []string{v.Results[0].Check, v.Results[1].Check, v.Results[2].Check}, []string{"a", "b", "c"})
	testutil.Equal(t, order[2], "c")
}

func TestParallelChecksStayWithinTheLimit(t *testing.T) {
	var mu sync.Mutex
	running, peak := 0, 0
	busy := func(context.Context, Workspace) Outcome {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return Pass(nil)
	}
	checks := make([]Check, 6)
	for i := range checks {
		checks[i] = Check{Name: string(rune('a' + i)), Run: busy}
	}
	v := runParallel(context.Background(), t, checks...)
	testutil.True(t, v.Passed && peak <= parallelSlots, "no more than two checks at once")
}

func TestWaitForGivesTheSlotBack(t *testing.T) {
	release := make(chan struct{})
	v := runParallel(context.Background(), t,
		Check{Name: CheckTests, Run: func(context.Context, Workspace) Outcome { return within(release, "never released") }},
		Check{Name: checkBranches, Run: func(ctx context.Context, ws Workspace) Outcome {
			return Judge(ws.WaitFor(ctx, CheckTests) == nil, "the tests never finished", nil, nil)
		}},
		Check{Name: "other", Run: func(context.Context, Workspace) Outcome { close(release); return Pass(nil) }},
	)
	testutil.True(t, v.Passed, "the waiting check let the third one run")
}

func TestWaitForIsFreeOutsideTheScheduler(t *testing.T) {
	testutil.NoErr(t, Workspace{}.WaitFor(context.Background(), CheckTests))
	v := runParallel(context.Background(), t, Check{Name: "only", Run: func(ctx context.Context, ws Workspace) Outcome {
		return Judge(ws.WaitFor(ctx, "missing") == nil, "an unknown check blocked", nil, nil)
	}})
	testutil.True(t, v.Passed, "an unknown check is no dependency")
}

func TestWaitForStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	var waited error
	v := runParallel(ctx, t,
		Check{Name: CheckTests, Run: func(context.Context, Workspace) Outcome { return within(release, "never released") }},
		Check{Name: checkBranches, Run: func(ctx context.Context, ws Workspace) Outcome {
			cancel()
			waited = ws.WaitFor(ctx, CheckTests)
			close(release)
			return Pass(nil)
		}},
	)
	testutil.True(t, waited != nil && !v.Passed, "a cancelled gate stops waiting")
}
