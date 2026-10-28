package delivery_test

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/delivery"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	sha7     = "aaaaaaa"
	label    = "github.com/Owner/web"
	deployOK = "deploy ok"
	liveOK   = "live ok"
)

func entry(action, result string) journal.Entry {
	return journal.Entry{App: "web", Sha7: sha7, Action: action, Result: result}
}

func TestRepoLabel(t *testing.T) {
	testutil.Equal(t, delivery.RepoLabel("Owner/web"), label)
	testutil.Equal(t, delivery.RepoLabel("gitlab.com/group/app"), "gitlab.com/group/app")
	testutil.Equal(t, delivery.RepoLabel(" "), "")
}

func TestFromEntryMapsJournalActions(t *testing.T) {
	cases := []struct {
		entry journal.Entry
		want  []string
	}{
		{entry(journal.ActionGate, journal.ResultOK), []string{"checks ok"}},
		{entry(journal.ActionGate, journal.ResultFail), []string{"checks fail"}},
		{entry(journal.ActionDeploy, journal.ResultOK), []string{deployOK, liveOK}},
		{entry(journal.ActionDeploy, journal.ResultFail), []string{"deploy fail"}},
		{entry(journal.ActionRedeploy, journal.ResultOK), []string{deployOK, liveOK}},
		{entry(journal.ActionRollback, journal.ResultOK), []string{liveOK}},
		{entry(journal.ActionRollback, journal.ResultFail), nil},
		{entry(journal.ActionPrune, journal.ResultOK), nil},
	}
	for _, c := range cases {
		var got []string
		for _, step := range delivery.FromEntry(c.entry, "Owner/web") {
			testutil.Equal(t, step.Repo, label)
			testutil.Equal(t, step.SHA, sha7)
			testutil.Equal(t, step.Target, delivery.DefaultTarget)
			got = append(got, step.Step+" "+step.State)
		}
		testutil.Equal(t, got, c.want)
	}
	testutil.Equal(t, len(delivery.FromEntry(entry(journal.ActionDeploy, journal.ResultOK), "")), 0)
	testutil.Equal(t, len(delivery.FromEntry(journal.Entry{Sha7: journal.NoValue, Action: journal.ActionRefuse}, "Owner/web")), 0)
}

func TestLogNotifierWritesOneStructuredLinePerStep(t *testing.T) {
	var logs strings.Builder
	n := delivery.LogNotifier{Log: slog.New(slog.NewJSONHandler(&logs, nil))}
	testutil.NoErr(t, n.Notify(t.Context(), delivery.FromEntry(entry(journal.ActionDeploy, journal.ResultOK), "Owner/web")))
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	testutil.Equal(t, len(lines), 2)
	var live map[string]any
	testutil.NoErr(t, json.Unmarshal([]byte(lines[1]), &live))
	testutil.Equal(t, live["event"], delivery.EventStep)
	testutil.Equal(t, live["repo"], label)
	testutil.Equal(t, live["step"], delivery.StepLive)
	testutil.Equal(t, live["sha"], sha7)
	testutil.NoErr(t, n.Notify(t.Context(), nil))
	testutil.Equal(t, len(strings.Split(strings.TrimSpace(logs.String()), "\n")), 2)
}

func TestDiscardSaysDeliveryIsOff(t *testing.T) {
	steps := delivery.FromEntry(entry(journal.ActionDeploy, journal.ResultOK), "Owner/web")
	testutil.Is(t, delivery.Discard{}.Notify(t.Context(), steps), delivery.ErrDisabled)
}
