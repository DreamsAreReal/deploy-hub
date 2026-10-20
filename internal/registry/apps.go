package registry

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultAppsRoot = "/opt"
	commentPrefix   = "#"
)

var ErrUnknownApp = errors.New("app not in allowlist")

type Entry struct {
	Name string
	Dir  string
}

type Apps struct {
	Path string
	Root string
}

func (a Apps) root() string {
	if a.Root == "" {
		return defaultAppsRoot
	}
	return a.Root
}

func ParseApps(text, root string) []Entry {
	var out []Entry
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], commentPrefix) {
			continue
		}
		dir := filepath.Join(root, fields[0])
		if len(fields) > 1 {
			dir = fields[1]
		}
		out = append(out, Entry{Name: fields[0], Dir: dir})
	}
	return out
}

func (a Apps) All() ([]Entry, error) {
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return nil, err
	}
	return ParseApps(string(data), a.root()), nil
}

func (a Apps) Lookup(name string) (Entry, error) {
	entries, err := a.All()
	if err != nil {
		return Entry{}, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, nil
		}
	}
	return Entry{}, ErrUnknownApp
}

func (a Apps) DirFor(name string) string {
	if e, err := a.Lookup(name); err == nil {
		return e.Dir
	}
	return filepath.Join(a.root(), name)
}

func (a Apps) Add(e Entry) error {
	f, err := os.OpenFile(a.Path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, filePerm)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(e.Name + " " + e.Dir + "\n")
	return errors.Join(werr, f.Close())
}
