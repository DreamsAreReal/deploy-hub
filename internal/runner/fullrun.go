package runner

import (
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	planNew        = "new"
	planContinue   = "continue"
	planSkip       = "skip"
	SessionMaxAge  = 72 * time.Hour
	NightlyAge     = 20 * time.Hour
	NightStartHour = 0
	NightEndHour   = 6
)

type cacheState struct {
	Requested      bool          `json:"requested"`
	SessionStarted *time.Time    `json:"session_started,omitempty"`
	Last           *gate.FullRun `json:"last,omitempty"`
}

func (s *Service) loadState(repo string) (cacheState, error) {
	var st cacheState
	data, _, err := s.loadStored(repo, stateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	return st, err
}

func (s *Service) saveState(repo string, st cacheState) error {
	data, err := json.Marshal(st)
	if err == nil {
		_, err = s.store(repo, stateFile, data)
	}
	return err
}

func (s *Service) fullPlan(st *cacheState) string {
	now := s.Now().UTC()
	if st.SessionStarted != nil && now.Sub(*st.SessionStarted) < SessionMaxAge {
		return planContinue
	}
	if !st.Requested && st.Last != nil && !nightlyDue(st.Last.Time, now) {
		st.SessionStarted = nil
		return planSkip
	}
	st.SessionStarted, st.Requested = &now, false
	return planNew
}

func nightlyDue(last, now time.Time) bool {
	return now.Sub(last) >= NightlyAge && now.Hour() >= NightStartHour && now.Hour() < NightEndHour
}

func (s *Service) recordRun(c cacheCall, st *cacheState, run mutation.RunSummary) error {
	full := c.meta.Mode == modeFull && st.SessionStarted != nil
	switch {
	case run.Complete && (full || (run.Fresh && c.meta.Mode != modeFull)):
		st.Last = &gate.FullRun{Commit: strings.TrimPrefix(c.tag, registry.TagPrefix), Bases: run.Bases, Passed: run.Passed, Time: s.Now().UTC()}
		st.SessionStarted, st.Requested = nil, false
		s.Log.Info("full mutation run recorded", "app", c.entry.Name, "repo", c.repo, "tag", c.tag, "passed", run.Passed, "bases", run.Bases)
		s.Out.Printf(msgFullRecorded, run.Passed)
		if err := s.drop(c.repo, sessionFile); err != nil {
			return err
		}
	case st.SessionStarted == nil && (run.Stale || gateAfterFailedFullRun(c, st.Last)):
		st.Requested = true
		s.Log.Info("full mutation run requested", "app", c.entry.Name, "repo", c.repo, "tag", c.tag, "bases", run.Bases)
	}
	return s.saveState(c.repo, *st)
}

func gateAfterFailedFullRun(c cacheCall, last *gate.FullRun) bool {
	return c.meta.Mode != modeFull && last != nil && !last.Passed && last.Commit != strings.TrimPrefix(c.tag, registry.TagPrefix)
}

func (s *Service) planFull(repo string, st *cacheState) (string, error) {
	plan := s.fullPlan(st)
	err := s.saveState(repo, *st)
	if err == nil && plan != planContinue {
		err = s.drop(repo, sessionFile)
	}
	if err == nil {
		s.Out.Printf(msgCachePlan, plan)
	}
	return plan, err
}
