package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	hostSlug    = "203-0-113-7"
	appRepo     = "owner/web"
	appName     = "web"
	imageName   = "ghcr.io/owner/web"
	gateTimeout = "60s"
	gateTick    = "1s"
	httpTimeout = 2 * time.Second

	appPortFloor  = 10000
	appPortSpan   = 12000
	standbyOffset = 10000
)

type metricsSink struct {
	mu     sync.Mutex
	bodies []string
}

func (m *metricsSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.bodies = append(m.bodies, string(data))
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (m *metricsSink) all() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.bodies, "")
}

type hub struct {
	t       *testing.T
	issuer  *issuer
	root    string
	dir     string
	appDir  string
	port    string
	fake    fake
	env     []string
	metrics *metricsSink
}

type result struct {
	code   int
	stdout string
	stderr string
}

var (
	portMu   sync.Mutex
	nextPort = appPortFloor + os.Getpid()%appPortSpan
)

func freePort(t *testing.T) string {
	t.Helper()
	portMu.Lock()
	defer portMu.Unlock()
	for range appPortSpan {
		p := nextPort
		nextPort = appPortFloor + (p-appPortFloor+1)%appPortSpan
		if bindable(p) && bindable(p+standbyOffset) {
			return strconv.Itoa(p)
		}
	}
	t.Fatal("no free app and standby port pair")
	return ""
}

func bindable(port int) bool {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	return l.Close() == nil
}

func newHub(t *testing.T) *hub {
	t.Helper()
	root := t.TempDir()
	h := &hub{t: t, root: root, dir: filepath.Join(root, "deploy-hub"), appDir: filepath.Join(root, appName), port: freePort(t),
		fake: fake{state: filepath.Join(root, "fake")}, metrics: &metricsSink{}}
	testutil.Mkdir(t, filepath.Join(h.dir, "bin"))
	testutil.Mkdir(t, h.fake.state)
	testutil.NoErr(t, os.Link(hubBinary, filepath.Join(h.dir, "bin", "deploy-hub")))
	fakeBin := filepath.Join(root, "fakebin")
	testutil.Mkdir(t, fakeBin)
	self, err := os.Executable()
	testutil.NoErr(t, err)
	for _, name := range []string{binDocker, binSudo, binCaddy, binSysctl} {
		testutil.NoErr(t, os.Symlink(self, filepath.Join(fakeBin, name)))
	}
	testutil.Write(t, filepath.Join(h.dir, "apps.list"), appName+"\n")
	testutil.Write(t, filepath.Join(h.appDir, "app.conf"), "profile=service\nport="+h.port+"\nhealth_path=/\nimage="+imageName+"\nrepo="+appRepo+"\n")
	h.issuer = newIssuer(t)
	testutil.Write(t, filepath.Join(h.appDir, "docker-compose.yml"), "services: {}\n")
	sink := httptest.NewServer(h.metrics)
	t.Cleanup(sink.Close)
	t.Cleanup(h.fake.killAll)
	me, err := user.Current()
	testutil.NoErr(t, err)
	h.env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HUB_DIR="+h.dir,
		"CADDYFILE="+filepath.Join(root, "Caddyfile"),
		"CADDY_USER="+me.Username,
		"DEPLOY_USER="+me.Username,
		"DEPLOY_HUB_METRICS_URL="+sink.URL,
		"DEPLOY_HUB_HEALTH_TIMEOUT="+gateTimeout,
		"DEPLOY_HUB_HEALTH_INTERVAL="+gateTick,
		"HOST_SLUG="+hostSlug,
		"DEPLOY_HUB_JWKS_URL="+h.issuer.url,
		"DEPLOY_HUB_JWKS_CACHE="+filepath.Join(root, "jwks.json"),
		envState+"="+h.fake.state,
	)
	return h
}

func (h *hub) run(stdin string, env []string, args ...string) result {
	h.t.Helper()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(context.Background(), "deploy-hub", args...)
	cmd.Env = append(append([]string(nil), h.env...), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errb
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else {
		testutil.NoErr(h.t, err)
	}
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func (h *hub) ssh(request, stdin string) result {
	h.t.Helper()
	return h.run(stdin, []string{"SSH_ORIGINAL_COMMAND=" + request}, "runner")
}

func (h *hub) served() string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+h.port+"/", nil)
	testutil.NoErr(h.t, err)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func (h *hub) file(rel string) string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Clean(filepath.Join(h.root, rel)))
	if err != nil {
		return ""
	}
	return string(data)
}

func tag(prefix string) string {
	return "sha-" + prefix + strings.Repeat("0", shaHexLen-len(prefix))
}

func (h *hub) deployStdin(tagName string) string {
	return "token=" + jobAuth + "\nactor=ci\nsubject=feat: e2e\nstart=" + strconv.FormatInt(time.Now().Unix(), 10) + "\n" +
		"oidc=" + h.issuer.token(h.t, appRepo, tagName) + "\ndigest=" + digestFor(tagName) + "\n"
}

func digestFor(tagName string) string {
	sum := sha256.Sum256([]byte(tagName))
	if strings.Contains(tagName, badPull) {
		return badDigest
	}
	return "sha256:" + hex.EncodeToString(sum[:])
}
