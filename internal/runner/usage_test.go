package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/delivery"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func (e *env) sample(name string, labels obs.Labels) float64 {
	e.t.Helper()
	v, ok := e.metrics.value(name, labels)
	testutil.True(e.t, ok, "no sample "+name)
	return v
}

func TestRequestsCountedByVerbAndApp(t *testing.T) {
	e := newEnv(t)
	testutil.Equal(t, e.dispatch("status", ""), ExitOK)
	testutil.Equal(t, e.dispatch("history "+appName, ""), ExitOK)
	testutil.Equal(t, e.sample(obs.MetricRequest, obs.Labels{obs.LabelVerb: VerbStatus, obs.LabelApp: journal.NoValue}), 1.0)
	testutil.Equal(t, e.sample(obs.MetricRequest, obs.Labels{obs.LabelVerb: VerbHistory, obs.LabelApp: appName}), 1.0)
	testutil.True(t, !e.metrics.has(obs.MetricRequestRefused, obs.Labels{}), "nothing refused")
}

func TestRefusedRequestsCountedByVerbAndReason(t *testing.T) {
	cases := []struct {
		raw, verb, reason string
	}{
		{"explode", verbInvalid, reasonBadRequest},
		{"", verbInvalid, reasonBadRequest},
		{"deploy ghost " + goodTag, VerbDeploy, reasonUnknownApp},
		{"deploy " + appName + " latest", VerbDeploy, reasonBadTag},
		{"rollback " + appName + " " + goodTag, VerbRollback, reasonNotGated},
		{"report " + appName + " " + goodTag, VerbReport, reasonBadVerdict},
	}
	for _, c := range cases {
		e := newEnv(t)
		testutil.Equal(t, e.dispatch(c.raw, ""), ExitFail)
		testutil.Equal(t, e.sample(obs.MetricRequestRefused, obs.Labels{obs.LabelVerb: c.verb, obs.LabelReason: c.reason}), 1.0)
		e.expectLog("request refused")
		testutil.Contains(t, e.logs.String(), `"reason_code":"`+c.reason+`"`)
	}
}

func TestRefusalReasonCoversEveryMessage(t *testing.T) {
	for msg, reason := range refusalReasons {
		testutil.Equal(t, refusalReason(msg), reason)
	}
	testutil.Equal(t, refusalReason("refused: something new"), reasonOther)
	testutil.Equal(t, verbOf("  "), verbInvalid)
	testutil.Equal(t, verbOf("status now"), VerbStatus)
}

func TestDeliveryOutcomeIsMeasured(t *testing.T) {
	e := newEnv(t)
	e.svc.Delivery = &captureNotifier{}
	testutil.Equal(t, e.dispatch("deploy "+appName+" "+goodTag, meta()), ExitOK)
	testutil.Equal(t, e.sample(obs.MetricDeliverySuccess, obs.Labels{}), 1.0)

	failed := newEnv(t)
	failed.svc.Delivery = &captureNotifier{err: delivery.ErrRejected}
	testutil.Equal(t, failed.dispatch("deploy "+appName+" "+goodTag, meta()), ExitOK)
	testutil.Equal(t, failed.sample(obs.MetricDeliverySuccess, obs.Labels{}), 0.0)

	disabled := newEnv(t)
	disabled.svc.Delivery = delivery.Discard{}
	testutil.Equal(t, disabled.dispatch("deploy "+appName+" "+goodTag, meta()), ExitOK)
	testutil.True(t, !disabled.metrics.has(obs.MetricDeliverySuccess, obs.Labels{}), "disabled delivery is not a failure")

	quiet := newEnv(t)
	notes := &captureNotifier{}
	quiet.svc.Delivery = notes
	testutil.Equal(t, quiet.dispatch("status", ""), ExitOK)
	testutil.True(t, !quiet.metrics.has(obs.MetricDeliverySuccess, obs.Labels{}), "nothing to deliver")
	testutil.Equal(t, len(notes.steps), 0)
}

func TestUnreadableJournalKeepsCountersOut(t *testing.T) {
	e := newEnv(t)
	testutil.Equal(t, e.dispatch("status", ""), ExitOK)
	testutil.Equal(t, e.sample(obs.MetricJournalReadable, obs.Labels{}), 1.0)
	testutil.True(t, e.metrics.has(obs.MetricOperationsTotal, obs.Labels{}), "counters pushed")

	broken := newEnv(t)
	journalPath := filepath.Join(broken.hubDir, "deploys.log")
	testutil.NoErr(t, os.RemoveAll(journalPath))
	testutil.NoErr(t, os.MkdirAll(journalPath, 0o750))
	testutil.Equal(t, broken.dispatch("status", ""), ExitOK)
	testutil.Equal(t, broken.sample(obs.MetricJournalReadable, obs.Labels{}), 0.0)
	testutil.True(t, !broken.metrics.has(obs.MetricOperationsTotal, obs.Labels{}), "no fake counter reset")
	broken.expectLog("journal read failed, journal counters not pushed")
}

func TestCleanupFailuresAreLogged(t *testing.T) {
	e := newEnv(t)
	e.sim.startOld(goodTag)
	e.state(registry.DeployState{Current: goodTag})
	e.sim.logoutErr = errSim
	e.sim.rmErr = errSim
	testutil.Equal(t, e.dispatch("deploy "+appName+" "+good2Tag, meta()), ExitOK)
	e.expectLog("registry logout failed, credentials may remain in the docker config", "standby not removed, it keeps its memory", "no leftover standby removed")
	testutil.Excludes(t, e.logs.String(), "standby cleaned up")
}
