package sqlfiles

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	schema    = "store/schema.sql"
	migration = "store/002.sql"
	fixture   = "store/testdata/old.sql"
)

var errSim = errors.New("simulated")

func unparsable(file string, line int) string {
	return `{"filepath":"` + file + `","violations":[{"code":"PRS","start_line_no":` + strconv.Itoa(line) +
		`,"description":"unparsable"},{"code":"LT01","start_line_no":1,"description":"spacing"}]}`
}

func clean(file string) string {
	return `{"filepath":"` + file + `","violations":[{"code":"LT01","start_line_no":2,"description":"spacing"}]}`
}

func repo(t *testing.T, sqlfluff func(dialect string, files []string) (procexec.Result, error), files ...string) gate.Workspace {
	t.Helper()
	ws := gate.Workspace{Src: t.TempDir(), Out: t.TempDir(), PolicyDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	listing := strings.Join(files, "\x00") + "\x00"
	ws.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		switch {
		case c.Name == source.GitBinary:
			return procexec.Result{Stdout: listing}, nil
		case c.Args[0] == cmdRun:
			i := slices.Index(c.Args, "--dialect")
			return sqlfluff(c.Args[i+1], c.Args[slices.Index(c.Args, "--disable-progress-bar")+1:])
		}
		return procexec.Result{}, nil
	}}
	return ws
}

func TestLintTriesDialectsUntilEveryFileParses(t *testing.T) {
	var asked []string
	ws := repo(t, func(dialect string, files []string) (procexec.Result, error) {
		asked = append(asked, dialect+":"+strings.Join(files, ","))
		reports := make([]string, 0, len(files))
		for _, f := range files {
			switch {
			case f == schema && dialect == "postgres":
				reports = append(reports, clean(f))
			case f == schema || f == migration:
				reports = append(reports, unparsable(f, 4+len(dialect)-len("sqlite")))
			}
		}
		return procexec.Result{Stdout: "[" + strings.Join(reports, ",") + "]"}, nil
	}, schema, migration, fixture, "README.md")
	out := Lint(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Reason, reasonLint)
	testutil.Equal(t, out.Findings, []string{migration + ":4 PRS unparsable"})
	testutil.Equal(t, asked, []string{
		"sqlite:" + schema + "," + migration, "postgres:" + schema + "," + migration,
		"mysql:" + migration, "tsql:" + migration, "ansi:" + migration,
	})
	calls := procexectest.CallsOf(ws.Exec)
	testutil.Equal(t, calls[1].Args, []string{"venv", flagQuiet, flagClear, ws.Report(venvDir)})
	testutil.Equal(t, calls[2].Args[len(calls[2].Args)-1], filepath.Join(ws.PolicyDir, policySub, toolsLock))
	testutil.Equal(t, calls[2].Env, []string{envCache + ws.Report(cacheDir), envNoProgress})
	testutil.Equal(t, calls[3].Args[:6], []string{"run", flagQuiet, "--no-project", flagPython, python(ws), "--"})
}

func TestLintPassesWhenTheFirstDialectParses(t *testing.T) {
	calls := 0
	ws := repo(t, func(_ string, files []string) (procexec.Result, error) {
		calls++
		return procexec.Result{Stdout: "[" + clean(files[0]) + "]"}, nil
	}, schema)
	testutil.Equal(t, Lint(context.Background(), ws).Status, gate.StatusPass)
	testutil.Equal(t, calls, 1)
	testutil.Equal(t, Lint(context.Background(), ws).Status, gate.StatusPass)
	testutil.Equal(t, len(procexectest.CallsOf(ws.Exec)), 6)
}

func TestLintWithoutSQLInstallsNothing(t *testing.T) {
	ws := repo(t, nil, fixture, "main.go")
	testutil.Equal(t, Lint(context.Background(), ws).Status, gate.StatusPass)
	testutil.Equal(t, len(procexectest.CallsOf(ws.Exec)), 1)
}

func TestLintFailures(t *testing.T) {
	garbled := repo(t, func(string, []string) (procexec.Result, error) { return procexec.Result{Stdout: "oops"}, nil }, schema)
	testutil.Contains(t, Lint(context.Background(), garbled).Reason, sqlfluff, "oops")
	crashed := repo(t, func(string, []string) (procexec.Result, error) { return procexec.Result{}, errSim }, schema)
	testutil.Contains(t, Lint(context.Background(), crashed).Reason, "uv run")
	unlisted := repo(t, nil, schema)
	unlisted.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }}
	testutil.Contains(t, Lint(context.Background(), unlisted).Reason, "git ls-files")
	noVenv := repo(t, nil, schema)
	noVenv.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		if c.Name == source.GitBinary {
			return procexec.Result{Stdout: schema}, nil
		}
		return procexec.Result{}, errSim
	}}
	testutil.Contains(t, Lint(context.Background(), noVenv).Reason, "uv venv")
	noLock := repo(t, nil, schema)
	noLock.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		if c.Name == source.GitBinary {
			return procexec.Result{Stdout: schema}, nil
		}
		if c.Args[0] == "pip" {
			return procexec.Result{}, errSim
		}
		return procexec.Result{}, nil
	}}
	testutil.Contains(t, Lint(context.Background(), noLock).Reason, "uv pip")
}

func TestNotApplicableChecks(t *testing.T) {
	testutil.Equal(t, NoCode(context.Background(), gate.Workspace{}).Status, gate.StatusNA)
	testutil.Equal(t, NoCode(context.Background(), gate.Workspace{}).Reason, reasonNoCode)
	testutil.Equal(t, NoDeps(context.Background(), gate.Workspace{}).Reason, reasonNoDeps)
	passed, err := Passed(context.Background(), gate.Workspace{})
	testutil.NoErr(t, err)
	testutil.Equal(t, len(passed), 0)
}
