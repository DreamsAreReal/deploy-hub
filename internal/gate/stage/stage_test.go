package stage

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var errExit = errors.New("exit status 1")

const (
	argCreate    = "create"
	argPort      = "port"
	pathHealth   = "/health"
	pathAPI      = "/api"
	badState     = "true false x 0"
	stoppedState = "false " + "false 0 0"
	exitedState  = "false " + "false 0 1"
	unpublished  = "garbage"
)

type dockerSim struct {
	addr     string
	state    string
	runErr   error
	portOut  string
	zapOut   string
	catErr   error
	logsOut  string
	logsErr  error
	badImage string
}

func (d *dockerSim) handle(c procexec.Command) (procexec.Result, error) {
	switch c.Args[0] {
	case "run":
		return d.run(c.Args)
	case argPort:
		if d.portOut != "" {
			return procexec.Result{Stdout: d.portOut}, nil
		}
		return procexec.Result{Stdout: "0.0.0.0:1\n[::]:1\n" + d.addr + "\n"}, nil
	case "inspect":
		return procexec.Result{Stdout: d.state}, nil
	case "logs":
		return procexec.Result{Stderr: d.logsOut}, d.logsErr
	}
	return procexec.Result{}, nil
}

func (d *dockerSim) run(args []string) (procexec.Result, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "--entrypoint cat"):
		return procexec.Result{Stdout: d.zapOut}, d.catErr
	case strings.Contains(joined, zapFullScan) || strings.Contains(joined, zapAPIScan):
		return procexec.Result{Stdout: "WARN-NEW: 1"}, errExit
	case d.badImage != "" && strings.HasSuffix(joined, " "+d.badImage):
		return procexec.Result{}, errExit
	default:
		return procexec.Result{}, d.runErr
	}
}

func app(t *testing.T, handler http.Handler) (gate.Workspace, *dockerSim) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	sim := &dockerSim{addr: strings.TrimPrefix(srv.URL, "http://"), state: "true false 0 0\n"}
	ws := gate.Workspace{
		AppName: "web", Image: "web:gate", Exec: &procexectest.Fake{Handler: sim.handle}, Log: slog.New(slog.DiscardHandler),
		App: policy.App{
			Kind: policy.KindService, Port: 8080, Health: pathHealth, CPUs: "0.5", Memory: "256m", Env: []string{"MODE=test"},
			DAST: policy.DAST{Spider: 90 * time.Second, MaxTime: 5 * time.Minute},
			Load: policy.LoadProfile{Rate: 200, Duration: 300 * time.Millisecond, P95: time.Second, Concurrency: 8, Paths: []string{pathHealth, pathAPI}},
		},
		Policy: policy.Policy{Images: policy.Images{ZAP: "zap@sha256:1"}, Limits: policy.Limits{StageReady: 10 * time.Second}},
	}
	return ws, sim
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func TestStartRunsTheImageWithProductionLimits(t *testing.T) {
	ws, sim := app(t, okHandler())
	var logs bytes.Buffer
	ws.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	st, err := Start(context.Background(), ws, "test")
	testutil.NoErr(t, err)
	testutil.Equal(t, st.HostAddr, sim.addr)
	testutil.Equal(t, st.URL("/x"), "http://"+sim.addr+"/x")
	testutil.Equal(t, st.InternalURL("/x"), "http://deploy-hub-gate-web-test:8080/x")
	lines := procexectest.LinesOf(ws.Exec)
	testutil.Equal(t, lines[0], "docker rm -f deploy-hub-gate-web-test")
	testutil.Equal(t, lines[2], "docker network create deploy-hub-gate-web-test-net")
	testutil.Contains(t, lines[3], "--cpus 0.5 --memory 256m --memory-swap 256m --pids-limit 256", "--security-opt no-new-privileges", "-p 127.0.0.1::8080", "-e MODE=test web:gate")
	st.Stop(context.Background())
	all := strings.Join(procexectest.LinesOf(ws.Exec), "\n")
	testutil.Equal(t, strings.Count(all, "docker rm -f deploy-hub-gate-web-test"), 2)
	testutil.Equal(t, strings.Count(all, "docker network rm deploy-hub-gate-web-test-net"), 2)
	state, err := st.State(context.Background())
	testutil.NoErr(t, err)
	testutil.Equal(t, state, State{Running: true})
	testutil.Contains(t, logs.String(), `"msg":"gate stage ready","stage":"deploy-hub-gate-web-test","addr":"`+sim.addr+`","cpus":"0.5","memory":"256m"`)
}

