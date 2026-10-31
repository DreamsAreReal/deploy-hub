package hostsync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func openParent(dir string) (*os.Root, error) {
	if err := realDir(dir); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	opened, serr := r.Stat(".")
	now, lerr := os.Lstat(dir)
	if serr != nil || lerr != nil || !now.IsDir() || !os.SameFile(opened, now) {
		_ = r.Close()
		return nil, fmt.Errorf("%w: %s", ErrParent, dir)
	}
	return r, nil
}

func inParent(path string, do func(r *os.Root, name string) error) error {
	r, err := openParent(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(do(r, filepath.Base(path)), r.Close())
}
