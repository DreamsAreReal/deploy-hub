package mainguard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const statePerm = 0o600

type Seen struct {
	Head      string    `json:"head"`
	Violation string    `json:"violation,omitempty"`
	Since     time.Time `json:"since,omitzero"`
}

type State map[string]Seen

func Load(path string) (State, error) {
	st := State{}
	data, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}

func (s State) Save(path string) error {
	data, _ := json.Marshal(s)
	staged := path + ".tmp"
	if err := os.WriteFile(staged, data, statePerm); err != nil {
		return err
	}
	return os.Rename(staged, path)
}

func (s State) clean(repo string, cutoff time.Time) bool {
	seen := s[repo]
	return seen.Violation == "" || seen.Since.Before(cutoff)
}
