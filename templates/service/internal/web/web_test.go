package web

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	missingPath  = "/missing"
	metricsPath  = "/metrics"
	healthCount  = `http_requests_total{route="GET /healthz",code="200"} 2`
	missingCount = `http_requests_total{route="unmatched",code="404"} 1`
)

func newService(t testing.TB, dsn string) (*Service, *bytes.Buffer) {
	t.Helper()
	hub, err := NewHub(dsn)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	return New(slog.New(slog.NewJSONHandler(logs, nil)), hub), logs
}

func serve(t testing.TB, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody))
	return response
}

func checkHeaders(t testing.TB, response *httptest.ResponseRecorder) {
	t.Helper()
	for name, value := range securityHeaders {
		if got := response.Header().Get(name); got != value {
			t.Fatalf("%s = %q, want %q", name, got, value)
		}
	}
}

func TestPagesAnswerJSONWithSecurityHeaders(t *testing.T) {
	service, _ := newService(t, "")
	handler := service.Handler()
	for path, body := range map[string]string{HealthPath: `{"status":"ok"}` + "\n", "/": `{"service":"` + Name + `"}` + "\n"} {
		response := serve(t, handler, http.MethodGet, path)
		if response.Code != http.StatusOK || response.Body.String() != body || response.Header().Get(contentType) != jsonType {
			t.Fatalf("%s: %d %q %v", path, response.Code, response.Body, response.Header())
		}
		checkHeaders(t, response)
	}
	for method, path := range map[string]string{http.MethodGet: missingPath, http.MethodPost: HealthPath} {
		if response := serve(t, handler, method, path); response.Code < http.StatusBadRequest {
			t.Fatalf("%s %s answered %d", method, path, response.Code)
		}
	}
}

func TestRequestsAreCountedTimedAndLogged(t *testing.T) {
	service, logs := newService(t, "")
	handler := service.Handler()
	serve(t, handler, http.MethodGet, HealthPath)
	serve(t, handler, http.MethodGet, HealthPath)
	serve(t, handler, http.MethodGet, missingPath)
	exposed := serve(t, service.Metrics(), http.MethodGet, metricsPath).Body.String()
	for _, want := range []string{healthCount, missingCount, `http_request_duration_seconds_count{route="GET /healthz"} 2`, "process_resident_memory_bytes"} {
		if !strings.Contains(exposed, want) {
			t.Fatalf("no %q in\n%s", want, exposed)
		}
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"route":"GET /healthz","status":200`) || !strings.Contains(lines[2], `"route":"unmatched","status":404`) {
		t.Fatalf("logs %q", lines)
	}
}

func TestPanicIsSentToBugsinkBeforeTheAnswer(t *testing.T) {
	envelopes := make(chan string, 1)
	bugsink := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		envelopes <- string(body)
	}))
	defer bugsink.Close()
	service, logs := newService(t, strings.Replace(bugsink.URL, "://", "://key@", 1)+"/1")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("broken handler") })
	if response := serve(t, service.observe(mux), http.MethodGet, "/boom"); response.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", response.Code)
	}
	select {
	case envelope := <-envelopes:
		if !strings.Contains(envelope, "broken handler") {
			t.Fatalf("envelope %s", envelope)
		}
	default:
		t.Fatal("the panic was not flushed to Bugsink before the answer")
	}
	if !strings.Contains(logs.String(), `"route":"GET /boom","status":500`) {
		t.Fatalf("logs %s", logs)
	}
}

func TestBadDSNIsRefused(t *testing.T) {
	if _, err := NewHub("ftp://key@bugsink/1"); err == nil {
		t.Fatal("a DSN with an unknown scheme was accepted")
	}
}

func FuzzEveryAnswerKeepsTheSecurityHeaders(f *testing.F) {
	service, _ := newService(f, "")
	handler := service.Handler()
	for _, seed := range []string{HealthPath, "/", missingPath, "/a/../healthz", "//x"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		request.URL.Path = path
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code >= http.StatusInternalServerError {
			t.Fatalf("%q answered %d", path, response.Code)
		}
		checkHeaders(t, response)
	})
}
