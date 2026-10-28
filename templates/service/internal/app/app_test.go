package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DreamsAreReal/starter/internal/web"
)

const (
	waitStep    = 10 * time.Millisecond
	waitLimit   = 5 * time.Second
	loopback    = "127.0.0.1:0"
	tcp         = "tcp"
	healthcheck = "-healthcheck"
	someDSN     = "dsn"
)

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func portOf(t *testing.T, address string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func listenOn(t *testing.T) net.Listener {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), tcp, loopback)
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func freePort(t *testing.T) string {
	t.Helper()
	listener := listenOn(t)
	defer func() { _ = listener.Close() }()
	return portOf(t, listener.Addr().String())
}

func fetch(t *testing.T, url string) string {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestLoadFallsBackToDefaults(t *testing.T) {
	if got := Load(env(nil)); got != (Config{Port: defaultPort, MetricsPort: defaultMetric}) {
		t.Fatalf("defaults %+v", got)
	}
	set := map[string]string{envPort: "1", envMetrics: "2", envSentryDSN: someDSN}
	if got := Load(env(set)); got != (Config{Port: "1", MetricsPort: "2", SentryDSN: someDSN}) {
		t.Fatalf("set %+v", got)
	}
}

func TestMainServesUntilCancelled(t *testing.T) {
	ports, cancel, done, logs := start(t)
	if body := fetch(t, "http://127.0.0.1:"+ports[envMetrics]+"/metrics"); !strings.Contains(body, `route="GET /healthz",code="200"`) {
		t.Fatalf("metrics %s", body)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d: %s", code, logs)
	}
	if !strings.Contains(logs.String(), `"msg":"started"`) {
		t.Fatalf("logs %s", logs)
	}
}

func TestMainFailsLoudly(t *testing.T) {
	busy := listenOn(t)
	defer func() { _ = busy.Close() }()
	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer unhealthy.Close()
	cases := map[string]struct {
		args []string
		env  map[string]string
	}{
		"busy metrics port": {env: map[string]string{envPort: freePort(t), envMetrics: portOf(t, busy.Addr().String())}},
		"bad dsn":           {env: map[string]string{envSentryDSN: "ftp://key@bugsink/1"}},
		"unknown flag":      {args: []string{"-unknown"}},
		"nothing listens":   {args: []string{healthcheck}, env: map[string]string{envPort: freePort(t)}},
		"bad health url":    {args: []string{healthcheck}, env: map[string]string{envPort: "%zz"}},
		"unhealthy":         {args: []string{healthcheck}, env: map[string]string{envPort: portOf(t, unhealthy.Listener.Addr().String())}},
	}
	logs := &logBuffer{}
	for name, c := range cases {
		if code := Main(t.Context(), c.args, env(c.env), logs); code != exitFailed {
			t.Errorf("%s: exit %d", name, code)
		}
	}
	if out := logs.String(); !strings.Contains(out, `"msg":"stopped"`) || !strings.Contains(out, "flag provided but not defined: -unknown") {
		t.Fatalf("logs %s", out)
	}
}

func start(t *testing.T) (ports map[string]string, cancel context.CancelFunc, done chan int, logs *logBuffer) {
	t.Helper()
	ports = map[string]string{envPort: freePort(t), envMetrics: freePort(t)}
	ctx, cancel := context.WithCancel(t.Context())
	logs = &logBuffer{}
	done = make(chan int, 1)
	go func() { done <- Main(ctx, nil, env(ports), logs) }()
	deadline := time.Now().Add(waitLimit)
	for Main(t.Context(), []string{healthcheck}, env(ports), io.Discard) != exitOK {
		if time.Now().After(deadline) {
			t.Fatalf("never healthy: %s", logs)
		}
		time.Sleep(waitStep)
	}
	return ports, cancel, done, logs
}

func stillOpen(t *testing.T, dialer *net.Dialer, address string) bool {
	t.Helper()
	probe, err := dialer.DialContext(t.Context(), tcp, address)
	if err != nil {
		return false
	}
	_ = probe.Close()
	return true
}

func TestStopWaitsForOpenConnections(t *testing.T) {
	ports, cancel, done, logs := start(t)
	var dialer net.Dialer
	site := "127.0.0.1:" + ports[envPort]
	conn, err := dialer.DialContext(t.Context(), tcp, site)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(waitStep)
	cancel()
	for stillOpen(t, &dialer, site) {
		time.Sleep(waitStep)
	}
	select {
	case code := <-done:
		t.Fatalf("stopped with exit %d while a connection was open: %s", code, logs)
	default:
	}
	_ = conn.Close()
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d: %s", code, logs)
	}
}

func TestServeStopsWhenAListenerBreaks(t *testing.T) {
	site, metrics := listenOn(t), listenOn(t)
	_ = site.Close()
	hub, err := web.NewHub("")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	if err := Serve(t.Context(), log, web.New(log, hub), site, metrics); err == nil {
		t.Fatal("a broken listener did not stop the service")
	}
}
