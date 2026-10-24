package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	DefaultRunnerDir = "/opt/deploy-hub/ci-runner"
	SetupCommand     = "ci-runner-setup"
	VerbRunnerSync   = "ci-runner-sync"
	setupScope       = "host"
	scriptPerm       = 0o755
	dataPerm         = 0o644
	dirPerm          = 0o755
	shebang          = "#!"
)

var ErrRunnerSetup = errors.New("ci-runner-setup host failed")

type RunnerFiles struct {
	Files   fs.FS
	Dir     string
	Exec    procexec.Runner
	Runners Runners
}

func (r RunnerFiles) Sync(ctx context.Context) (bool, error) {
	changed, err := r.install(ctx)
	r.Runners.RestartIdle(ctx)
	return changed, err
}

func (r RunnerFiles) install(ctx context.Context) (bool, error) {
	same, err := r.installed()
	if err != nil || same {
		return false, err
	}
	staged, previous := r.Dir+candidateSuffix, r.Dir+previousSuffix
	if err := r.stage(staged); err != nil {
		_ = os.RemoveAll(staged)
		return false, err
	}
	_ = os.RemoveAll(previous)
	hadPrevious := false
	if _, serr := os.Stat(r.Dir); serr == nil {
		if err := os.Rename(r.Dir, previous); err != nil {
			_ = os.RemoveAll(staged)
			return false, err
		}
		hadPrevious = true
	}
	if err := os.Rename(staged, r.Dir); err != nil {
		return false, errors.Join(err, r.putBack(hadPrevious))
	}
	if err := r.setup(ctx); err != nil {
		return false, errors.Join(err, r.rollback(ctx, hadPrevious))
	}
	return true, r.Runners.MarkAll()
}

func (r RunnerFiles) setup(ctx context.Context) error {
	_, err := r.Exec.Run(ctx, procexec.Command{Name: SetupCommand, Args: []string{setupScope}})
	if err != nil {
		return fmt.Errorf("%w: %w: %s", ErrRunnerSetup, err, procexec.Output(err))
	}
	return nil
}

func (r RunnerFiles) rollback(ctx context.Context, hadPrevious bool) error {
	if err := os.RemoveAll(r.Dir); err != nil || !hadPrevious {
		return err
	}
	if err := r.putBack(true); err != nil {
		return err
	}
	return r.setup(context.WithoutCancel(ctx))
}

func (r RunnerFiles) putBack(hadPrevious bool) error {
	if !hadPrevious {
		return nil
	}
	return os.Rename(r.Dir+previousSuffix, r.Dir)
}

func (r RunnerFiles) tree() (map[string][]byte, error) {
	entries, err := fs.ReadDir(r.Files, ".")
	if err != nil {
		return nil, err
	}
	tree := make(map[string][]byte, len(entries))
	for _, e := range entries {
		data, ferr := fs.ReadFile(r.Files, e.Name())
		if ferr != nil {
			return nil, ferr
		}
		tree[path.Base(e.Name())] = data
	}
	return tree, nil
}

func (r RunnerFiles) installed() (bool, error) {
	want, err := r.tree()
	if err != nil {
		return false, err
	}
	have, err := os.ReadDir(r.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil || len(have) != len(want) {
		return false, err
	}
	for name, data := range want {
		if !r.holds(name, data) {
			return false, nil
		}
	}
	return true, nil
}

func (r RunnerFiles) holds(name string, data []byte) bool {
	p := filepath.Join(r.Dir, name)
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != fileMode(data) {
		return false
	}
	disk, err := os.ReadFile(filepath.Clean(p))
	return err == nil && bytes.Equal(disk, data)
}

func (r RunnerFiles) stage(dir string) error {
	tree, err := r.tree()
	if err != nil {
		return err
	}
	_ = os.RemoveAll(dir)
	if err := os.Mkdir(dir, dirPerm); err != nil {
		return err
	}
	for name, data := range tree {
		if err := writeMode(filepath.Join(dir, name), data, fileMode(data)); err != nil {
			return err
		}
	}
	return nil
}

func fileMode(data []byte) os.FileMode {
	if bytes.HasPrefix(data, []byte(shebang)) {
		return scriptPerm
	}
	return dataPerm
}
