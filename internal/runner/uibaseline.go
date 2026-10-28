package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	VerbBaselineGet  = "ui-baseline-get"
	VerbBaselinePut  = "ui-baseline-put"
	baselineTagFile  = "tag"
	baselineNewDir   = ".new"
	baselineOldDir   = ".old"
	baselineDirPerm  = 0o700
	MaxBaselineShots = 400
	MaxBaselineBytes = 64 << 20
	pngMagic         = "\x89PNG\r\n\x1a\n"
	shotSeparator    = ":"
)

var (
	shotName         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}\.png$`)
	errBadShots      = errors.New(msgBadShots)
	errBaselineStale = errors.New(msgBaselineStale)
)

func (s *Service) baselineGet(ctx context.Context, raw string, entry registry.Entry, tag string, stdin io.Reader) int {
	if _, code, ok := s.cacheRequest(ctx, raw, VerbBaselineGet, entry, tag, stdin); !ok {
		return code
	}
	from, shots, err := s.loadBaseline(entry.Name)
	if errors.Is(err, fs.ErrNotExist) {
		s.Log.Info("no ui baseline yet", "app", entry.Name)
		return ExitOK
	}
	if err != nil {
		s.Log.Error("ui baseline unreadable", "app", entry.Name, "error", err.Error())
		s.Err.Println(msgBaselineRead)
		return ExitFail
	}
	s.Out.Printf(msgBaselineFrom, from)
	for _, name := range slices.Sorted(maps.Keys(shots)) {
		s.Out.Printf(msgBaselineShot, name, base64.StdEncoding.EncodeToString(shots[name]))
	}
	s.Log.Info("ui baseline served", "app", entry.Name, "from", from, "shots", len(shots))
	return ExitOK
}

func (s *Service) baselinePut(ctx context.Context, raw string, entry registry.Entry, tag string, stdin io.Reader) int {
	c, code, ok := s.cacheRequest(ctx, raw, VerbBaselinePut, entry, tag, stdin)
	if !ok {
		return code
	}
	shots, err := decodeShots(c.meta.Shots)
	if err == nil && registry.LoadState(entry.Dir).Current != tag {
		err = errBaselineStale
	}
	if err != nil {
		s.Log.Warn("ui baseline rejected", "app", entry.Name, "tag", tag, "error", err.Error())
		return s.refuse(raw, err.Error())
	}
	release, err := s.CacheLocker.Acquire(ctx, func() {})
	if err != nil {
		s.Err.Println(msgCacheBusy)
		return ExitFail
	}
	defer release()
	if err := s.storeBaseline(entry.Name, tag, shots); err != nil {
		s.Log.Error("ui baseline not stored", "app", entry.Name, "error", err.Error())
		s.Err.Println(msgBaselineWrite)
		return ExitFail
	}
	s.Log.Info("ui baseline stored", "app", entry.Name, "tag", tag, "shots", len(shots))
	s.Out.Printf(msgBaselineStored, len(shots), tag)
	return ExitOK
}

func decodeShots(lines []string) (map[string][]byte, error) {
	if len(lines) == 0 || len(lines) > MaxBaselineShots {
		return nil, errBadShots
	}
	shots := make(map[string][]byte, len(lines))
	for _, line := range lines {
		name, encoded, found := strings.Cut(line, shotSeparator)
		data, err := base64.StdEncoding.DecodeString(encoded)
		if !found || err != nil || !shotName.MatchString(name) || !bytes.HasPrefix(data, []byte(pngMagic)) {
			return nil, errBadShots
		}
		shots[name] = data
	}
	return shots, nil
}

func (s *Service) loadBaseline(app string) (string, map[string][]byte, error) {
	root, err := os.OpenRoot(s.Paths.UIBaseline(app))
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = root.Close() }()
	from, err := root.ReadFile(baselineTagFile)
	if err != nil {
		return "", nil, err
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return "", nil, err
	}
	shots := map[string][]byte{}
	for _, item := range entries {
		if !item.Type().IsRegular() || !shotName.MatchString(item.Name()) {
			continue
		}
		data, err := root.ReadFile(item.Name())
		if err != nil {
			return "", nil, err
		}
		shots[item.Name()] = data
	}
	return strings.TrimSpace(string(from)), shots, nil
}

func (s *Service) storeBaseline(app, tag string, shots map[string][]byte) error {
	dir := s.Paths.UIBaseline(app)
	from, err := os.ReadFile(filepath.Join(dir, baselineTagFile))
	if err == nil && strings.TrimSpace(string(from)) == tag {
		return writeShots(dir, shots)
	}
	fresh, old := dir+baselineNewDir, dir+baselineOldDir
	if err := errors.Join(os.RemoveAll(fresh), os.RemoveAll(old), os.MkdirAll(fresh, baselineDirPerm)); err != nil {
		return err
	}
	if err := errors.Join(writeShots(fresh, shots), writeAtomic(filepath.Join(fresh, baselineTagFile), []byte(tag+"\n"))); err != nil {
		return err
	}
	if err := os.Rename(dir, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(fresh, dir); err != nil {
		return fmt.Errorf("ui baseline swap: %w", err)
	}
	return os.RemoveAll(old)
}

func writeShots(dir string, shots map[string][]byte) error {
	for _, name := range slices.Sorted(maps.Keys(shots)) {
		if err := writeAtomic(filepath.Join(dir, name), shots[name]); err != nil {
			return err
		}
	}
	return nil
}
