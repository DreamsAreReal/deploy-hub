package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
)

const (
	StepChecks = "checks"
	StepDeploy = "deploy"
	StepLive   = "live"

	StateOK   = "ok"
	StateFail = "fail"

	DefaultTarget = "prod"
	githubHost    = "github.com/"
	hostSeparator = "."
	sendTimeout   = 5 * time.Second
	firstBadCode  = 300
	contentType   = "application/json"
	detailRestore = "rollback"
)

var (
	ErrRejected = errors.New("delivery step rejected")
	ErrDisabled = errors.New("delivery disabled")
)

type Step struct {
	Repo   string `json:"repo"`
	Target string `json:"target"`
	SHA    string `json:"sha"`
	Step   string `json:"step"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type Notifier interface {
	Notify(ctx context.Context, steps []Step) error
}

func RepoLabel(repo string) string {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return ""
	}
	if first, _, _ := strings.Cut(repo, "/"); strings.Contains(first, hostSeparator) {
		return repo
	}
	return githubHost + repo
}

func FromEntry(e journal.Entry, repo string) []Step {
	label := RepoLabel(repo)
	if label == "" || e.Sha7 == "" || e.Sha7 == journal.NoValue {
		return nil
	}
	base := Step{Repo: label, Target: DefaultTarget, SHA: e.Sha7}
	switch {
	case e.Action == journal.ActionGate:
		return []Step{with(base, StepChecks, stateOf(e.Result), "")}
	case e.Action == journal.ActionDeploy || e.Action == journal.ActionRedeploy:
		if e.Result != journal.ResultOK {
			return []Step{with(base, StepDeploy, StateFail, "")}
		}
		return []Step{with(base, StepDeploy, StateOK, ""), with(base, StepLive, StateOK, "")}
	case e.Action == journal.ActionRollback && e.Result == journal.ResultOK:
		return []Step{with(base, StepLive, StateOK, detailRestore)}
	}
	return nil
}

func with(base Step, step, state, detail string) Step {
	base.Step, base.State, base.Detail = step, state, detail
	return base
}

func stateOf(result string) string {
	if result == journal.ResultOK {
		return StateOK
	}
	return StateFail
}

type HTTPNotifier struct {
	URL    string
	Client *http.Client
	Log    *slog.Logger
}

func (n HTTPNotifier) Notify(ctx context.Context, steps []Step) error {
	if n.URL == "" {
		return ErrDisabled
	}
	if len(steps) == 0 {
		return nil
	}
	if err := n.send(ctx, steps); err != nil {
		n.Log.Warn("delivery steps not sent", "error", withoutURL(err), "steps", len(steps))
		return err
	}
	n.Log.Info("delivery steps sent", "steps", len(steps))
	return nil
}

func withoutURL(err error) string {
	var failed *url.Error
	if errors.As(err, &failed) {
		return failed.Op + ": " + failed.Err.Error()
	}
	return err.Error()
}

func (n HTTPNotifier) send(ctx context.Context, steps []Step) error {
	body, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := n.Client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= firstBadCode {
		return fmt.Errorf("%w: status %d", ErrRejected, resp.StatusCode)
	}
	return nil
}

type Discard struct{}

func (Discard) Notify(context.Context, []Step) error { return ErrDisabled }
