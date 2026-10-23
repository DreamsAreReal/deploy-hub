package mutation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

const (
	VerdictsFile    = "mutation-verdicts.json"
	verdictsVersion = 1
	SourceCache     = "cache"
	SourceRun       = "run"
)

var ErrVerdicts = errors.New("mutation verdict cache rejected")

type Verdicts struct {
	Version int               `json:"version"`
	Bases   []string          `json:"bases"`
	Entries map[string]Status `json:"entries"`
}

var final = []Status{StatusKilled, StatusSurvived, StatusNotCovered, StatusTimedOut, StatusExhausted, StatusNotViable}

func cacheable(s Status) bool {
	return slices.Contains(final, s)
}

func LoadVerdicts(path, digest string) (Verdicts, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Verdicts{}, fmt.Errorf("%w: %w", ErrVerdicts, err)
	}
	if got := Digest(data); got != digest {
		return Verdicts{}, fmt.Errorf("%w: sha256 %s, the hub recorded %s", ErrVerdicts, got, digest)
	}
	return decodeVerdicts(data)
}

func decodeVerdicts(data []byte) (Verdicts, error) {
	var v Verdicts
	if err := json.Unmarshal(data, &v); err != nil {
		return Verdicts{}, fmt.Errorf("%w: %w", ErrVerdicts, err)
	}
	if v.Version != verdictsVersion {
		return Verdicts{}, fmt.Errorf("%w: version %d, expected %d", ErrVerdicts, v.Version, verdictsVersion)
	}
	return v, nil
}

func (v Verdicts) lookup(key string) (Status, bool) {
	s, ok := v.Entries[key]
	return s, ok && cacheable(s)
}

func (v Verdicts) merge(o Verdicts) Verdicts {
	v.Bases = append(v.Bases, o.Bases...)
	slices.Sort(v.Bases)
	v.Bases = slices.Compact(v.Bases)
	for k, s := range o.Entries {
		if cacheable(s) {
			v.Entries[k] = s
		}
	}
	return v
}

func MergeVerdicts(out string, paths []string) (string, error) {
	all := Verdicts{Version: verdictsVersion, Entries: map[string]Status{}}
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			return "", err
		}
		v, err := decodeVerdicts(data)
		if err != nil {
			return "", err
		}
		all = all.merge(v)
	}
	return all.Save(out)
}

func (v Verdicts) Save(path string) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return Digest(data), os.WriteFile(path, data, filePerm)
}

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
