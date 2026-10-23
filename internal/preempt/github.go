package preempt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	acceptJSON  = "application/vnd.github+json"
	apiVersion  = "2022-11-28"
	maxDocument = 4 << 20
	httpTimeout = 30 * time.Second
	recentRuns  = 100
)

var ErrHTTP = errors.New("unexpected HTTP status")

// Run is the part of a GitHub workflow run that preemption looks at.
type Run struct {
	ID         int64  `json:"id"`
	WorkflowID int64  `json:"workflow_id"`
	Event      string `json:"event"`
	HeadBranch string `json:"head_branch"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Attempt    int    `json:"run_attempt"`
}

// GitHub talks to the Actions API of the owner's repos with the hub token.
type GitHub struct {
	API    string
	Owner  string
	Token  string
	Client *http.Client
}

func (g GitHub) do(ctx context.Context, method, path string, ok int, out any) error {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, g.API+path, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != ok {
		return fmt.Errorf("%w: %s %s: %s", ErrHTTP, method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (g GitHub) runsPath(repo string) string {
	return "/repos/" + g.Owner + "/" + repo + "/actions/runs"
}

// Recent lists the newest runs of a repo; active runs are among them.
func (g GitHub) Recent(ctx context.Context, repo string) ([]Run, error) {
	var doc struct {
		Runs []Run `json:"workflow_runs"`
	}
	err := g.do(ctx, http.MethodGet, g.runsPath(repo)+"?per_page="+strconv.Itoa(recentRuns), http.StatusOK, &doc)
	return doc.Runs, err
}

func (g GitHub) Get(ctx context.Context, repo string, id int64) (Run, error) {
	var r Run
	err := g.do(ctx, http.MethodGet, g.runsPath(repo)+"/"+strconv.FormatInt(id, 10), http.StatusOK, &r)
	return r, err
}

func (g GitHub) Cancel(ctx context.Context, repo string, id int64) error {
	return g.do(ctx, http.MethodPost, g.runsPath(repo)+"/"+strconv.FormatInt(id, 10)+"/cancel", http.StatusAccepted, nil)
}

func (g GitHub) Rerun(ctx context.Context, repo string, id int64) error {
	return g.do(ctx, http.MethodPost, g.runsPath(repo)+"/"+strconv.FormatInt(id, 10)+"/rerun", http.StatusCreated, nil)
}
