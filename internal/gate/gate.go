package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusNA   Status = "n/a"

	ValueFindings = "findings"
	ValueCrashed  = "crashed"
	crashedFormat = "check crashed: %v"
	verdictPerm   = 0o600
	moreFormat    = "and %d more"
	outputTail    = 600
)

type Kind int

const (
	KindQuality Kind = iota
	KindSecurity
)

type Need int

const (
	NeedSource Need = iota
	NeedImage
	NeedHTTP
)

var ErrVerdict = errors.New("invalid verdict")

type Outcome struct {
	Status   Status
	Reason   string
	Values   map[string]float64
	Findings []string
}

func Pass(values map[string]float64) Outcome {
	return Outcome{Status: StatusPass, Values: values}
}

func Fail(reason string, findings ...string) Outcome {
	return Outcome{Status: StatusFail, Reason: reason, Findings: findings}
}

func NotApplicable(reason string) func(context.Context, Workspace) Outcome {
	return func(context.Context, Workspace) Outcome {
		return Outcome{Status: StatusNA, Reason: reason}
	}
}

func Judge(ok bool, reason string, values map[string]float64, findings []string) Outcome {
	if ok {
		return Outcome{Status: StatusPass, Values: values, Findings: findings}
	}
	return Outcome{Status: StatusFail, Reason: reason, Values: values, Findings: findings}
}

func Findings(reason string, findings []string) Outcome {
	values := map[string]float64{ValueFindings: float64(len(findings))}
	return Judge(len(findings) == 0, reason, values, findings)
}

type Check struct {
	Name  string
	Kind  Kind
	Needs Need
	Long  bool
	Run   func(ctx context.Context, ws Workspace) Outcome
}

type Workspace struct {
	Src         string
	Out         string
	PolicyDir   string
	Image       string
	AppName     string
	Repo        string
	Module      string
	App         policy.App
	Policy      policy.Policy
	Exec        procexec.Runner
	Log         *slog.Logger
	MemoryLimit int64
	Mutation    MutationPlan
}

type MutationPlan struct {
	Cache       string
	CacheSHA256 string
	Full        bool
	Budget      time.Duration
	Last        *FullRun
}

type FullRun struct {
	Commit string    `json:"commit"`
	Bases  []string  `json:"bases"`
	Passed bool      `json:"passed"`
	Time   time.Time `json:"time"`
}

type Subject struct {
	App    string `json:"app"`
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
	Own    bool   `json:"own"`
}

type Result struct {
	Check    string             `json:"check"`
	Status   Status             `json:"status"`
	Reason   string             `json:"reason,omitempty"`
	Values   map[string]float64 `json:"values,omitempty"`
	Findings []string           `json:"findings,omitempty"`
	Seconds  float64            `json:"seconds"`
}

type Verdict struct {
	Subject
	Passed  bool     `json:"passed"`
	Seconds float64  `json:"seconds"`
	Results []Result `json:"results"`
}

type Runner struct {
	Checks []Check
	Now    func() time.Time
	Log    *slog.Logger
}

func (r Runner) Run(ctx context.Context, ws Workspace, subj Subject) Verdict {
	started := r.Now()
	v := Verdict{Subject: subj, Passed: true}
	r.Log.Info("gate started", "app", subj.App, "repo", subj.Repo, "commit", subj.Commit, "own", subj.Own, "checks", len(r.Checks))
	for _, c := range r.Checks {
		res := r.runOne(ctx, ws, subj, c)
		v.Passed = v.Passed && res.Status != StatusFail
		v.Results = append(v.Results, res)
	}
	v.Seconds = r.Now().Sub(started).Seconds()
	r.Log.Info("gate finished", "app", subj.App, "commit", subj.Commit, "passed", v.Passed, "failed_checks", v.Failed(), "seconds", v.Seconds)
	return v
}

