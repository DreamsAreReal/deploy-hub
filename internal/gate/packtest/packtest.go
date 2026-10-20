package packtest

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var (
	ErrSim  = errors.New("simulated")
	ErrPass = errors.New("not handled by the hook")
)

func ExitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.CommandContext(context.Background(), "sh", "-c", `exit "$1"`, "sh", strconv.Itoa(code)).Run()
	testutil.True(t, err != nil, "exit status")
	return err
}

type reply struct {
	out string
	err error
}

type World struct {
	T      *testing.T
	WS     gate.Workspace
	Fake   *procexectest.Fake
	Files  []string
	Hook   func(c procexec.Command, line string) (procexec.Result, error)
	Expand func(content string) string
	Kill   func(src string) error
	Broken func(src string) bool
	mu     sync.Mutex
	rules  map[string]reply
	writes map[string]string
	gitErr error
}

func New(t *testing.T, files map[string]string, pol policy.Policy) *World {
	t.Helper()
	w := &World{T: t, rules: map[string]reply{}, writes: map[string]string{}}
	src := t.TempDir()
	for name, content := range files {
		testutil.Write(t, filepath.Join(src, filepath.FromSlash(name)), content)
		w.Files = append(w.Files, name)
	}
	slices.Sort(w.Files)
	w.Fake = &procexectest.Fake{Handler: w.Handle}
	w.WS = gate.Workspace{Src: src, Out: t.TempDir(), PolicyDir: t.TempDir(), Exec: w.Fake, Log: slog.New(slog.DiscardHandler), Policy: pol}
	return w
}

func (w *World) On(substr, out string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rules[substr] = reply{out: out, err: err}
}

func (w *World) Produce(flag, content string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes[flag] = content
}

func (w *World) FailGit() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gitErr = ErrSim
}

func (w *World) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	clear(w.writes)
}

func (w *World) Read(rel string, c procexec.Command) string {
	return testutil.Read(w.T, filepath.Join(c.Dir, filepath.FromSlash(rel)))
}

func (w *World) Handle(c procexec.Command) (procexec.Result, error) {
	if c.Name == source.GitBinary {
		return w.listing()
	}
	w.produce(c.Args)
	line := procexectest.Line(c)
	if w.Hook != nil {
		if res, err := w.Hook(c, line); !errors.Is(err, ErrPass) {
			return res, err
		}
	}
	return w.Reply(line)
}

func (w *World) listing() (procexec.Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return procexec.Result{Stdout: strings.Join(w.Files, "\x00") + "\x00"}, w.gitErr
}

func (w *World) Reply(line string) (procexec.Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	best := ""
	for substr := range w.rules {
		if strings.Contains(line, substr) && len(substr) > len(best) {
			best = substr
		}
	}
	r := w.rules[best]
	return procexec.Result{Stdout: r.out}, r.err
}

func (w *World) produce(args []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range args {
		for flag, content := range w.writes {
			if target := outputOf(args, i, flag); filepath.IsAbs(target) {
				testutil.Write(w.T, target, w.expand(content))
			}
		}
	}
}

func outputOf(args []string, i int, flag string) string {
	rest, ok := strings.CutPrefix(args[i], flag)
	if !ok {
		return ""
	}
	if target := strings.TrimPrefix(rest, "="); target != "" || i+1 == len(args) {
		return target
	}
	return args[i+1]
}

func (w *World) expand(content string) string {
	if w.Expand == nil {
		return content
	}
	return w.Expand(content)
}

func Breaks[W any](t *testing.T, fresh func(*testing.T) W, run func(W) error, cases map[string]func(W)) {
	t.Helper()
	for want, brk := range cases {
		w := fresh(t)
		brk(w)
		err := run(w)
		testutil.True(t, err != nil, want)
		testutil.Contains(t, err.Error(), want)
	}
}
