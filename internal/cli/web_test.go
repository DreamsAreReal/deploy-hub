package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	webWait = 5 * time.Second
	webTick = 20 * time.Millisecond
)

func loopbackPort(t *testing.T) (string, net.Listener) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	testutil.NoErr(t, err)
	addr, ok := ln.Addr().(*net.TCPAddr)
	testutil.True(t, ok, "tcp listener")
	return strconv.Itoa(addr.Port), ln
}

func call(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	testutil.NoErr(t, err)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(webWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(webTick)
	}
}

func TestWebServesDashboardAndActionsUntilStopped(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join(h.hubDir, "apps.list"), appWeb+"\n")
	port, ln := loopbackPort(t)
	testutil.NoErr(t, ln.Close())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	deps := Deps{Exec: func(...string) procexec.Runner { return h.exec }, Now: time.Now, Checks: h.checks}
	go func() {
		done <- Run(ctx, []string{CmdWeb, "--port", port}, h.lookup, IO{Stdin: strings.NewReader(""), Stdout: h.out, Stderr: h.err}, deps)
	}()
	base := "http://127.0.0.1:" + port
	var overview string
	eventually(t, "dashboard did not answer", func() bool {
		code, body := call(t, http.MethodGet, base+"/api/overview", "")
		overview = body
		return code == http.StatusOK
	})
	testutil.Contains(t, overview, `"name":"web"`, `"url":"https://web.`+testSlug+`.sslip.io"`)

	job := submit(t, base+"/actions", "app=web&action=rollback")
	var page string
	eventually(t, "action did not finish", func() bool {
		_, page = call(t, http.MethodGet, base+job, "")
		return !strings.Contains(page, "http-equiv")
	})
	testutil.Contains(t, page, "That did not work", "error: app.conf not found for app (web)")

	cancel()
	testutil.Equal(t, <-done, ExitOK)
	testutil.Contains(t, h.out.String(), "web: dashboard on http://127.0.0.1:"+port)
}

func TestWebRefusesBadPorts(t *testing.T) {
	h := newHarness(t)
	h.want(ExitUsage, CmdWeb, "--bogus")
	h.want(ExitFail, CmdWeb, "--port", "0")
	h.want(ExitFail, CmdWeb, "--port", "8970; id")
	port, ln := loopbackPort(t)
	defer func() { _ = ln.Close() }()
	h.want(ExitFail, CmdWeb, "--port", port)
	testutil.Contains(t, h.err.String(), "web: ", "address already in use")
}

func submit(t *testing.T, url, form string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form))
	testutil.NoErr(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	testutil.NoErr(t, err)
	_ = resp.Body.Close()
	testutil.Equal(t, resp.StatusCode, http.StatusSeeOther)
	return resp.Header.Get("Location")
}
