package e2e

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const jobAuth = "e2e-ephemeral-job-auth-4f2a9c"

var (
	goodA = tag("aaaa1")
	goodB = tag("bbbb2")
	dead  = tag("dead3")
	bad   = tag("bad04")
)

func TestForcedCommandRefusesEverythingElse(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	requests := []string{
		"", "bash", "sh -c id", "deploy web " + goodA + "; rm -rf /", "deploy web $(reboot)",
		"deploy ../../etc " + goodA, "deploy ghost " + goodA, "rollback web sha-xyz", "history web|cat",
		"status && id", "deploy web latest", "DEPLOY web " + goodA,
	}
	for _, r := range requests {
		res := h.ssh(r, "")
		testutil.True(t, res.code == 1, "request must be refused: "+r)
	}
	testutil.Equal(t, h.file("fake/calls.log"), "")
	journal := h.file("deploy-hub/deploys.log")
	testutil.True(t, strings.Count(journal, " refuse denied(") == len(requests), journal)
	testutil.Contains(t, h.metrics.all(), `deploy_hub_operations_total{action="refuse",app="-",result="denied"}`)
}

func TestDeployLifecycle(t *testing.T) {
	t.Parallel()
	h := newHub(t)

	res := h.ssh("deploy web "+goodA, h.deployStdin(goodA))
	testutil.Equal(t, res.code, 0)
	testutil.Contains(t, res.stdout, "health: ok — web@aaaa100 live")
	testutil.Equal(t, h.served(), imageName+":"+goodA)

	res = h.ssh("status", "")
	testutil.Equal(t, res.code, 0)
	testutil.Contains(t, res.stdout, "web | aaaa100 | running/ok | ")

	res = h.ssh("deploy web "+goodB, h.deployStdin(goodB))
	testutil.Equal(t, res.code, 0)
	testutil.Contains(t, res.stdout, "blue-green: starting standby web-bg", "cutting over")
	testutil.Equal(t, h.served(), imageName+":"+goodB)
	testutil.Contains(t, h.file("Caddyfile"), "web."+hostSlug+".sslip.io {\n\tlog\n\treverse_proxy 127.0.0.1:"+h.port+" {\n\t\tlb_try_duration 30s\n\t\tlb_try_interval 250ms\n\t}\n}")
	testutil.Missing(t, filepath.Join(h.appDir, ".caddy-port"))
	_, standbyLeft := h.fake.load("web-bg")
	testutil.True(t, !standbyLeft, "standby must be removed")
	calls := h.file("fake/calls.log")
	testutil.Contains(t, calls, "run -d --name web-bg --restart no", "rm -f web-bg", "logout ghcr.io")

	res = h.ssh("rollback web", "")
	testutil.Equal(t, res.code, 0)
	testutil.Contains(t, res.stdout, "rolled back to previous sha (aaaa100)")
	testutil.Equal(t, h.served(), imageName+":"+goodA)
	testutil.Equal(t, h.file("web/.deploy-state"), "current="+goodA+"\nprevious="+goodB+"\n")

	res = h.ssh("history web", "")
	testutil.Contains(t, res.stdout, "web@aaaa100 deploy ok", "web@bbbb200 deploy ok", "web@aaaa100 rollback ok")
	testutil.Contains(t, h.metrics.all(),
		`deploy_hub_operations_total{action="deploy",app="web",result="ok"} 2`,
		`deploy_hub_lead_time_seconds{app="web"}`,
		`deploy_hub_health_gate_duration_seconds{app="web",result="ok",target="standby"}`)
	assertNoLeak(t, h, jobAuth)
}

func TestBrokenReleaseKeepsServing(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	testutil.Equal(t, h.ssh("deploy web "+goodA, h.deployStdin(goodA)).code, 0)
	res := h.ssh("deploy web "+dead, h.deployStdin(dead))
	testutil.Equal(t, res.code, 1)
	testutil.Contains(t, res.stderr, "standby unhealthy", "zero-downtime: prod never changed")
	testutil.Equal(t, h.served(), imageName+":"+goodA)
	testutil.Contains(t, h.file("web/.deploy-state"), "current="+goodA)
	testutil.Contains(t, h.file("deploy-hub/deploys.log"), "web@dead300 deploy fail")
}

func TestPullFailureIsRecorded(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	res := h.ssh("deploy web "+bad, h.deployStdin(bad))
	testutil.Equal(t, res.code, 1)
	testutil.Contains(t, res.stderr, "error: image pull failed: manifest unknown")
	testutil.Contains(t, h.file("deploy-hub/last-error.log"), "web pull error: manifest unknown")
	testutil.Equal(t, h.served(), "")
	assertNoLeak(t, h, jobAuth)
}

