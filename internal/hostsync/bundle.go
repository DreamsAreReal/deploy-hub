package hostsync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	bundlePerm      = 0o700
	placeholderArgs = "deploy-hub-host-sync"
)

var (
	ErrSymlink    = errors.New("symlinks are refused")
	ErrNotRegular = errors.New("only regular files and directories are allowed")
	ErrUnlisted   = errors.New("bundle file is not listed in the host spec")
	ErrMissing    = errors.New("host spec file is missing from the bundle")
)

func (s Syncer) extract(ctx context.Context, ref, tmp string) (string, error) {
	bundle := filepath.Join(tmp, bundleDir)
	if err := os.Mkdir(bundle, bundlePerm); err != nil {
		return "", err
	}
	res, err := s.Exec.Run(ctx, procexec.Command{Name: DockerBinary, Args: []string{"create", "--pull", "never", ref, placeholderArgs}})
	if err != nil {
		return "", fmt.Errorf("cannot create a container from %s: %s", ref, procexec.Output(err))
	}
	id := strings.TrimSpace(res.Stdout)
	defer s.remove(context.WithoutCancel(ctx), id)
	src := id + ":" + policy.HostBundleDir + "/."
	if _, err := s.Exec.Run(ctx, procexec.Command{Name: DockerBinary, Args: []string{"cp", src, bundle}}); err != nil {
		return "", fmt.Errorf("cannot copy %s out of %s: %s", policy.HostBundleDir, ref, procexec.Output(err))
	}
	return bundle, nil
}

func (s Syncer) remove(ctx context.Context, id string) {
	if _, err := s.Exec.Run(ctx, procexec.Command{Name: DockerBinary, Args: []string{"rm", "-f", id}}); err != nil {
		s.Log.Warn("bundle container not removed", "container", id, "error", err.Error())
	}
}

func checkBundle(bundle string, spec policy.Host) error {
	listed := map[string]bool{}
	for _, f := range spec.Files {
		listed[f.Src] = true
	}
	seen := map[string]bool{}
	err := filepath.WalkDir(bundle, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(bundle, path)
		return checkEntry(filepath.ToSlash(rel), d.Type(), listed, seen)
	})
	if err != nil {
		return err
	}
	for src := range listed {
		if !seen[src] {
			return fmt.Errorf("%w: %s", ErrMissing, src)
		}
	}
	return nil
}

func checkEntry(rel string, mode fs.FileMode, listed, seen map[string]bool) error {
	switch {
	case mode&fs.ModeSymlink != 0:
		return fmt.Errorf("%w: %s", ErrSymlink, rel)
	case mode.IsDir():
		return nil
	case !mode.IsRegular():
		return fmt.Errorf("%w: %s", ErrNotRegular, rel)
	case !listed[rel]:
		return fmt.Errorf("%w: %s", ErrUnlisted, rel)
	}
	seen[rel] = true
	return nil
}
