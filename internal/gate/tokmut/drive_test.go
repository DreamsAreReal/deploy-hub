package tokmut

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/packtest"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	fileJS   = "src/m.js"
	suite    = "suite"
	sourceJS = "export function above(v) {\n  return v > 3;\n}\nexport function never(v) {\n  return v + 1;\n}\n"
)

var errSim = packtest.ErrSim

func project(t *testing.T, files map[string]string) gate.Workspace {
	t.Helper()
	ws := gate.Workspace{
		Src: t.TempDir(), Out: t.TempDir(), Log: slog.New(slog.DiscardHandler),
		Policy: policy.Policy{
			Thresholds: policy.Thresholds{MutationScore: 90},
			Limits:     policy.Limits{MutantFloor: 50 * time.Millisecond, MutantFactor: 1},
		},
	}
	names := make([]string, 0, len(files))
	for name, content := range files {
		testutil.Write(t, filepath.Join(ws.Src, filepath.FromSlash(name)), content)
		names = append(names, name)
	}
	ws.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stdout: strings.Join(names, "\x00") + "\x00"}, nil
	}}
	return ws
}

func outcomes(t *testing.T) Suite {
	t.Helper()
	exit := packtest.ExitError(t, 1)
	return Suite{
		Family:  JavaScript,
		Files:   []string{fileJS},
		Seconds: 0.01,
		Covered: func(_ string, line int) (bool, []string) { return line == 2, []string{suite} },
		Compile: func(_ context.Context, _ int, dir, rel string) error {
			if strings.Contains(testutil.Read(t, filepath.Join(dir, rel)), ">=") {
				return errSim
			}
			return nil
		},
		Test: func(_ context.Context, _ int, dir string, tests []string) error {
			testutil.Equal(t, tests, []string{suite})
			if strings.Contains(testutil.Read(t, filepath.Join(dir, fileJS)), "<=") {
				return exit
			}
			return nil
		},
	}
}

func TestRunClassifiesEveryMutant(t *testing.T) {
	ws := project(t, map[string]string{fileJS: sourceJS, "README.md": ""})
	var mu sync.Mutex
	prepared, cleaned := map[int]bool{}, map[int]bool{}
	s := outcomes(t)
	s.Prepare = func(_ context.Context, w int, dir string) error {
		mu.Lock()
		defer mu.Unlock()
		prepared[w] = true
		testutil.Equal(t, testutil.Read(t, filepath.Join(dir, fileJS)), sourceJS)
		return nil
	}
	s.Cleanup = func(w int) {
		mu.Lock()
		defer mu.Unlock()
		cleaned[w] = true
	}
	rep, err := Run(context.Background(), ws, s)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts, map[mutation.Status]int{
		mutation.StatusKilled: 1, mutation.StatusSurvived: 1, mutation.StatusNotViable: 1, mutation.StatusNotCovered: 2,
	})
	testutil.True(t, len(prepared) > 0, "sandboxes are prepared")
	testutil.Equal(t, len(cleaned), mutation.Workers())
	for i := range mutation.Workers() {
		testutil.Missing(t, ws.Report(SandboxPrefix+strconv.Itoa(i)))
	}
	testutil.Equal(t, testutil.Read(t, filepath.Join(ws.Src, filepath.FromSlash(fileJS))), sourceJS)
}

func TestRunTimeoutsAndErrors(t *testing.T) {
	ws := project(t, map[string]string{fileJS: sourceJS})
	slow := outcomes(t)
	slow.Test = func(ctx context.Context, _ int, _ string, _ []string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	rep, err := Run(context.Background(), ws, slow)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts[mutation.StatusTimedOut], 2)
	broken := outcomes(t)
	broken.Test = func(context.Context, int, string, []string) error { return errSim }
	_, err = Run(context.Background(), ws, broken)
	testutil.Is(t, err, mutation.ErrIncomplete)
	hungry := outcomes(t)
	hungry.Test = func(context.Context, int, string, []string) error {
		return fmt.Errorf("%w: signal: killed", procexec.ErrMemoryLimit)
	}
	rep, err = Run(context.Background(), ws, hungry)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts[mutation.StatusExhausted], 2)
	custom := outcomes(t)
	custom.Test = func(context.Context, int, string, []string) error { return errSim }
	custom.Killed = func(err error) bool { return errors.Is(err, errSim) }
	rep, err = Run(context.Background(), ws, custom)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts[mutation.StatusKilled], 2)
}

func TestRunSandboxFailures(t *testing.T) {
	ws := project(t, map[string]string{fileJS: sourceJS})
	failures := map[string]func(dir string) error{
		"prepare": func(string) error { return errSim },
		"root":    os.RemoveAll,
		"staging": func(dir string) error {
			target := filepath.Join(dir, filepath.FromSlash(fileJS))
			if err := os.Remove(target); err != nil {
				return err
			}
			return os.Mkdir(target, 0o700)
		},
	}
	for name, prepare := range failures {
		t.Run(name, func(t *testing.T) {
			s := outcomes(t)
			s.Prepare = func(_ context.Context, _ int, dir string) error { return prepare(dir) }
			_, err := Run(context.Background(), ws, s)
			testutil.Is(t, err, mutation.ErrIncomplete)
		})
	}
	unlisted := project(t, map[string]string{fileJS: sourceJS})
	unlisted.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }}
	_, err := Run(context.Background(), unlisted, outcomes(t))
	testutil.Is(t, err, mutation.ErrIncomplete)
}

func TestRunRejectsUnreadableSources(t *testing.T) {
	missing := project(t, map[string]string{})
	_, err := Run(context.Background(), missing, outcomes(t))
	testutil.Fails(t, err)
	invalid := project(t, map[string]string{fileJS: "const a = \"\xff\";\n"})
	_, err = Run(context.Background(), invalid, outcomes(t))
	testutil.Contains(t, err.Error(), fileJS)
}

func TestCheckJudgesTheSuite(t *testing.T) {
	ws := project(t, map[string]string{fileJS: sourceJS})
	failing := Check(func(_ context.Context, budgeted gate.Workspace) (Suite, error) {
		testutil.Equal(t, budgeted.MemoryLimit, mutation.MemoryBudget())
		return Suite{}, errSim
	})
	out := failing(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Contains(t, out.Reason, errSim.Error())
	passing := Check(func(context.Context, gate.Workspace) (Suite, error) {
		s := outcomes(t)
		s.Test = func(context.Context, int, string, []string) error { return packtest.ExitError(t, 1) }
		s.Covered = func(string, int) (bool, []string) { return true, []string{suite} }
		s.Compile = func(context.Context, int, string, string) error { return nil }
		return s, nil
	})
	testutil.Equal(t, passing(context.Background(), ws).Values[mutation.ValueScore], float64(100))
}