func TestStartMountsAFreshVolumeOnEveryDataDirectory(t *testing.T) {
	ws, _ := app(t, okHandler())
	ws.App.Data = []string{dataDir, "/cache"}
	st, err := Start(context.Background(), ws, "test")
	testutil.NoErr(t, err)
	st.Stop(context.Background())
	lines := procexectest.LinesOf(ws.Exec)
	testutil.Equal(t, lines[1], "docker volume rm -f deploy-hub-gate-web-test-data-0")
	testutil.Equal(t, lines[2], "docker volume rm -f deploy-hub-gate-web-test-data-1")
	testutil.Contains(t, strings.Join(lines, "\n"),
		"--mount type=volume,src=deploy-hub-gate-web-test-data-0,dst=/data --mount type=volume,src=deploy-hub-gate-web-test-data-1,dst=/cache web:gate")
	testutil.Equal(t, strings.Count(strings.Join(lines, "\n"), "docker volume rm -f deploy-hub-gate-web-test-data-1"), 2)
}

func cleanups(ws gate.Workspace) int {
	return strings.Count(strings.Join(procexectest.LinesOf(ws.Exec), "\n"), "docker rm -f deploy-hub-gate-web-x")
}

func TestStartFailures(t *testing.T) {
	ws, sim := app(t, okHandler())
	sim.runErr = errExit
	_, err := Start(context.Background(), ws, "x")
	testutil.Contains(t, err.Error(), "docker run")
	testutil.Equal(t, cleanups(ws), 2)
	ws2, sim2 := app(t, okHandler())
	sim2.portOut = unpublished
	_, err = Start(context.Background(), ws2, "x")
	testutil.Is(t, err, ErrState)
	testutil.Equal(t, cleanups(ws2), 2)
	ws3, _ := app(t, okHandler())
	ws3.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		if c.Args[0] == "network" && c.Args[1] == argCreate {
			return procexec.Result{Stderr: "exists"}, errExit
		}
		if c.Args[0] == argPort {
			return procexec.Result{}, errExit
		}
		return procexec.Result{}, nil
	}}
	_, err = Start(context.Background(), ws3, "x")
	testutil.Contains(t, err.Error(), "docker network")
	ws4, _ := app(t, okHandler())
	ws4.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		if c.Args[0] == argPort {
			return procexec.Result{}, errExit
		}
		return procexec.Result{}, nil
	}}
	_, err = Start(context.Background(), ws4, "x")
	testutil.Contains(t, err.Error(), "docker port")
}

func TestStartWaitsForHealth(t *testing.T) {
	var calls atomic.Int32
	ws, _ := app(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	_, err := Start(context.Background(), ws, "x")
	testutil.NoErr(t, err)
	testutil.True(t, calls.Load() >= 2, "polled until healthy")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bad, _ := app(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	_, err = Start(ctx, bad, "x")
	testutil.Fails(t, err)
	testutil.Equal(t, cleanups(bad), 2)
	slow, _ := app(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }))
	slow.Policy.Limits.StageReady = 50 * time.Millisecond
	_, err = Start(context.Background(), slow, "x")
	testutil.Is(t, err, ErrNotReady)
	testutil.True(t, !healthy(context.Background(), http.DefaultClient, "http://bad host/"), "bad URL is not healthy")
	testutil.True(t, !healthy(context.Background(), http.DefaultClient, "http://127.0.0.1:1/"), "closed port is not healthy")
}

