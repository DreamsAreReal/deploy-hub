package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

type metricsSink struct {
	mu   sync.Mutex
	body strings.Builder
}

func (m *metricsSink) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.body.Write(b)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (m *metricsSink) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.body.String()
}

func TestServerCommandPushesItsRun(t *testing.T) {
	h := newHarness(t)
	sink := &metricsSink{}
	h.env[envMetricsURL] = sink.start(t)
	h.want(ExitUsage, CmdFacts)
	got := sink.String()
	for _, line := range []string{
		`deploy_hub_command_exit_code{command="facts"} 2 `,
		`deploy_hub_command_duration_seconds{command="facts",result="usage"} `,
		`deploy_hub_command_panicked{command="facts"} 0 `,
		`deploy_hub_command_log_records{command="facts",level="warn"} 0 `,
		`deploy_hub_command_log_records{command="facts",level="error"} 0 `,
	} {
		testutil.Contains(t, got, line)
	}
}

func TestLocalCommandPushesNothing(t *testing.T) {
	h := newHarness(t)
	sink := &metricsSink{}
	h.env[envMetricsURL] = sink.start(t)
	h.want(ExitUsage, CmdRender)
	testutil.Equal(t, sink.String(), "")
}

func TestPanicIsRecoveredLoggedAndCounted(t *testing.T) {
	h := newHarness(t)
	sink := &metricsSink{}
	h.env[envMetricsURL] = sink.start(t)
	logFile := filepath.Join(h.hubDir, "facts.log")
	h.env[envLogFile] = logFile
	deps := OSDeps()
	deps.Exec = func(...string) procexec.Runner { panic("boom") }
	code := Run(t.Context(), []string{CmdFacts, appWeb}, h.lookup, IO{Stdin: strings.NewReader(""), Stdout: h.out, Stderr: h.err}, deps)
	testutil.Equal(t, code, ExitFail)
	got := sink.String()
	testutil.Contains(t, got, `deploy_hub_command_panicked{command="facts"} 1 `)
	testutil.Contains(t, got, `deploy_hub_command_duration_seconds{command="facts",result="panic"} `)
	testutil.Contains(t, got, `deploy_hub_command_log_records{command="facts",level="error"} 1 `)
	logs := testutil.Read(t, logFile)
	testutil.Contains(t, logs, `"msg":"command panicked"`, `"panic":"boom"`, `"stack":"goroutine`)
	testutil.Contains(t, logs, `"result":"panic"`)
}
