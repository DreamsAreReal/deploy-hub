package runner

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/oidc"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	fullSha       = "aaaaaaa1111" + "0000000000000000000000000000"
	checkTests    = "tests"
	checkMutation = "mutation"
	checkDAST     = "dast"
	valueScore    = "mutation_score"
)

func TestDeployRequiresVerifiedIdentity(t *testing.T) {
	cases := map[string]struct {
		stdin string
		want  string
	}{
		"no token":      {"digest=" + digest + "\n", msgNoIdentity},
		"forged token":  {"oidc=forged\ndigest=" + digest + "\n", msgUnverified},
		"no digest":     {"oidc=" + goodOIDC + "\n", msgBadDigest},
		"short digest":  {"oidc=" + goodOIDC + "\ndigest=sha256:abc\n", msgBadDigest},
		"tag as digest": {"oidc=" + goodOIDC + "\ndigest=" + goodTag + "\n", msgBadDigest},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.bare = true
			testutil.Equal(t, e.dispatch("deploy web "+goodTag, c.stdin), ExitFail)
			e.expectOut(e.err, c.want)
			testutil.Equal(t, e.exec.Count("docker pull"), 0)
			testutil.Equal(t, registry.LoadState(e.appDir), registry.DeployState{})
		})
	}
}

func TestForgedIdentityIsJournaledAsRefusal(t *testing.T) {
	e := newEnv(t)
	e.bare = true
	e.dispatch("deploy web "+goodTag, "oidc=forged\ndigest="+digest+"\n")
	e.expectJournal("refuse denied(deploy_web_" + goodTag + ")")
	testutil.True(t, e.metrics.has(obs.MetricIdentityVerified, obs.Labels{obs.LabelApp: appName, obs.LabelVerb: VerbDeploy}), "identity metric")
	testutil.True(t, sampleValue(e, obs.MetricIdentityVerified) == 0, "rejected identity is 0")
	testutil.Contains(t, e.logs.String(), "deploy identity rejected")
	e.noSecretLeak("forged")
}

func sampleValue(e *env, name string) float64 {
	for _, s := range e.metrics.samples {
		if s.Name == name {
			return s.Value
		}
	}
	return -1
}

const truncatedJSON = "unexpected end of JSON input"

func checkSample(e *env, name, check string) float64 {
	for _, s := range e.metrics.samples {
		if s.Name == name && s.Labels[obs.LabelCheck] == check {
			return s.Value
		}
	}
	return -1
}

func TestIdentityExpectationsAreBoundToAppAndCommit(t *testing.T) {
	e := newEnv(t)
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, ""), ExitOK)
	testutil.Equal(t, len(e.ident.wants), 1)
	want := e.ident.wants[0]
	testutil.Equal(t, want.Repository, appRepo)
	testutil.Equal(t, want.WorkflowRef, hubRef)
	testutil.Equal(t, want.Audience, "deploy-hub")
	testutil.Equal(t, want.Commit, strings.TrimPrefix(goodTag, registry.TagPrefix))
	testutil.True(t, !want.Now.IsZero(), "clock passed")
	testutil.Equal(t, sampleValue(e, obs.MetricIdentityVerified), float64(1))
	testutil.Contains(t, e.logs.String(), "deploy identity verified", `"run_id":"7"`)
	testutil.True(t, e.metrics.has(obs.MetricImagePullDuration, obs.Labels{obs.LabelApp: appName, obs.LabelResult: probeOK}), "pull duration")
}

func TestAppWithoutRepoBindingIsRefused(t *testing.T) {
	e := newEnv(t)
	e.write(filepath.Join(e.appDir, registry.AppConfFile), "profile=service\nport="+appPort+"\nimage="+image+"\n")
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, ""), ExitFail)
	e.expectOut(e.err, msgNoRepo)
	testutil.Equal(t, len(e.ident.wants), 0)
}

func TestKeyOutageRefusesDeploy(t *testing.T) {
	e := newEnv(t)
	e.ident.err = oidc.ErrKeys
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, ""), ExitFail)
	e.expectOut(e.err, msgUnverified)
}

func TestTagFailureAfterPullIsFatal(t *testing.T) {
	e := newEnv(t)
	base := e.exec.Handler
	e.exec.Handler = func(c procexec.Command) (procexec.Result, error) {
		if c.Args[0] == "tag" {
			return procexec.Result{}, errSim
		}
		return base(c)
	}
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, ""), ExitFail)
	e.expectJournal("web@aaaaaaa deploy fail")
	testutil.True(t, e.metrics.has(obs.MetricImagePullDuration, obs.Labels{obs.LabelResult: probeFail}), "failed pull metric")
}