func TestStartKeepsTheCrashedContainerEvidenceBeforeRemovingIt(t *testing.T) {
	ws, sim := app(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	ws.Policy.Limits.StageReady = 50 * time.Millisecond
	var logs bytes.Buffer
	ws.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	sim.state = "false true 0 137"
	sim.logsOut = "panic: out of memory\n"
	_, err := Start(context.Background(), ws, "x")
	testutil.Is(t, err, ErrNotReady)
	testutil.Contains(t, err.Error(), "container: running=false oom=true restarts=0 exit=137", "logs: panic: out of memory")
	testutil.Contains(t, logs.String(), `"msg":"gate stage failed","stage":"deploy-hub-gate-web-x"`, `"container":"running=false oom=true restarts=0 exit=137"`, `"logs":"panic: out of memory"`)
	lines := strings.Join(procexectest.LinesOf(ws.Exec), "\n")
	testutil.Contains(t, lines, "docker logs --tail 50 deploy-hub-gate-web-x\ndocker rm -f deploy-hub-gate-web-x")

	noPort, usim := app(t, okHandler())
	usim.portOut = unpublished
	usim.state = exitedState
	usim.logsErr = errExit
	_, err = Start(context.Background(), noPort, "x")
	testutil.Is(t, err, ErrState)
	testutil.Contains(t, err.Error(), "container: running=false oom=false restarts=0 exit=1", "logs: docker logs: exit status 1")

	gone, gsim := app(t, okHandler())
	gsim.portOut = unpublished
	gsim.state = ""
	_, err = Start(context.Background(), gone, "x")
	testutil.Contains(t, err.Error(), "container: unexpected container state")
}

func TestState(t *testing.T) {
	ws, sim := app(t, okHandler())
	st := &Stage{ws: ws, Name: "n"}
	sim.state = "false true 3 137"
	s, err := st.State(context.Background())
	testutil.NoErr(t, err)
	testutil.Equal(t, s, State{Running: false, OOM: true, Restarts: 3, ExitCode: 137})
	for _, bad := range []string{"true", badState, "true false 0 x"} {
		sim.state = bad
		_, serr := st.State(context.Background())
		testutil.Is(t, serr, ErrState)
	}
	ws.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errExit }}
	_, err = (&Stage{ws: ws, Name: "n"}).State(context.Background())
	testutil.Contains(t, err.Error(), "docker inspect")
}

const zapJSON = `{"site":[{"alerts":[
{"pluginid":"10021","name":"X-Content-Type-Options Header Missing","riskcode":"1","count":"3","instances":[{"uri":"http://web/"}]},
{"pluginid":"40012","name":"XSS","riskcode":"3","count":"1","instances":[]},
{"pluginid":"10109","name":"Modern Web Application","riskcode":"0","count":"1"}]}]}`

func TestDAST(t *testing.T) {
	ws, sim := app(t, okHandler())
	sim.zapOut = zapJSON
	out := DAST(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Findings, []string{
		"Low X-Content-Type-Options Header Missing (plugin 10021, 3 instances) http://web/",
		"High XSS (plugin 40012, 1 instances) ",
	})
	all := strings.Join(procexectest.LinesOf(ws.Exec), "\n")
	testutil.Contains(t, all, "docker volume create deploy-hub-gate-web-dast-zap",
		"run --rm --network deploy-hub-gate-web-dast-net -v deploy-hub-gate-web-dast-zap:/zap/wrk zap@sha256:1 zap-full-scan.py -t http://deploy-hub-gate-web-dast:8080/health -J zap.json -I -m 2 -T 5 -z -config scanner.maxScanDurationInMins=5 -config scanner.threadPerHost=4",
		"run --rm -u 0 -v deploy-hub-gate-web-dast-zap:/zap/wrk --entrypoint chown zap@sha256:1 zap:zap /zap/wrk",
		"docker volume rm -f deploy-hub-gate-web-dast-zap")
	sim.zapOut = `{"site":[{"alerts":[]}]}`
	ws.App.DAST.Target = "/api"
	testutil.Equal(t, DAST(context.Background(), ws).Status, gate.StatusPass)
	testutil.Contains(t, strings.Join(procexectest.LinesOf(ws.Exec), "\n"), "zap-full-scan.py -t http://deploy-hub-gate-web-dast:8080/api ")
	sim.state = stoppedState
	crashed := DAST(context.Background(), ws)
	testutil.Equal(t, crashed.Findings, []string{reasonCrashed})
}

