package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	webBoot    = 5 * time.Second
	jobTimeout = 90 * time.Second
	webPoll    = 100 * time.Millisecond
	webTimeout = 15 * time.Second
	headerType = "Content-Type"
	typeForm   = "application/x-www-form-urlencoded"
	jobDone    = "Done!"
	jobFailed  = "That did not work"
)

type webApp struct {
	Name     string `json:"name"`
	Sha7     string `json:"sha7"`
	Previous string `json:"previous"`
	URL      string `json:"url"`
	Healthy  bool   `json:"healthy"`
}

type webOverview struct {
	Apps []webApp `json:"apps"`
}

func (h *hub) startWeb() string {
	h.t.Helper()
	port := freePort(h.t)
	cmd := exec.CommandContext(context.Background(), "deploy-hub", "web", "--port", port)
	cmd.Env = h.env
	testutil.NoErr(h.t, cmd.Start())
	h.t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})
	base := "http://127.0.0.1:" + port
	deadline := time.Now().Add(webBoot)
	for time.Now().Before(deadline) {
		if code, _ := h.request(http.MethodGet, base+"/", "", nil); code == http.StatusOK {
			return base
		}
		time.Sleep(webPoll)
	}
	h.t.Fatal("dashboard did not start")
	return ""
}

func (h *hub) request(method, url, body string, headers map[string]string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), webTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	testutil.NoErr(h.t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func (h *hub) act(base, form string) string {
	h.t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), webTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/actions", strings.NewReader(form))
	testutil.NoErr(h.t, err)
	req.Header.Set(headerType, typeForm)
	resp, err := client.Do(req)
	testutil.NoErr(h.t, err)
	_ = resp.Body.Close()
	testutil.Equal(h.t, resp.StatusCode, http.StatusSeeOther)
	page := ""
	deadline := time.Now().Add(jobTimeout)
	for time.Now().Before(deadline) {
		_, page = h.request(http.MethodGet, base+resp.Header.Get("Location"), "", nil)
		if !strings.Contains(page, "http-equiv") {
			return page
		}
		time.Sleep(webPoll)
	}
	h.t.Fatal("action did not finish")
	return page
}

func TestDashboardRollsBackAndRedeploys(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	testutil.Equal(t, h.ssh("deploy web "+goodA, h.deployStdin(goodA)).code, 0)
	testutil.Equal(t, h.ssh("deploy web "+goodB, h.deployStdin(goodB)).code, 0)
	base := h.startWeb()

	code, page := h.request(http.MethodGet, base+"/", "", nil)
	testutil.Equal(t, code, http.StatusOK)
	testutil.Contains(t, page, "<title>Apps · deploy-hub</title>", "😊", `<span class="chip">bbbb200</span>`, `href="/confirm/rollback/web"`)

	_, body := h.request(http.MethodGet, base+"/api/overview", "", nil)
	var ov webOverview
	testutil.NoErr(t, json.Unmarshal([]byte(body), &ov))
	testutil.Equal(t, len(ov.Apps), 1)
	testutil.Equal(t, ov.Apps[0], webApp{Name: appName, Sha7: "bbbb200", Previous: goodA, URL: "https://web." + hostSlug + ".sslip.io", Healthy: true})

	_, page = h.request(http.MethodGet, base+"/confirm/rollback/web", "", nil)
	testutil.Contains(t, page, "Roll back web?", "aaaa100")

	code, _ = h.request(http.MethodPost, base+"/actions", "app=web&action=rollback",
		map[string]string{headerType: typeForm, "Origin": "https://evil.example"})
	testutil.Equal(t, code, http.StatusForbidden)
	testutil.Equal(t, h.served(), imageName+":"+goodB)

	page = h.act(base, "app=web&action=rollback")
	testutil.Contains(t, page, jobDone, "rolled back to previous sha (aaaa100)")
	testutil.Equal(t, h.served(), imageName+":"+goodA)

	page = h.act(base, "app=web&action=redeploy&tag="+goodA)
	testutil.Contains(t, page, jobDone, "redeployed to requested sha (aaaa100)")
	testutil.Equal(t, h.served(), imageName+":"+goodA)
	testutil.Contains(t, h.file("deploy-hub/deploys.log"), "web@aaaa100 rollback ok", "web@aaaa100 redeploy ok")

	page = h.act(base, "app=web&action=redeploy&tag=sha-ffff0000000")
	testutil.Contains(t, page, jobFailed, "refused: tag was never deployed successfully")
	testutil.Equal(t, h.served(), imageName+":"+goodA)
}
