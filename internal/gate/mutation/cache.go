package mutation

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	actionSuffix = "-a"
	outputSuffix = "-d"
	workerSep    = "-"
	walkRoot     = "."
)

type workerCache struct {
	dir  string
	base map[string]bool
}

func (e *engine) workerCaches(workers int) ([]*workerCache, error) {
	base, err := cacheEntries(e.cacheDir)
	if err != nil {
		return nil, err
	}
	caches := make([]*workerCache, 0, workers)
	for w := range workers {
		c := &workerCache{dir: e.cacheDir + workerSep + strconv.Itoa(w), base: base}
		if err := c.seed(e.cacheDir); err != nil {
			return nil, err
		}
		caches = append(caches, c)
	}
	return caches, nil
}

func cacheEntries(dir string) (map[string]bool, error) {
	entries := map[string]bool{}
	err := fs.WalkDir(os.DirFS(dir), walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(path, actionSuffix) || strings.HasSuffix(path, outputSuffix)) {
			entries[path] = true
		}
		return err
	})
	return entries, err
}

func (c *workerCache) seed(from string) error {
	if err := os.MkdirAll(c.dir, dirPerm); err != nil {
		return err
	}
	for rel := range c.base {
		dst := filepath.Join(c.dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
			return err
		}
		if err := os.Link(filepath.Join(from, rel), dst); err != nil {
			return err
		}
	}
	return nil
}

func (c *workerCache) trim() error {
	root, err := os.OpenRoot(c.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var stale []string
	err = fs.WalkDir(root.FS(), walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !c.base[path] {
			stale = append(stale, path)
		}
		return err
	})
	for _, path := range stale {
		err = errors.Join(err, root.Remove(path))
	}
	return err
}

func (e *engine) dropWorkerCaches() {
	for _, c := range e.workers {
		_ = os.RemoveAll(c.dir)
	}
}