func TestProvisionFactsRender(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	res := h.run("", nil, "provision", "--app", "api", "--profile", "service", "--port", "9555", "--cport", "80", "--mem", "64m", "--image", "ghcr.io/owner/api", "--repo", "owner/api")
	testutil.Equal(t, res.code, 0)
	testutil.Contains(t, res.stdout, "apps.list: added 'api "+filepath.Join(h.root, "api")+"'", "provision: ok")
	testutil.Contains(t, h.file("api/docker-compose.yml"), `"127.0.0.1:9555:80"`)
	testutil.Contains(t, h.file("Caddyfile"), "api."+hostSlug+".sslip.io")
	res = h.run("", nil, "facts", "api")
	testutil.Contains(t, res.stdout, "in_list=1\ndir_exists=1\nconf_exists=1\ncompose_exists=1\nenv_exists=1\nport=9555\n")
	res = h.run("", nil, "provision", "--app", "x;id", "--profile", "service")
	testutil.Equal(t, res.code, 1)

	in := filepath.Join(h.root, "compose.yml")
	testutil.Write(t, in, "services:\n  api:\n    build: ./api\n    ports: ['127.0.0.1:9100:8000']\n  db:\n    image: postgres:17\n")
	res = h.run("", nil, "render", "api", in, filepath.Join(h.root, "out.yml"))
	testutil.Equal(t, res.code, 0)
	testutil.Equal(t, res.stdout, "main=api\nport=9100\npublic=true\nhealth=http\ncontext=./api\ndockerfile=api/Dockerfile\n")
}

func assertNoLeak(t *testing.T, h *hub, secret string) {
	t.Helper()
	var all strings.Builder
	_ = filepath.Walk(h.dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(path) == ".log" {
			all.WriteString(h.file(strings.TrimPrefix(path, h.root+string(os.PathSeparator))))
		}
		return nil
	})
	all.WriteString(h.file("fake/calls.log"))
	all.WriteString(h.metrics.all())
	testutil.Excludes(t, all.String(), secret)
}

func TestOnlyTheHubWorkflowCanDeploy(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	other := newIssuer(t)
	attempts := map[string]string{
		"no token":         "digest=" + digestFor(goodA) + "\n",
		"foreign key":      "oidc=" + other.token(t, appRepo, goodA) + "\ndigest=" + digestFor(goodA) + "\n",
		"other repository": "oidc=" + h.issuer.token(t, "owner/api", goodA) + "\ndigest=" + digestFor(goodA) + "\n",
		"other commit":     "oidc=" + h.issuer.token(t, appRepo, goodB) + "\ndigest=" + digestFor(goodA) + "\n",
		"no digest":        "oidc=" + h.issuer.token(t, appRepo, goodA) + "\n",
	}
	for name, stdin := range attempts {
		res := h.ssh("deploy web "+goodA, stdin)
		testutil.True(t, res.code == 1, name+" must be refused")
	}
	testutil.Excludes(t, h.file("fake/calls.log"), "pull")
	testutil.Equal(t, h.served(), "")
	testutil.Equal(t, h.file("web/.deploy-state"), "")
	testutil.True(t, strings.Count(h.file("deploy-hub/deploys.log"), " refuse denied(") == 3, h.file("deploy-hub/deploys.log"))
	testutil.Contains(t, h.metrics.all(), `deploy_hub_identity_verified{app="web",verb="deploy"} 0`)
}

func TestGateVerdictIsRecorded(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	verdict := `{"app":"web","repo":"owner/web","commit":"` + strings.TrimPrefix(goodA, "sha-") + `","own":true,"passed":false,"seconds":42,` +
		`"results":[{"check":"tests","status":"pass","values":{"tests":3},"seconds":1},{"check":"sast","status":"fail","reason":"found","findings":["x.go:1 rule"],"seconds":2}]}`
	stdin := "oidc=" + h.issuer.token(t, appRepo, goodA) + "\nverdict=" + base64.StdEncoding.EncodeToString([]byte(verdict)) + "\n"
	res := h.ssh("report web "+goodA, stdin)
	testutil.Equal(t, res.code, 0)
	testutil.Contains(t, res.stdout, "gate web@aaaa100: blocked sast")
	testutil.Contains(t, h.file("deploy-hub/deploys.log"), "web@aaaa100 gate fail 42s")
	testutil.Contains(t, h.file("deploy-hub/logs/runner.log"), `"msg":"gate check failed"`, "x.go:1 rule")
	testutil.Contains(t, h.metrics.all(),
		`deploy_hub_gate_passed{app="web",own="true"} 0`,
		`deploy_hub_gate_check_passed{app="web",check="sast"} 0`,
		`deploy_hub_gate_check_value{app="web",check="tests",name="tests"} 3`,
		`deploy_hub_operations_total{action="gate",app="web",result="fail"} 1`)
	testutil.Equal(t, h.file("fake/calls.log"), "")
}

func TestRollbackCannotReachUngatedImages(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	testutil.Equal(t, h.ssh("deploy web "+goodA, h.deployStdin(goodA)).code, 0)
	res := h.ssh("rollback web "+goodB, "")
	testutil.Equal(t, res.code, 1)
	testutil.Contains(t, res.stderr, "never deployed successfully")
	testutil.Equal(t, h.served(), imageName+":"+goodA)
}