func TestRollbackOnlyToDeployedVersions(t *testing.T) {
	e := newEnv(t)
	e.state(registry.DeployState{Current: goodTag})
	testutil.Equal(t, e.dispatch("rollback web "+good2Tag, ""), ExitFail)
	e.expectOut(e.err, msgNotGated)
	e.expectJournal("refuse denied(rollback_web_" + good2Tag + ")")
	testutil.Equal(t, e.exec.Count("docker"), 0)
	e2 := newEnv(t)
	e2.write(filepath.Join(e2.hubDir, "deploys.log"), "garbage\n")
	if err := os.Chmod(filepath.Join(e2.hubDir, "deploys.log"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(e2.hubDir, "deploys.log"), 0o600) })
	testutil.Equal(t, e2.dispatch("rollback web "+good2Tag, ""), ExitFail)
	testutil.Contains(t, e2.logs.String(), "journal read failed")
	e3 := newEnv(t)
	e3.deployedBefore(good2Tag)
	e3.write(filepath.Join(e3.hubDir, "deploys.log"), strings.Replace(e3.journal(), "deploy ok", "deploy fail", 1))
	testutil.Equal(t, e3.dispatch("rollback web "+good2Tag, ""), ExitFail)
	e3.expectOut(e3.err, msgNotGated)
	e3.expectLog("rollback target never passed a deploy")
}

func verdict(passed bool, commit string) gate.Verdict {
	status := gate.StatusPass
	if !passed {
		status = gate.StatusFail
	}
	return gate.Verdict{
		Subject: gate.Subject{App: appName, Repo: appRepo, Commit: commit, Own: true},
		Passed:  passed, Seconds: 321.5,
		Results: []gate.Result{
			{Check: checkTests, Status: gate.StatusPass, Values: map[string]float64{checkTests: 12}, Seconds: 3},
			{Check: checkMutation, Status: status, Reason: "score low", Values: map[string]float64{valueScore: 80}, Findings: []string{"a.go:1 survived"}, Seconds: 60},
			{Check: checkDAST, Status: gate.StatusNA, Reason: "no HTTP"},
		},
	}
}

func encodeVerdict(t *testing.T, v gate.Verdict) string {
	t.Helper()
	data, err := json.Marshal(v)
	testutil.NoErr(t, err)
	return "verdict=" + base64.StdEncoding.EncodeToString(data) + "\n"
}

func TestReportRecordsBlockedVerdict(t *testing.T) {
	e := newEnv(t)
	testutil.Equal(t, e.dispatch("report web "+goodTag, encodeVerdict(t, verdict(false, fullSha))), ExitOK)
	e.expectJournal("web@aaaaaaa gate fail 321s")
	e.expectOut(e.out, "gate web@aaaaaaa: blocked mutation")
	testutil.True(t, e.metrics.has(obs.MetricGateVerdict, obs.Labels{obs.LabelApp: appName, obs.LabelOwn: "true"}), "gate passed metric")
	testutil.Equal(t, sampleValue(e, obs.MetricGateVerdict), float64(0))
	testutil.Equal(t, sampleValue(e, obs.MetricGateFailedChecks), float64(1))
	testutil.Equal(t, sampleValue(e, obs.MetricGateDuration), 321.5)
	testutil.True(t, e.metrics.has(obs.MetricGateCheckValue, obs.Labels{obs.LabelCheck: checkMutation, obs.LabelName: valueScore}), "check value")
	testutil.Equal(t, checkSample(e, obs.MetricGateCheckVerdict, checkTests), float64(1))
	testutil.Equal(t, checkSample(e, obs.MetricGateCheckVerdict, checkMutation), float64(0))
	testutil.True(t, e.metrics.has(obs.MetricGateCheckDuration, obs.Labels{obs.LabelCheck: checkMutation}), "check duration")
	testutil.True(t, !e.metrics.has(obs.MetricGateCheckVerdict, obs.Labels{obs.LabelCheck: checkDAST}), "n/a check emits no pass metric")
	testutil.True(t, e.metrics.has(obs.MetricOperationsTotal, obs.Labels{obs.LabelAction: "gate", obs.LabelResult: "fail"}), "journal counter")
	testutil.Contains(t, e.logs.String(), `"msg":"gate check failed"`, `"check":"mutation"`, "a.go:1 survived", `"msg":"gate check not applicable"`, `"msg":"gate check passed"`)
	testutil.Equal(t, e.exec.Count("docker"), 0)
	testutil.Equal(t, registry.LoadState(e.appDir), registry.DeployState{})
	e.expectLog("gate verdict recorded")
}

