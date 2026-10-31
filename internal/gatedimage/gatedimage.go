package gatedimage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	DockerBinary    = "docker"
	BundlePerm      = 0o700
	placeholderArgs = "deploy-hub-bundle"
	digestSeparator = "@"
)

var (
	ErrNotGated   = errors.New("commit has no passing gate verdict in the journal")
	ErrNoDigest   = errors.New("image was not pulled by digest from the app's registry")
	ErrSymlink    = errors.New("symlinks are refused")
	ErrNotRegular = errors.New("only regular files and directories are allowed")
	ErrUnlisted   = errors.New("bundle file is not listed in the spec")
	ErrMissing    = errors.New("spec file is missing from the bundle")
)

type Source struct {
	Exec    procexec.Runner
	Journal journal.Journal
	Log     *slog.Logger
}

func (s Source) Gated(app, tag string) error {
	entries, err := s.Journal.Entries()
	if err != nil {
		return err
	}
	sha7 := registry.Sha7(tag)
	for _, e := range journal.ForApp(entries, app) {
		if e.Sha7 == sha7 && e.Action == journal.ActionGate && e.Result == journal.ResultOK {
			return nil
		}
	}
	return fmt.Errorf("%w: %s@%s", ErrNotGated, app, sha7)
}

func (s Source) Pinned(ctx context.Context, image, tag string) (string, error) {
	res, err := s.Exec.Run(ctx, procexec.Command{Name: DockerBinary, Args: []string{"image", "inspect", "--format", "{{json .RepoDigests}}", image + ":" + tag}})
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoDigest, procexec.Output(err))
	}
	var digests []string
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &digests); jerr != nil {
		return "", fmt.Errorf("%w: %s", ErrNoDigest, jerr.Error())
	}
	for _, d := range digests {
		repo, digest, _ := strings.Cut(d, digestSeparator)
		if repo == image && validate.Digest(digest) == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("%w: %s:%s", ErrNoDigest, image, tag)
}

func (s Source) Extract(ctx context.Context, ref, from, to string) error {
	if err := os.Mkdir(to, BundlePerm); err != nil {
		return err
	}
	res, err := s.Exec.Run(ctx, procexec.Command{Name: DockerBinary, Args: []string{"create", "--pull", "never", ref, placeholderArgs}})
	if err != nil {
		return fmt.Errorf("cannot create a container from %s: %s", ref, procexec.Output(err))
	}
	id := strings.TrimSpace(res.Stdout)
	defer s.remove(context.WithoutCancel(ctx), id)
	if _, err := s.Exec.Run(ctx, procexec.Command{Name: DockerBinary, Args: []string{"cp", id + ":" + from + "/.", to}}); err != nil {
		return fmt.Errorf("cannot copy %s out of %s: %s", from, ref, procexec.Output(err))
	}
	return nil
}

func (s Source) remove(ctx context.Context, id string) {
	if _, err := s.Exec.Run(ctx, procexec.Command{Name: DockerBinary, Args: []string{"rm", "-f", id}}); err != nil {
		s.Log.Warn("bundle container not removed", "container", id, "error", err.Error())
	}
}

func CheckBundle(bundle string, srcs []string) error {
	listed := map[string]bool{}
	for _, src := range srcs {
		listed[src] = true
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
	for _, src := range srcs {
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
