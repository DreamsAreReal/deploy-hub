package runner

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const deniedLabel = "denied"

func TestRefusedRequests(t *testing.T) {
	cases := []struct {
		raw, msg, fingerprint string
	}{
		{"", msgRefused, "empty"},
		{"rm -rf /", msgRefused, "rm_-rf_"},
		{"bash -c id", msgRefused, "bash_-c_id"},
		{"deploy ../etc sha-aaaaaaa", msgUnknownApp, "deploy_..etc_sha-aaaaaaa"},
		{"deploy WEB sha-aaaaaaa", msgUnknownApp, "deploy_WEB_sha-aaaaaaa"},
		{"deploy ghost sha-aaaaaaa", msgUnknownApp, "deploy_ghost_sha-aaaaaaa"},
		{"deploy web latest", msgBadTag, "deploy_web_latest"},
		{"deploy web sha-aaaaaaa;reboot", msgBadTag, "deploy_web_sha-aaaaaaareboot"},
		{reqInjection, msgBadTag, "deploy_web_id"},
		{"deploy web", msgBadTag, "deploy_web"},
		{"rollback web sha-ZZZ", msgBadTag, "rollback_web_sha-ZZZ"},
		{"rollback ghost", msgUnknownApp, "rollback_ghost"},
		{VerbHistory, msgUnknownApp, VerbHistory},
		{"history ghost", msgUnknownApp, "history_ghost"},
		{"history web;cat /etc/shadow", msgRefused, "history_webcat_etcshadow"},
		{"status && id", msgRefused, "status__id"},
	}
	for _, c := range cases {
		e := newEnv(t)
		defer e.expectLog("request refused")
		if code := e.dispatch(c.raw, ""); code != ExitFail {
			t.Fatalf("%q: exit %d", c.raw, code)
		}
		e.expectOut(e.err, c.msg)
		e.expectJournal("-@- refuse denied(" + c.fingerprint + ") 0s")
		if strings.Contains(e.journal(), "smoke=") {
			t.Fatalf("%q: refusals carry no smoke suffix", c.raw)
		}
		if len(e.exec.Calls()) != 0 {
			t.Fatalf("%q: refused request ran commands %v", c.raw, e.exec.Lines())
		}
		if !e.metrics.has(obs.MetricOperationsTotal, obs.Labels{obs.LabelAction: "refuse", obs.LabelResult: deniedLabel}) {
			t.Fatalf("%q: refusal metric missing", c.raw)
		}
	}
}

func TestFingerprintLimit(t *testing.T) {
	if got := Fingerprint(strings.Repeat("a", 100)); len(got) != fingerprintLimit {
		t.Fatalf("len %d", len(got))
	}
	testutil.Equal(t, Fingerprint("az AZ 09.-_"), "az_AZ_09.-_")
	testutil.Equal(t, Fingerprint("`{@[/:"), deniedEmpty)
}

func TestParseRequest(t *testing.T) {
	r, err := ParseRequest("  deploy   web   sha-abcdef0 ")
	if err != nil || r != (Request{Verb: VerbDeploy, App: appName, Tag: "sha-abcdef0"}) {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = ParseRequest("status")
	if err != nil || r != (Request{Verb: VerbStatus}) {
		t.Fatalf("%+v %v", r, err)
	}
	for _, raw := range []string{"status x", "history web extra", "history ../web", "deploy web sha-abcdef0 extra", "rollback web sha-abcdef0 x"} {
		if _, perr := ParseRequest(raw); perr == nil {
			t.Fatalf("%q: extra words must be refused", raw)
		}
	}
	r, err = ParseRequest("rollback web")
	if err != nil || r.Tag != "" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestMissingAppsListRefusesNamedApps(t *testing.T) {
	e := newEnv(t)
	e.svc.Apps.Path = e.hubDir + "/missing.list"
	if code := e.dispatch("history web", ""); code != ExitFail {
		t.Fatal("must refuse")
	}
	e.expectOut(e.err, msgUnknownApp)
}

func TestRemovedAppsLeaveCounters(t *testing.T) {
	const gone = "gone"
	e := newEnv(t)
	e.write(filepath.Join(e.hubDir, "deploys.log"),
		"[2026-07-17T00:09:31Z] "+gone+"@b3f4cf0 deploy fail 94s\n[2026-07-17T00:10:00Z] web@1111111 deploy ok 20s\n")
	e.dispatch("status", "")
	testutil.True(t, e.metrics.has(obs.MetricOperationsTotal, obs.Labels{obs.LabelApp: appName}), "registered app counted")
	testutil.True(t, !e.metrics.has(obs.MetricOperationsTotal, obs.Labels{obs.LabelApp: gone}), "removed app not counted")
	testutil.True(t, !e.metrics.has(obs.MetricLastIncidentTime, obs.Labels{obs.LabelApp: gone}), "removed app has no incident")
}
