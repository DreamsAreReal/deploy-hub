package preempt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const statePerm = 0o600

type Entry struct {
	Repo       string    `json:"repo"`
	ID         int64     `json:"id"`
	WorkflowID int64     `json:"workflow_id"`
	Branch     string    `json:"branch"`
	Pending    bool      `json:"pending"`
	Reruns     int       `json:"reruns"`
	Changed    time.Time `json:"changed"`
}

type State struct {
	Runs []Entry `json:"runs"`
}

func Load(path string) (State, error) {
	var st State
	data, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}

func (s *State) Save(path string) error {
	data, _ := json.Marshal(s)
	staged := path + ".tmp"
	if err := os.WriteFile(staged, data, statePerm); err != nil {
		return err
	}
	return os.Rename(staged, path)
}

func (s *State) cancelled(repo string, r Run, now time.Time) {
	for i := range s.Runs {
		if s.Runs[i].Repo == repo && s.Runs[i].ID == r.ID {
			s.Runs[i].Pending, s.Runs[i].Changed = true, now
			return
		}
	}
	s.Runs = append(s.Runs, Entry{Repo: repo, ID: r.ID, WorkflowID: r.WorkflowID, Branch: r.HeadBranch, Pending: true, Changed: now})
}

func (s *State) maxReruns(repo string) int {
	most := 0
	for _, e := range s.Runs {
		if e.Repo == repo {
			most = max(most, e.Reruns)
		}
	}
	return most
}

func (s *State) prune(cutoff time.Time) {
	kept := s.Runs[:0]
	for _, e := range s.Runs {
		if e.Pending || e.Changed.After(cutoff) {
			kept = append(kept, e)
		}
	}
	s.Runs = kept
}
