package gate

import (
	"context"
	"slices"
	"sync"
)

const CheckTests = "tests"

func (ws Workspace) WaitFor(ctx context.Context, check string) error {
	if ws.wait == nil {
		return nil
	}
	return ws.wait(ctx, check)
}

func (r Runner) runAll(ctx context.Context, ws Workspace, subj Subject) []Result {
	results := make([]Result, len(r.Checks))
	if ws.Policy.Limits.ParallelChecks <= 1 {
		for i, c := range r.Checks {
			results[i] = r.runOne(ctx, ws, subj, c)
		}
		return results
	}
	s := &scheduler{runner: r, ws: ws, subj: subj, results: results, done: make(map[string]chan struct{}, len(r.Checks)), free: ws.Policy.Limits.ParallelChecks}
	s.turn = sync.NewCond(&s.mu)
	for _, c := range r.Checks {
		s.done[c.Name] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for i := range r.Checks {
		wg.Go(func() { s.run(ctx, i) })
	}
	wg.Wait()
	return results
}

type scheduler struct {
	runner  Runner
	ws      Workspace
	subj    Subject
	results []Result
	done    map[string]chan struct{}
	mu      sync.Mutex
	turn    *sync.Cond
	free    int
	queue   []int
}

func (s *scheduler) run(ctx context.Context, i int) {
	c := s.runner.Checks[i]
	defer close(s.done[c.Name])
	for _, name := range c.After {
		_ = s.await(ctx, name)
	}
	s.acquire(i)
	defer s.release()
	ws := s.ws
	ws.wait = func(ctx context.Context, check string) error {
		s.release()
		defer s.acquire(i)
		return s.await(ctx, check)
	}
	s.results[i] = s.runner.runOne(ctx, ws, s.subj, c)
}

func (s *scheduler) await(ctx context.Context, check string) error {
	done, ok := s.done[check]
	if !ok {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *scheduler) acquire(i int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, _ := slices.BinarySearch(s.queue, i)
	s.queue = slices.Insert(s.queue, at, i)
	for s.free == 0 || s.queue[0] != i {
		s.turn.Wait()
	}
	s.queue = s.queue[1:]
	s.free--
	s.turn.Broadcast()
}

func (s *scheduler) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.free++
	s.turn.Broadcast()
}
