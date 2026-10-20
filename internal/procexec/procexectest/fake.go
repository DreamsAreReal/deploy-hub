package procexectest

import (
	"context"
	"strings"
	"sync"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

type Handler func(c procexec.Command) (procexec.Result, error)

type Fake struct {
	mu      sync.Mutex
	calls   []procexec.Command
	Handler Handler
}

func (f *Fake) Run(_ context.Context, c procexec.Command) (procexec.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	h := f.Handler
	f.mu.Unlock()
	if h == nil {
		return procexec.Result{}, nil
	}
	res, err := h(c)
	if err != nil && procexec.Output(err) == "" {
		err = &procexec.Error{Err: err, Output: res.Combined()}
	}
	return res, err
}

func (f *Fake) Calls() []procexec.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]procexec.Command(nil), f.calls...)
}

func Line(c procexec.Command) string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

func (f *Fake) Lines() []string {
	calls := f.Calls()
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, Line(c))
	}
	return out
}

func (f *Fake) Find(substr string) (procexec.Command, bool) {
	for _, c := range f.Calls() {
		if strings.Contains(Line(c), substr) {
			return c, true
		}
	}
	return procexec.Command{}, false
}

func (f *Fake) Count(substr string) int {
	n := 0
	for _, l := range f.Lines() {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func LinesOf(r procexec.Runner) []string {
	if f, ok := r.(*Fake); ok {
		return f.Lines()
	}
	return nil
}

func CallsOf(r procexec.Runner) []procexec.Command {
	if f, ok := r.(*Fake); ok {
		return f.Calls()
	}
	return nil
}