func (r Runner) runOne(ctx context.Context, ws Workspace, subj Subject, c Check) Result {
	if reason := notApplicable(c, ws.App, subj); reason != "" {
		r.Log.Info("gate check not applicable", "check", c.Name, "reason", reason)
		return Result{Check: c.Name, Status: StatusNA, Reason: reason}
	}
	t := r.Now()
	limit := ws.Policy.Limits.Check
	if c.Long {
		limit = ws.Policy.Limits.LongCheck
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out := r.runSafely(ctx, ws, c)
	if err := ctx.Err(); err != nil {
		out = Fail(fmt.Sprintf("check did not complete: %v", err))
	}
	res := Result{Check: c.Name, Status: out.Status, Reason: out.Reason, Values: out.Values, Findings: capFindings(out.Findings, ws.Policy.Limits.FindingsListed), Seconds: r.Now().Sub(t).Seconds()}
	r.logResult(res, len(out.Findings))
	return res
}

func (r Runner) runSafely(ctx context.Context, ws Workspace, c Check) (out Outcome) {
	defer func() {
		if p := recover(); p != nil {
			r.Log.Error("gate check panicked", "check", c.Name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			out = Fail(fmt.Sprintf(crashedFormat, p))
			out.Values = map[string]float64{ValueCrashed: 1}
		}
	}()
	return c.Run(ctx, ws)
}

func (r Runner) logResult(res Result, total int) {
	attrs := []any{"check", res.Check, "status", res.Status, "seconds", res.Seconds, "findings_total", total}
	for k, v := range res.Values {
		attrs = append(attrs, "value_"+k, v)
	}
	if res.Status == StatusFail {
		r.Log.Error("gate check failed", append(attrs, "reason", res.Reason, "findings", res.Findings)...)
		return
	}
	r.Log.Info("gate check passed", attrs...)
}

func notApplicable(c Check, app policy.App, subj Subject) string {
	switch {
	case c.Kind == KindQuality && !subj.Own:
		return "not own code: quality checks apply to repositories of the policy owners"
	case c.Needs == NeedImage && !app.HasImage():
		return "app kind " + app.Kind + " ships no container image"
	case c.Needs == NeedHTTP && !app.HasHTTP():
		return "app kind " + app.Kind + " exposes no HTTP surface"
	default:
		return ""
	}
}

func capFindings(findings []string, limit int) []string {
	if len(findings) <= limit {
		return findings
	}
	out := append([]string(nil), findings[:limit]...)
	return append(out, fmt.Sprintf(moreFormat, len(findings)-limit))
}

func (v Verdict) Failed() []string {
	var out []string
	for _, r := range v.Results {
		if r.Status == StatusFail {
			out = append(out, r.Check)
		}
	}
	return out
}

func (v Verdict) Save(path string) error {
	data, err := json.Marshal(v)
	if err == nil {
		err = os.WriteFile(path, data, verdictPerm)
	}
	return err
}

func Decode(data []byte) (Verdict, error) {
	var v Verdict
	if err := json.Unmarshal(data, &v); err != nil {
		return Verdict{}, fmt.Errorf("%w: %w", ErrVerdict, err)
	}
	if v.App == "" || len(v.Results) == 0 {
		return Verdict{}, fmt.Errorf("%w: app and results are required", ErrVerdict)
	}
	if v.Passed != (len(v.Failed()) == 0) {
		return Verdict{}, fmt.Errorf("%w: passed flag contradicts the results", ErrVerdict)
	}
	return v, nil
}

func (ws Workspace) Run(ctx context.Context, name string, args ...string) (procexec.Result, error) {
	return ws.RunEnv(ctx, nil, name, args...)
}

func (ws Workspace) RunEnv(ctx context.Context, env []string, name string, args ...string) (procexec.Result, error) {
	started := time.Now()
	res, err := ws.Exec.Run(ctx, procexec.Command{Name: name, Args: args, Dir: ws.Src, Env: env, MemoryLimit: ws.MemoryLimit})
	ws.Log.Debug("gate tool finished", "tool", name, "args", args, "seconds", time.Since(started).Seconds(), "failed", err != nil)
	return res, err
}

func (ws Workspace) Report(name string) string {
	return filepath.Join(ws.Out, name)
}

func FromTool(reason string, res procexec.Result, runErr error, findings []string, parseErr error) Outcome {
	switch {
	case len(findings) > 0:
		return Findings(reason, findings)
	case parseErr != nil:
		return Fail("tool report unreadable: " + parseErr.Error())
	case runErr != nil:
		return Fail("tool failed: " + runErr.Error() + ": " + Tail(res.Combined()))
	default:
		return Findings(reason, nil)
	}
}

func Tail(s string) string {
	return s[max(0, len(s)-outputTail):]
}

func (ws Workspace) Rel(path string) string {
	return RelTo(ws.Src, path)
}

func RelTo(root, path string) string {
	roots := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		roots = append(roots, resolved)
	}
	for _, r := range roots {
		if rel, ok := strings.CutPrefix(path, r+string(filepath.Separator)); ok {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}
