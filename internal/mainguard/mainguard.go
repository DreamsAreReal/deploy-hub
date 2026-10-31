package mainguard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/preempt"
)

const (
	DefaultState  = "/var/lib/deploy-hub/main-guard.json"
	DefaultHold   = 24 * time.Hour
	mainBranch    = "main"
	compareAhead  = "ahead"
	statusDone    = "completed"
	KindRewrite   = "rewrite"
	KindNoPR      = "no-pull-request"
	KindRedChecks = "checks-not-green"
	labelRepo     = "repo"
	conclusionOK  = "success"
	maxCommits    = 250
)

var ErrTooMany = errors.New("too many new commits on main to check")

var passing = map[string]bool{conclusionOK: true, "skipped": true, "neutral": true}

type Guard struct {
	GitHub  preempt.GitHub
	Repos   []string
	State   string
	Hold    time.Duration
	Now     func() time.Time
	Metrics obs.Pusher
	Log     *slog.Logger
}

type branchDoc struct {
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type compareDoc struct {
	Status  string `json:"status"`
	Commits []struct {
		SHA string `json:"sha"`
	} `json:"commits"`
}

type pullDoc struct {
	Number   int     `json:"number"`
	MergedAt *string `json:"merged_at"`
	Base     struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type checksDoc struct {
	Runs []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

func (g Guard) Run(ctx context.Context) error {
	st, err := Load(g.State)
	if err != nil {
		return err
	}
	samples := make([]obs.Sample, 0, len(g.Repos)+1)
	var errs []error
	for _, repo := range g.Repos {
		errs = append(errs, g.repo(ctx, repo, st))
		samples = append(samples, obs.Sample{Name: obs.MetricMainGuardOK, Labels: obs.Labels{labelRepo: repo}, Value: obs.Bool(st.clean(repo, g.Now().Add(-g.Hold)))})
	}
	errs = append(errs, st.Save(g.State))
	err = errors.Join(errs...)
	g.Metrics.Push(context.WithoutCancel(ctx), append(samples,
		obs.Sample{Name: obs.MetricMainGuardSuccess, Labels: obs.Labels{}, Value: obs.Bool(err == nil)}))
	return err
}

func (g Guard) repo(ctx context.Context, repo string, st State) error {
	var b branchDoc
	if err := g.GitHub.Repo(ctx, repo, "/branches/"+mainBranch, &b); err != nil {
		return err
	}
	head, seen := b.Commit.SHA, st[repo]
	if seen.Head == head {
		return nil
	}
	if seen.Head != "" {
		kind, sha, err := g.check(ctx, repo, seen.Head, head)
		if err != nil {
			return err
		}
		if kind != "" {
			seen.Violation, seen.Since = kind+" "+sha, g.Now()
			g.Log.Error("main moved without a green pull request", "repo", repo, "kind", kind, "commit", sha)
		}
	}
	seen.Head = head
	st[repo] = seen
	return nil
}

func (g Guard) check(ctx context.Context, repo, from, to string) (string, string, error) {
	var c compareDoc
	if err := g.GitHub.Repo(ctx, repo, "/compare/"+from+"..."+to, &c); err != nil {
		return "", "", err
	}
	if c.Status != compareAhead {
		return KindRewrite, to, nil
	}
	if len(c.Commits) >= maxCommits {
		return "", "", fmt.Errorf("%w: %s %s...%s", ErrTooMany, repo, from, to)
	}
	for _, commit := range c.Commits {
		kind, err := g.commit(ctx, repo, commit.SHA)
		if err != nil || kind != "" {
			return kind, commit.SHA, err
		}
	}
	return "", "", nil
}

func (g Guard) commit(ctx context.Context, repo, sha string) (string, error) {
	var pulls []pullDoc
	if err := g.GitHub.Repo(ctx, repo, "/commits/"+sha+"/pulls", &pulls); err != nil {
		return "", err
	}
	for _, p := range pulls {
		if p.MergedAt != nil && p.Base.Ref == mainBranch {
			return g.checks(ctx, repo, p.Head.SHA)
		}
	}
	return KindNoPR, nil
}

func (g Guard) checks(ctx context.Context, repo, sha string) (string, error) {
	var c checksDoc
	if err := g.GitHub.Repo(ctx, repo, "/commits/"+sha+"/check-runs?per_page=100", &c); err != nil {
		return "", err
	}
	green := false
	for _, r := range c.Runs {
		if r.Status != statusDone || !passing[r.Conclusion] {
			return KindRedChecks, nil
		}
		green = green || r.Conclusion == conclusionOK
	}
	if !green {
		return KindRedChecks, nil
	}
	return "", nil
}