func TestDASTWithOpenAPIAndFailures(t *testing.T) {
	ws, sim := app(t, okHandler())
	ws.App.DAST.OpenAPI = "/openapi.json"
	sim.zapOut = `{"site":[]}`
	testutil.Equal(t, DAST(context.Background(), ws).Status, gate.StatusPass)
	testutil.Contains(t, strings.Join(procexectest.LinesOf(ws.Exec), "\n"), "zap-api-scan.py -t http://deploy-hub-gate-web-dast:8080/openapi.json", "-f openapi")
	sim.catErr = errExit
	testutil.Contains(t, DAST(context.Background(), ws).Reason, "ZAP produced no report")
	sim.catErr = nil
	sim.zapOut = "{"
	testutil.Contains(t, DAST(context.Background(), ws).Reason, "ZAP produced no report")
	noImage, _ := app(t, okHandler())
	noImage.Image = ""
	testutil.Equal(t, DAST(context.Background(), noImage).Status, gate.StatusFail)
	broken, bsim := app(t, okHandler())
	bsim.runErr = errExit
	testutil.Contains(t, DAST(context.Background(), broken).Reason, "docker run")
	noVolume, vsim := app(t, okHandler())
	noVolume.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		if c.Args[0] == "volume" && c.Args[1] == argCreate {
			return procexec.Result{}, errExit
		}
		return vsim.handle(c)
	}}
	noChown, csim := app(t, okHandler())
	noChown.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		if strings.Contains(strings.Join(c.Args, " "), "--entrypoint chown") {
			return procexec.Result{}, errExit
		}
		return csim.handle(c)
	}}
	testutil.Contains(t, DAST(context.Background(), noChown).Reason, "docker run")
	testutil.Contains(t, DAST(context.Background(), noVolume).Reason, "docker volume")
}

