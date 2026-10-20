package runner

import (
	"context"
	"errors"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/oidc"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

var (
	errNoIdentity = errors.New(msgNoIdentity)
	errNoRepo     = errors.New(msgNoRepo)
	errNotGated   = errors.New(msgNotGated)
	errBadDigest  = errors.New(msgBadDigest)
)

type Verifier interface {
	Verify(ctx context.Context, token string, want oidc.Expect) (oidc.Claims, error)
}

type OIDCVerifier struct {
	Fetcher oidc.Fetcher
}

func (v OIDCVerifier) Verify(ctx context.Context, token string, want oidc.Expect) (oidc.Claims, error) {
	keys, err := v.Fetcher.Keys(ctx)
	if err != nil {
		return oidc.Claims{}, err
	}
	return oidc.Verify(token, keys, want)
}

func (s *Service) authorize(ctx context.Context, verb string, entry registry.Entry, conf registry.AppConf, tag, token string) error {
	err := s.verifyIdentity(ctx, verb, entry, conf, tag, token)
	s.samples = append(s.samples, obs.Sample{
		Name:   obs.MetricIdentityVerified,
		Labels: obs.Labels{obs.LabelApp: entry.Name, obs.LabelVerb: verb},
		Value:  boolValue(err == nil),
	})
	return err
}

func (s *Service) verifyIdentity(ctx context.Context, verb string, entry registry.Entry, conf registry.AppConf, tag, token string) error {
	if token == "" {
		return errNoIdentity
	}
	if conf.Repo == "" {
		return errNoRepo
	}
	want := oidc.Expect{Audience: s.Audience, WorkflowRef: s.WorkflowRef, Repository: conf.Repo, Commit: strings.TrimPrefix(tag, registry.TagPrefix), Now: s.Now()}
	claims, err := s.Identity.Verify(ctx, token, want)
	if err != nil {
		s.Log.Warn("deploy identity rejected", "app", entry.Name, "verb", verb, "tag", tag, "error", err.Error())
		return err
	}
	s.Log.Info("deploy identity verified", "app", entry.Name, "verb", verb, "repository", claims.Repository,
		"workflow", claims.JobWorkflowRef, "run_id", claims.RunID, "actor", claims.Actor, "event", claims.EventName, "ref", claims.Ref)
	return nil
}

func (s *Service) previouslyDeployed(entry registry.Entry, tag string) bool {
	st := registry.LoadState(entry.Dir)
	if tag == st.Current || tag == st.Previous {
		return true
	}
	app := entry.Name
	entries, err := s.Journal.Entries()
	if err != nil {
		s.Log.Error("journal read failed", "app", app, "error", err.Error())
		return false
	}
	for _, sha := range journal.SuccessfulShas(journal.ForApp(entries, app)) {
		if sha == registry.Sha7(tag) {
			return true
		}
	}
	return false
}
