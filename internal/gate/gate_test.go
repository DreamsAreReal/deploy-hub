package gate

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var errTool = errors.New("exit status 1")

func clock() func() time.Time {
	t := time.Unix(1_800_000_000, 0)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

func workspace(t *testing.T, logs *bytes.Buffer) Workspace {
	t.Helper()
	return Workspace{
		Src: t.TempDir(), Out: t.TempDir(), App: policy.App{Kind: policy.KindBinary},
		Policy: policy.Policy{Limits: policy.Limits{Check: time.Minute, FindingsListed: 2}},
		Exec:   &procexectest.Fake{}, Log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func check(name string, kind Kind, need Need, out Outcome) Check {
	return Check{Name: name, Kind: kind, Needs: need, Run: func(context.Context, Workspace) Outcome { return out }}
}

func TestRunnerCollectsResults(t *testing.T) {
	var logs bytes.Buffer
	ws := workspace(t, &logs)
	r := Runner{Now: clock(), Log: ws.Log, Checks: []Check{
		check("ok", KindQuality, NeedSource, Pass(map[string]float64{"v": 1})),
		check("bad", KindSecurity, NeedSource, Fail("broken", "a", "b", "c")),
		check("img", KindSecurity, NeedImage, Pass(nil)),
		check("web", KindQuality, NeedHTTP, Pass(nil)),
	}}
	v := r.Run(context.Background(), ws, Subject{App: "hub", Repo: "o/hub", Commit: "c", Own: true})
	testutil.True(t, !v.Passed, "a failed check blocks")
	testutil.Equal(t, v.Failed(), []string{"bad"})
	testutil.Equal(t, len(v.Results), 4)
	testutil.Equal(t, v.Results[0].Status, StatusPass)
	testutil.Equal(t, v.Results[1].Findings, []string{"a", "b", "and 1 more"})
	testutil.Equal(t, v.Results[2].Status, StatusNA)
	testutil.Contains(t, v.Results[2].Reason, "ships no container image")
	testutil.Equal(t, v.Results[3].Status, StatusNA)
	testutil.Contains(t, v.Results[3].Reason, "exposes no HTTP surface")
	testutil.Equal(t, v.Results[0].Seconds, float64(1))
	testutil.True(t, v.Seconds > 0, "duration recorded")
	testutil.Contains(t, logs.String(), `"msg":"gate started"`, `"msg":"gate check failed","check":"bad"`, `"findings_total":3`, `"value_v":1`,
		`"msg":"gate check passed","check":"ok"`, `"msg":"gate check not applicable","check":"img"`, `"msg":"gate finished"`, `"passed":false`)
}

func TestForeignCodeSkipsQualityChecks(t *testing.T) {
	var logs bytes.Buffer
	ws := workspace(t, &logs)
	ws.App = policy.App{Kind: policy.KindService}
	r := Runner{Now: clock(), Log: ws.Log, Checks: []Check{
		check("tests", KindQuality, NeedSource, Fail("x")),
		check("sast", KindSecurity, NeedSource, Pass(nil)),
		check("dast", KindSecurity, NeedHTTP, Pass(nil)),
		check("image", KindSecurity, NeedImage, Pass(nil)),
	}}
	v := r.Run(context.Background(), ws, Subject{App: "x", Own: false})
	testutil.True(t, v.Passed, "quality findings do not block foreign code")
	testutil.Equal(t, v.Results[0].Status, StatusNA)
	testutil.Contains(t, v.Results[0].Reason, "not own code")
	for _, r := range v.Results[1:] {
		testutil.Equal(t, r.Status, StatusPass)
	}
}

func TestUnfinishedCheckFails(t *testing.T) {
	var logs bytes.Buffer
	ws := workspace(t, &logs)
	ws.Policy.Limits.Check = time.Millisecond
	slow := Check{Name: "slow", Run: func(ctx context.Context, _ Workspace) Outcome {
		<-ctx.Done()
		return Pass(nil)
	}}
	v := Runner{Now: clock(), Log: ws.Log, Checks: []Check{slow}}.Run(context.Background(), ws, Subject{App: "x", Own: true})
	testutil.True(t, !v.Passed, "timeout blocks")
	testutil.Contains(t, v.Results[0].Reason, "check did not complete")
}

func TestOutcomeHelpers(t *testing.T) {
	testutil.Equal(t, Judge(true, "r", nil, []string{"f"}), Outcome{Status: StatusPass, Findings: []string{"f"}})
	testutil.Equal(t, Judge(false, "r", map[string]float64{"a": 1}, nil), Outcome{Status: StatusFail, Reason: "r", Values: map[string]float64{"a": 1}})
	testutil.Equal(t, Findings("r", nil).Status, StatusPass)
	testutil.Equal(t, Findings("r", nil).Values[ValueFindings], float64(0))
	f := Findings("r", []string{"x", "y"})
	testutil.Equal(t, f.Status, StatusFail)
	testutil.Equal(t, f.Values[ValueFindings], float64(2))
	testutil.Equal(t, capFindings([]string{"a", "b"}, 2), []string{"a", "b"})
	testutil.Equal(t, capFindings([]string{"a", "b", "c", "d"}, 1), []string{"a", "and 3 more"})
}

func TestFromTool(t *testing.T) {
	res := procexec.Result{Stdout: "out", Stderr: "err"}
	testutil.Equal(t, FromTool("r", res, errTool, []string{"f"}, nil).Findings, []string{"f"})
	testutil.Equal(t, FromTool("r", res, errTool, []string{"f"}, nil).Status, StatusFail)
	parse := FromTool("r", res, nil, nil, errTool)
	testutil.Equal(t, parse.Status, StatusFail)
	testutil.Contains(t, parse.Reason, "tool report unreadable: exit status 1")
	crash := FromTool("r", res, errTool, nil, nil)
	testutil.Contains(t, crash.Reason, "tool failed: exit status 1: outerr")
	clean := FromTool("r", res, nil, nil, nil)
	testutil.Equal(t, clean.Status, StatusPass)
}

func TestTailAndRel(t *testing.T) {
	testutil.Equal(t, Tail("short"), "short")
	exact := strings.Repeat("b", outputTail)
	testutil.Equal(t, Tail(exact), exact)
	long := strings.Repeat("a", outputTail) + "end"
	testutil.Equal(t, len(Tail(long)), outputTail)
	testutil.True(t, strings.HasSuffix(Tail(long), "end"), "keeps the end")
	ws := Workspace{Src: "/repo"}
	testutil.Equal(t, ws.Rel("/repo/a/b.go"), "a/b.go")
	testutil.Equal(t, ws.Rel("a/b.go"), "a/b.go")
	testutil.Equal(t, ws.Rel("/repository/x.go"), "/repository/x.go")
	testutil.Equal(t, ws.Report("x.json"), filepath.Join("", "x.json"))
}

func TestWorkspaceRunsToolsInTheCheckout(t *testing.T) {
	var logs bytes.Buffer
	ws := workspace(t, &logs)
	fake := &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stdout: c.Dir + "|" + strings.Join(c.Env, ",")}, nil
	}}
	ws.Exec = fake
	res, err := ws.RunEnv(context.Background(), []string{"A=1"}, "tool", "x")
	testutil.NoErr(t, err)
	testutil.Equal(t, res.Stdout, ws.Src+"|A=1")
	res, err = ws.Run(context.Background(), "tool")
	testutil.NoErr(t, err)
	testutil.Equal(t, res.Stdout, ws.Src+"|")
	testutil.Equal(t, fake.Lines(), []string{"tool x", "tool"})
	testutil.Contains(t, logs.String(), `"msg":"gate tool finished","tool":"tool"`, `"failed":false`)
	fake.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errTool }
	_, err = ws.Run(context.Background(), "broken")
	testutil.Fails(t, err)
	testutil.Contains(t, logs.String(), `"failed":true`)
}

