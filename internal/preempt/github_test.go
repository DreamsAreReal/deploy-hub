package preempt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	fullInput = "full"
	inputOn   = "true"
)

type recorded struct {
	method, path, query, body string
}

func workflowServer(t *testing.T, status int, reply string) (GitHub, *[]recorded) {
	t.Helper()
	var calls []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, recorded{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery, body: string(body)})
		if r.Header.Get("Authorization") != "Bearer "+pat || r.Header.Get("Accept") != acceptJSON {
			http.Error(w, "Bad credentials", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return GitHub{API: srv.URL, Owner: owner, Token: pat, Client: srv.Client()}, &calls
}

func TestDispatchStartsTheWorkflowOnTheRefWithItsInputs(t *testing.T) {
	gh, calls := workflowServer(t, http.StatusNoContent, "")
	testutil.NoErr(t, gh.Dispatch(context.Background(), repoA, "deploy-stt.yml", MainBranch, map[string]string{fullInput: inputOn}))
	testutil.Equal(t, len(*calls), 1)
	call := (*calls)[0]
	testutil.Equal(t, call.method, http.MethodPost)
	testutil.Equal(t, call.path, "/repos/Owner/tasks/actions/workflows/deploy-stt.yml/dispatches")
	var body map[string]any
	testutil.NoErr(t, json.Unmarshal([]byte(call.body), &body))
	testutil.Equal(t, body, map[string]any{"ref": "main", "inputs": map[string]any{fullInput: inputOn}})
}

func TestDispatchFailsOnAnyOtherStatus(t *testing.T) {
	gh, _ := workflowServer(t, http.StatusUnprocessableEntity, `{"message":"Unexpected inputs provided"}`)
	err := gh.Dispatch(context.Background(), repoA, "deploy.yml", MainBranch, nil)
	testutil.Is(t, err, ErrHTTP)
	testutil.Contains(t, err.Error(), "422")
}

func TestDispatchedListsTheDispatchRunsOfTheWorkflowOnTheBranch(t *testing.T) {
	created := time.Date(2026, 11, 1, 21, 0, 5, 0, time.UTC)
	gh, calls := workflowServer(t, http.StatusOK, `{"workflow_runs":[{"id":41,"event":"workflow_dispatch","status":"queued",
		"path":".github/workflows/deploy.yml","html_url":"https://github.com/Owner/tasks/actions/runs/41","head_sha":"abc","created_at":"2026-11-01T21:00:05Z"}]}`)
	runs, err := gh.Dispatched(context.Background(), repoA, "deploy.yml", MainBranch)
	testutil.NoErr(t, err)
	testutil.Equal(t, runs, []Run{{ID: 41, Event: EventDispatch, Status: "queued", Path: ".github/workflows/deploy.yml",
		URL: "https://github.com/Owner/tasks/actions/runs/41", HeadSHA: "abc", Created: created}})
	call := (*calls)[0]
	testutil.Equal(t, call.method, http.MethodGet)
	testutil.Equal(t, call.path, "/repos/Owner/tasks/actions/workflows/deploy.yml/runs")
	testutil.Equal(t, call.query, "branch=main&event=workflow_dispatch&per_page=20")
}

func TestTheWorkflowNameIsEscapedInThePath(t *testing.T) {
	gh, calls := workflowServer(t, http.StatusOK, `{"workflow_runs":[]}`)
	_, err := gh.Dispatched(context.Background(), repoA, "a b.yml", MainBranch)
	testutil.NoErr(t, err)
	testutil.Equal(t, (*calls)[0].path, "/repos/Owner/tasks/actions/workflows/a%20b.yml/runs")
}