func TestLoad(t *testing.T) {
	var hits atomic.Int32
	ws, sim := app(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == pathAPI {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	out := Load(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Contains(t, strings.Join(out.Findings, ";"), reasonErrors)
	testutil.True(t, out.Values[ValueRequests] > 0 && hits.Load() > 0, "traffic was sent")
	testutil.True(t, out.Values[ValueErrors] > 0, "errors counted")
	ws.App.Load.Paths = []string{pathHealth}
	testutil.Equal(t, Load(context.Background(), ws).Status, gate.StatusPass)
	sim.state = badState
	stateless := Load(context.Background(), ws)
	testutil.Equal(t, stateless.Status, gate.StatusFail)
	testutil.Contains(t, stateless.Reason, "unexpected container state")
	noImage, _ := app(t, okHandler())
	noImage.Image = ""
	testutil.Equal(t, Load(context.Background(), noImage).Status, gate.StatusFail)
	broken, bsim := app(t, okHandler())
	bsim.runErr = errExit
	testutil.Contains(t, Load(context.Background(), broken).Reason, "docker run")
}

func TestSummarizeSortsLatencies(t *testing.T) {
	samples := make([]sample, 0, 20)
	for i := 20; i >= 1; i-- {
		samples = append(samples, sample{latency: time.Duration(i), ok: i%2 == 0})
	}
	res := summarize(samples, 3, time.Second)
	testutil.Equal(t, res.P95, time.Duration(19))
	testutil.Equal(t, res.P99, time.Duration(20))
	testutil.Equal(t, res.Failed, 10)
	testutil.Equal(t, LoadResult{Requests: 99, Failed: 1, Dropped: 1}.ErrorPercent(), float64(2))
	testutil.Equal(t, LoadResult{Requests: 3, Elapsed: 2 * time.Second}.RPS(), 1.5)
}

func TestEvaluate(t *testing.T) {
	prof := policy.LoadProfile{P95: 100 * time.Millisecond, MaxErrors: 1}
	good := LoadResult{Requests: 100, Failed: 1, P95: 100 * time.Millisecond, P99: 200 * time.Millisecond, Elapsed: 10 * time.Second}
	out := Evaluate(good, State{Running: true}, prof)
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Equal(t, out.Values[ValueRPS], float64(10))
	testutil.Equal(t, out.Values[ValueP95], float64(100))
	testutil.Equal(t, out.Values[ValueP99], float64(200))
	testutil.Equal(t, out.Values[ValueErrors], float64(1))
	cases := map[string]struct {
		res   LoadResult
		state State
		want  string
	}{
		"slow":     {LoadResult{Requests: 10, P95: 101 * time.Millisecond}, State{Running: true}, reasonSlow},
		"errors":   {LoadResult{Requests: 99, Failed: 1, Dropped: 1}, State{Running: true}, reasonErrors},
		"crashed":  {good, State{Running: false}, reasonUnstable},
		"oom":      {good, State{Running: true, OOM: true}, reasonUnstable},
		"restarts": {good, State{Running: true, Restarts: 1}, reasonUnstable},
		"silent":   {LoadResult{}, State{Running: true}, reasonNoTraffic},
	}
	for name, c := range cases {
		out := Evaluate(c.res, c.state, prof)
		testutil.Equal(t, out.Status, gate.StatusFail)
		testutil.Contains(t, strings.Join(out.Findings, ";"), c.want)
		testutil.True(t, name != "", name)
	}
	testutil.Equal(t, LoadResult{}.RPS(), float64(0))
	testutil.Equal(t, LoadResult{}.ErrorPercent(), float64(percentScale))
}

func TestGeneratorDropsWhenSaturated(t *testing.T) {
	var hits atomic.Int32
	window := 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			time.Sleep(10 * window)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	g := &Generator{Client: srv.Client(), URLs: []string{srv.URL}, Rate: 500, Duration: window, Concurrency: 1}
	res := g.Run(context.Background())
	testutil.True(t, res.Dropped > 0, "requests beyond concurrency are dropped")
	testutil.Equal(t, res.Requests, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	idle := (&Generator{Client: srv.Client(), URLs: []string{srv.URL}, Rate: 10, Duration: time.Hour, Concurrency: 1}).Run(ctx)
	testutil.Equal(t, idle.Requests, 0)
	bad := (&Generator{Client: srv.Client(), URLs: []string{"http://bad host/"}, Rate: 100, Duration: 50 * time.Millisecond, Concurrency: 4}).Run(context.Background())
	testutil.Equal(t, bad.Failed, bad.Requests)
	closed := (&Generator{Client: srv.Client(), URLs: []string{"http://127.0.0.1:1/"}, Rate: 100, Duration: 50 * time.Millisecond, Concurrency: 4}).Run(context.Background())
	testutil.Equal(t, closed.Failed, closed.Requests)
}

func TestPercentile(t *testing.T) {
	testutil.Equal(t, percentile(nil, p95), time.Duration(0))
	lat := []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	testutil.Equal(t, percentile(lat, p95), time.Duration(19))
	testutil.Equal(t, percentile(lat, p99), time.Duration(20))
	testutil.Equal(t, percentile([]time.Duration{5}, p95), time.Duration(5))
	testutil.Equal(t, minutes(90*time.Second), 2)
	testutil.Equal(t, minutes(time.Minute), 1)
}
