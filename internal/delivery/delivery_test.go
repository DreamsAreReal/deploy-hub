package delivery_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestHTTPNotifierPostsAJSONArray(t *testing.T) {
	var got []delivery.Step
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer tracker.Close()
	n := delivery.HTTPNotifier{URL: tracker.URL, Client: tracker.Client(), Log: slog.New(slog.DiscardHandler)}
	testutil.NoErr(t, n.Notify(t.Context(), delivery.FromEntry(entry(journal.ActionDeploy, journal.ResultOK), "Owner/web")))
	testutil.Equal(t, len(got), 2)
	testutil.Equal(t, got[1].Step, delivery.StepLive)
}

func TestFailedPostKeepsTheSecretOutOfTheLog(t *testing.T) {
	var logs strings.Builder
	n := delivery.HTTPNotifier{URL: "http://127.0.0.1:1/api/deliveries/dl_secret", Client: &http.Client{},
		Log: slog.New(slog.NewTextHandler(&logs, nil))}
	testutil.Fails(t, n.Notify(t.Context(), delivery.FromEntry(entry(journal.ActionDeploy, journal.ResultOK), "Owner/web")))
	testutil.True(t, strings.Contains(logs.String(), "delivery steps not sent"), "failure logged: "+logs.String())
	testutil.True(t, !strings.Contains(logs.String(), "dl_secret"), "secret leaked: "+logs.String())
}

func TestNotifierOutcomeWithoutAPostOrAURL(t *testing.T) {
	calls := 0
	tracker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer tracker.Close()
	steps := delivery.FromEntry(entry(journal.ActionDeploy, journal.ResultOK), "Owner/web")
	testutil.NoErr(t, delivery.HTTPNotifier{URL: tracker.URL, Client: tracker.Client(), Log: slog.New(slog.DiscardHandler)}.Notify(t.Context(), nil))
	testutil.Is(t, delivery.HTTPNotifier{Client: tracker.Client(), Log: slog.New(slog.DiscardHandler)}.Notify(t.Context(), steps), delivery.ErrDisabled)
	testutil.Is(t, delivery.Discard{}.Notify(t.Context(), steps), delivery.ErrDisabled)
	testutil.Equal(t, calls, 0)
}

func TestAStepCarriesItsLinkOnlyWhenItHasOne(t *testing.T) {
	linked, err := json.Marshal(delivery.Step{Repo: label, Step: delivery.StepChecks, URL: "https://github.com/Owner/web/actions/runs/1"})
	testutil.NoErr(t, err)
	testutil.Contains(t, string(linked), `"url":"https://github.com/Owner/web/actions/runs/1"`)
	plain, err := json.Marshal(delivery.Step{Repo: label, Step: delivery.StepChecks})
	testutil.NoErr(t, err)
	testutil.Excludes(t, string(plain), "url")
}
