package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	EnvFile    = ".env"
	keyFileEnv = "SOPS_AGE_KEY_FILE"
	fileMode   = 0o600
	tempSuffix = ".laying-*"
	exportWord = "export "
)

var (
	ErrLocked  = errors.New("secrets store is locked: the age key is not on this server; is the home router reachable?")
	errShape   = errors.New("secrets store must hold text values grouped by app and env file")
	errFile    = errors.New("env file of the secrets store must be a plain name ending in .env")
	envName    = regexp.MustCompile(`^([a-z0-9][a-z0-9_.-]*)?\.env$`)
	errDecrypt = errors.New("cannot decrypt the secrets store")
	errUnsafe  = errors.New("value cannot be written to .env safely")
	plainValue = regexp.MustCompile(`^[A-Za-z0-9_./:@+,=-]*$`)
)

type Store struct {
	File    string
	KeyFile string
	Sops    string
	Exec    procexec.Runner
}

type appValues map[string]map[string]string

func (s Store) Lay(ctx context.Context, app, dir string) ([]string, error) {
	if _, err := os.Stat(s.KeyFile); err != nil {
		return nil, ErrLocked
	}
	all, err := s.values(ctx)
	if err != nil {
		return nil, err
	}
	var laid []string
	for _, file := range Keys(all[app]) {
		if !envName.MatchString(file) {
			return laid, fmt.Errorf("%w: %s", errFile, file)
		}
		if err = layFile(filepath.Join(dir, file), all[app][file]); err != nil {
			return laid, err
		}
		for _, key := range Keys(all[app][file]) {
			laid = append(laid, file+":"+key)
		}
	}
	return laid, nil
}

func layFile(path string, values map[string]string) error {
	existing, err := os.ReadFile(filepath.Clean(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	merged, err := Merge(existing, values)
	if err != nil {
		return err
	}
	return writeAtomically(path, merged)
}

func (s Store) values(ctx context.Context) (map[string]appValues, error) {
	res, err := s.Exec.Run(ctx, procexec.Command{Name: s.Sops, Args: []string{"--decrypt", "--output-type", "json", s.File},
		Env: []string{keyFileEnv + "=" + s.KeyFile}})
	if err != nil {
		return nil, fmt.Errorf("%w %s (exit %d)", errDecrypt, s.File, procexec.ExitCode(errors.Unwrap(err)))
	}
	var all map[string]appValues
	if json.Unmarshal([]byte(res.Stdout), &all) != nil {
		return nil, errShape
	}
	return all, nil
}

func Keys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func Merge(existing []byte, values map[string]string) ([]byte, error) {
	written := map[string]bool{}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(existing), "\n"), "\n") {
		key := keyOf(line)
		if _, managed := values[key]; !managed {
			out = appendLine(out, line)
			continue
		}
		if written[key] {
			continue
		}
		pair, err := format(key, values[key])
		if err != nil {
			return nil, err
		}
		out, written[key] = append(out, pair), true
	}
	for _, key := range Keys(values) {
		if written[key] {
			continue
		}
		pair, err := format(key, values[key])
		if err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return []byte(strings.Join(out, "\n") + "\n"), nil
}

func appendLine(out []string, line string) []string {
	if line == "" && len(out) == 0 {
		return out
	}
	return append(out, line)
}

func keyOf(line string) string {
	trimmed := strings.TrimPrefix(strings.TrimSpace(line), exportWord)
	key, _, found := strings.Cut(trimmed, "=")
	if !found || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	return strings.TrimSpace(key)
}

func format(key, value string) (string, error) {
	switch {
	case plainValue.MatchString(value):
		return key + "=" + value, nil
	case !strings.ContainsAny(value, "'\n"):
		return key + "='" + value + "'", nil
	case !strings.ContainsAny(value, "$`"):
		return key + "=" + `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value) + `"`, nil
	}
	return "", fmt.Errorf("%s: %w", key, errUnsafe)
}

func writeAtomically(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+tempSuffix)
	if err != nil {
		return err
	}
	name := temp.Name()
	_, err = temp.Write(data)
	err = errors.Join(err, temp.Chmod(fileMode), temp.Close())
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
