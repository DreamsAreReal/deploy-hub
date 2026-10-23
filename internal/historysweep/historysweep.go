package historysweep

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	GitBinary       = "git"
	DefaultAPI      = "https://api.github.com"
	DefaultGit      = "https://github.com"
	DefaultAuthFile = "/etc/deploy-hub/github.token"
	HubApp          = "deploy-hub"
	targetSeparator = "@"
	rootLength      = 40
	tempPattern     = "deploy-hub-history-sweep-"
	bareName        = "repo.git"
	gitSuffix       = ".git"
	tokenUser       = "x-access-token:"
	apiVersion      = "2022-11-28"
	errorBodyLimit  = 512
	deletedFormat   = "repo=%s deleted=%d"
)

var (
	ErrTarget    = errors.New("invalid history sweep target: want owner/name@<40 hex root commit>")
	ErrNoTargets = errors.New("no history sweep targets: set history_sweep in hub.conf")
	ErrNoToken   = errors.New("GitHub token file is empty")
	ErrDelete    = errors.New("GitHub refused to delete the ref")
)

type Target struct {
	Repo string
	Root string
}

func ParseTargets(raw string) ([]Target, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil, ErrNoTargets
	}
	targets := make([]Target, 0, len(fields))
	for _, f := range fields {
		repo, root, _ := strings.Cut(f, targetSeparator)
		if validate.RepoSlug(repo) != nil || !isSHA(root) {
			return nil, fmt.Errorf("%w: %q", ErrTarget, f)
		}
		targets = append(targets, Target{Repo: repo, Root: root})
	}
	return targets, nil
}

func isSHA(s string) bool {
	return len(s) == rootLength && strings.Trim(s, "0123456789abcdef") == ""
}

func ReadToken(path string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", ErrNoToken
	}
	return token, nil
}

type Sweeper struct {
	Targets []Target
	API     string
	Git     string
	Token   string
	Client  *http.Client
	Exec    procexec.Runner
	TempDir string
	Journal journal.Journal
	Metrics obs.Pusher
	Now     func() time.Time
	Log     *slog.Logger
}

func (s Sweeper) Run(ctx context.Context) error {
	var errs []error
	for _, t := range s.Targets {
		started := s.Now()
		deleted, err := s.sweep(ctx, t)
		s.report(ctx, t, deleted, err, started)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Repo, err))
		}
	}
	return errors.Join(errs...)
}

func (s Sweeper) report(ctx context.Context, t Target, deleted int, err error, started time.Time) {
	labels := obs.Labels{"repo": t.Repo}
	s.Metrics.Push(context.WithoutCancel(ctx), []obs.Sample{
		{Name: obs.MetricHistorySweepSuccess, Labels: labels, Value: obs.Bool(err == nil)},
		{Name: obs.MetricHistorySweepDeleted, Labels: labels, Value: float64(deleted)},
	})
	if err == nil && deleted == 0 {
		s.Log.Info("history sweep clean", "repo", t.Repo)
		return
	}
	result := journal.ResultOK
	if err != nil {
		result = journal.ResultFail
		s.Log.Error("history sweep failed", "repo", t.Repo, "deleted", deleted, "error", err.Error())
	}
	e := journal.Entry{App: HubApp, Sha7: journal.NoValue, Action: journal.ActionHistorySweep, Result: result,
		Seconds: int(s.Now().Sub(started).Seconds()), Extra: fmt.Sprintf(deletedFormat, t.Repo, deleted)}
	if jerr := s.Journal.Append(e); jerr != nil {
		s.Log.Error("journal append failed", "error", jerr.Error())
	}
}

func (s Sweeper) sweep(ctx context.Context, t Target) (int, error) {
	dir, err := os.MkdirTemp(s.TempDir, tempPattern)
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	bare := filepath.Join(dir, bareName)
	out, err := s.git(ctx, "clone", "--quiet", "--bare", "--filter=blob:none", "--", s.Git+"/"+t.Repo+gitSuffix, bare)
	if err == nil {
		out, err = s.git(ctx, "--git-dir", bare, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags")
	}
	if err != nil {
		return 0, err
	}
	deleted := 0
	var errs []error
	for _, ref := range strings.Fields(out) {
		err := s.sweepRef(ctx, t, bare, ref)
		if errors.Is(err, errKept) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}

var errKept = errors.New("ref starts at the snapshot root")

func (s Sweeper) sweepRef(ctx context.Context, t Target, bare, ref string) error {
	out, err := s.git(ctx, "--git-dir", bare, "rev-list", "--max-parents=0", ref, "--")
	if err != nil {
		return err
	}
	roots := slices.Compact(slices.Sorted(slices.Values(strings.Fields(out))))
	if slices.Equal(roots, []string{t.Root}) {
		return errKept
	}
	if err := s.delete(ctx, t.Repo, ref); err != nil {
		return err
	}
	s.Log.Info("ref deleted", "repo", t.Repo, "ref", ref, "roots", strings.Join(roots, " "))
	return nil
}

func (s Sweeper) git(ctx context.Context, args ...string) (string, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(tokenUser + s.Token))
	res, err := s.Exec.Run(ctx, procexec.Command{Name: GitBinary, Args: args, Env: []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + auth,
	}})
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, procexec.Output(err))
	}
	return res.Stdout, nil
}

func (s Sweeper) delete(ctx context.Context, repo, ref string) error {
	endpoint := s.API + "/repos/" + repo + "/git/" + EscapeRef(ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("delete %s: %w", ref, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	return fmt.Errorf("%w %s: HTTP %d: %s", ErrDelete, ref, resp.StatusCode, strings.TrimSpace(string(body)))
}

func EscapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