func TestVerdictRoundTrip(t *testing.T) {
	v := Verdict{Subject: Subject{App: "a", Commit: "c"}, Passed: false, Results: []Result{{Check: "x", Status: StatusFail}}}
	path := filepath.Join(t.TempDir(), "verdict.json")
	testutil.NoErr(t, v.Save(path))
	testutil.Perm(t, path, verdictPerm)
	got, err := Decode([]byte(testutil.Read(t, path)))
	testutil.NoErr(t, err)
	testutil.Equal(t, got, v)
	dir := t.TempDir()
	testutil.Mkdir(t, filepath.Join(dir, "verdict.json"))
	testutil.Fails(t, v.Save(filepath.Join(dir, "verdict.json")))
	nan := Verdict{Subject: Subject{App: "a"}, Results: []Result{{Check: "x", Values: map[string]float64{"v": math.NaN()}}}}
	unwritten := filepath.Join(t.TempDir(), "verdict.json")
	testutil.Fails(t, nan.Save(unwritten))
	testutil.Missing(t, unwritten)
}

func TestDecodeRejects(t *testing.T) {
	_, err := Decode([]byte(`{`))
	testutil.Contains(t, err.Error(), "unexpected end of JSON input")
	for _, raw := range []string{
		`{`,
		`{"app":"","results":[{"check":"x","status":"pass"}],"passed":true}`,
		`{"app":"a","results":[],"passed":true}`,
		`{"app":"a","results":[{"check":"x","status":"fail"}],"passed":true}`,
		`{"app":"a","results":[{"check":"x","status":"pass"}],"passed":false}`,
	} {
		_, derr := Decode([]byte(raw))
		testutil.Is(t, derr, ErrVerdict)
	}
	v, err := Decode([]byte(`{"app":"a","results":[{"check":"x","status":"n/a"}],"passed":true}`))
	testutil.NoErr(t, err)
	testutil.True(t, v.Passed, "n/a does not block")
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"app":"a","results":[{"check":"x","status":"pass"}],"passed":true}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Decode(data)
		if err == nil && (v.App == "" || len(v.Results) == 0 || v.Passed != (len(v.Failed()) == 0)) {
			t.Fatalf("inconsistent verdict accepted: %+v", v)
		}
	})
}
