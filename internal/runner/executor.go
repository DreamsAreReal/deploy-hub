package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

var ErrImport = errors.New("full mutation chunk rejected")

type FullJob struct {
	Plan          string `json:"plan"`
	App           string `json:"app"`
	Repo          string `json:"repo"`
	SessionSHA256 string `json:"session_sha256,omitempty"`
}

func (j FullJob) Runs() bool {
	return j.Plan == planNew || j.Plan == planContinue
}

func (s *Service) DispatchFull(ctx context.Context, app string) (FullJob, []byte, error) {
	entry, err := s.Apps.Lookup(app)
	if err != nil {
		return FullJob{}, nil, err
	}
	conf, err := s.loadConf(entry.Dir, entry.Name)
	if err != nil {
		return FullJob{}, nil, err
	}
	release, err := s.CacheLocker.Acquire(ctx, func() {})
	if err != nil {
		return FullJob{}, nil, err
	}
	defer release()
	st, err := s.loadState(app)
	if err != nil {
		return FullJob{}, nil, err
	}
	plan, err := s.planFull(app, &st)
	job := FullJob{Plan: plan, App: app, Repo: conf.Repo}
	if err != nil || plan != planContinue {
		return job, nil, err
	}
	data, sum, err := s.loadStored(app, sessionFile)
	if errors.Is(err, fs.ErrNotExist) {
		return job, nil, nil
	}
	job.SessionSHA256 = sum
	return job, data, err
}

func (s *Service) ImportFull(ctx context.Context, app, commit string, verdicts, run []byte) error {
	if validate.Commit(commit) != nil {
		return fmt.Errorf("%w: commit %q", ErrImport, commit)
	}
	v, err := mutation.DecodeVerdicts(verdicts)
	var summary mutation.RunSummary
	if err == nil {
		err = json.Unmarshal(run, &summary)
	}
	if err == nil {
		err = mutation.Verdicts{Version: 1, Bases: summary.Bases}.Check()
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrImport, err)
	}
	release, err := s.CacheLocker.Acquire(ctx, func() {})
	if err != nil {
		return err
	}
	defer release()
	c := cacheCall{entry: registry.Entry{Name: app}, tag: registry.TagPrefix + commit, meta: Meta{Mode: modeFull}}
	if s.keepUpload(c, v, &summary) != ExitOK {
		return fmt.Errorf("%w: the hub could not store it", ErrImport)
	}
	return nil
}