func TestReportRecordsPassedVerdict(t *testing.T) {
	e := newEnv(t)
	v := verdict(true, fullSha)
	v.Own = false
	testutil.Equal(t, e.dispatch("report web "+goodTag, encodeVerdict(t, v)), ExitOK)
	e.expectJournal("web@aaaaaaa gate ok 321s")
	e.expectOut(e.out, "gate web@aaaaaaa: passed")
	testutil.Equal(t, sampleValue(e, obs.MetricGateVerdict), float64(1))
	testutil.True(t, e.metrics.has(obs.MetricGateVerdict, obs.Labels{obs.LabelOwn: "false"}), "own label")
}

func TestReportRejections(t *testing.T) {
	other := verdict(true, fullSha)
	other.App = byocMain
	inconsistent := verdict(true, fullSha)
	inconsistent.Passed = false
	cases := map[string]struct {
		stdin  string
		bare   bool
		want   string
		logged string
	}{
		"no identity":       {encodeVerdict(t, verdict(true, fullSha)), true, msgNoIdentity, ""},
		"no verdict":        {"", false, msgBadVerdict, truncatedJSON},
		"not base64":        {"verdict=%%%\n", false, msgBadVerdict, "illegal base64 data"},
		"not json":          {"verdict=" + base64.StdEncoding.EncodeToString([]byte("{")) + "\n", false, msgBadVerdict, truncatedJSON},
		"other app":         {encodeVerdict(t, other), false, msgBadVerdict, `"error":"` + msgBadVerdict + `"`},
		"other commit":      {encodeVerdict(t, verdict(true, strings.Repeat("b", 40))), false, msgBadVerdict, `"error":"` + msgBadVerdict + `"`},
		"inconsistent flag": {encodeVerdict(t, inconsistent), false, msgBadVerdict, "passed flag contradicts the results"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.bare = c.bare
			testutil.Equal(t, e.dispatch("report web "+goodTag, c.stdin), ExitFail)
			e.expectOut(e.err, c.want)
			e.expectJournal("refuse denied(report_web_" + goodTag + ")")
			testutil.True(t, !strings.Contains(e.journal(), " gate "), "no gate entry")
			if c.logged != "" {
				e.expectLog("gate verdict rejected")
				testutil.Contains(t, e.logs.String(), c.logged)
			}
		})
	}
}

func TestReportNeedsAppConf(t *testing.T) {
	e := newEnv(t)
	testutil.NoErr(t, os.Remove(filepath.Join(e.appDir, registry.AppConfFile)))
	testutil.Equal(t, e.dispatch("report web "+goodTag, encodeVerdict(t, verdict(true, fullSha))), ExitFail)
	e.expectOut(e.err, msgNoConf)
	testutil.Equal(t, e.journal(), "")
	testutil.Equal(t, e.exec.Count(""), 0)
}

func TestReportRequestShape(t *testing.T) {
	for _, raw := range []string{"report web", "report web latest", "report web " + goodTag + " x", "report Web " + goodTag} {
		_, err := ParseRequest(raw)
		testutil.Fails(t, err)
	}
	r, err := ParseRequest("report web " + goodTag)
	testutil.NoErr(t, err)
	testutil.Equal(t, r, Request{Verb: VerbReport, App: appName, Tag: goodTag})
}

func TestOIDCVerifierUsesFetchedKeys(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "jwks.json")
	v := OIDCVerifier{Fetcher: oidc.Fetcher{Client: &http.Client{}, URL: "http://127.0.0.1:1/jwks", Cache: cache, Log: slog.New(slog.DiscardHandler)}}
	_, err := v.Verify(context.Background(), "a.b.c", oidc.Expect{})
	testutil.Is(t, err, oidc.ErrKeys)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	testutil.NoErr(t, err)
	doc := `{"keys":[{"kty":"RSA","kid":"k","n":"` + base64.RawURLEncoding.EncodeToString(key.N.Bytes()) + `","e":"AQAB"}]}`
	testutil.Write(t, cache, doc)
	_, err = v.Verify(context.Background(), "a.b.c", oidc.Expect{})
	testutil.Is(t, err, oidc.ErrToken)
}
