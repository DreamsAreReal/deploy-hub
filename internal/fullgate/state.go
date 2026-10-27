package fullgate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const statePerm = 0o600

type Result struct {
	At     time.Time `json:"at"`
	Passed bool      `json:"passed"`
	SHA    string    `json:"sha"`
	URL    string    `json:"url"`
}

type Current struct {
	App        string    `json:"app"`
	Dispatched time.Time `json:"dispatched"`
	RunID      int64     `json:"run_id"`
	Yielded    bool      `json:"yielded"`
}

type State struct {
	Results map[string]Result    `json:"results"`
	Since   map[string]time.Time `json:"since"`
	Tried   map[string]time.Time `json:"tried"`
	Current *Current             `json:"current,omitempty"`
	Night   string               `json:"night"`
}

func Load(path string) (State, error) {
	st := State{Results: map[string]Result{}, Since: map[string]time.Time{}, Tried: map[string]time.Time{}}
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

func (s *State) track(targets []Target, now time.Time) {
	for _, t := range targets {
		if _, seen := s.Since[t.App]; !seen {
			s.Since[t.App] = now
		}
	}
}

func (s *State) lastAt(app string) time.Time {
	if r, ok := s.Results[app]; ok {
		return r.At
	}
	return time.Time{}
}

func (s *State) age(app string, now time.Time) time.Duration {
	if r, ok := s.Results[app]; ok {
		return now.Sub(r.At)
	}
	return now.Sub(s.Since[app])
}

func (s *State) turn(app string) time.Time {
	last := s.lastAt(app)
	if tried := s.Tried[app]; tried.After(last) {
		return tried
	}
	return last
}
