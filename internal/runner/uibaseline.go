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
	baselineKeyFile  = "render-key"
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
	renderKey        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errBadShots      = errors.New(msgBadShots)
	errBaselineStale = errors.New(msgBaselineStale)
)

func (s *Service) baselineGet(ctx context.Context, raw string, entry registry.Entry, tag string, stdin io.Reader) int {
	if _, code, ok := s.cacheRequest(ctx, raw, VerbBaselineGet, entry, tag, stdin); !ok {
		return code
	}
	from, key, shots, err := s.loadBaseline(entry.Name)
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
	s.Out.Printf(msgBaselineKey, key)
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
	if err == nil && !renderKey.MatchString(c.meta.Key) {
		err = errBadShots
	}
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
	if err := s.storeBaseline(entry.Name, baselineSet{tag: tag, key: c.meta.Key, shots: shots}); err != nil {
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

type baselineSet struct {
	tag   string
	key   string
	shots map[string][]byte
}

func (s *Service) loadBaseline(app string) (string, string, map[string][]byte, error) {
	root, err := os.OpenRoot(s.Paths.UIBaseline(app))
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = root.Close() }()
	from, err := root.ReadFile(baselineTagFile)
	key, keyErr := root.ReadFile(baselineKeyFile)
	if err = errors.Join(err, keyErr); err != nil {
		return "", "", nil, err
	}
	shots, err := readShots(root)
	return strings.TrimSpace(string(from)), strings.TrimSpace(string(key)), shots, err
}

func readShots(root *os.Root) (map[string][]byte, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	shots := map[string][]byte{}
	for _, item := range entries {
		if !item.Type().IsRegular() || !shotName.MatchString(item.Name()) {
			continue
		}
		data, err := root.ReadFile(item.Name())
		if err != nil {
			return nil, err
		}
		shots[item.Name()] = data
	}
	return shots, nil
}

func (s *Service) storeBaseline(app string, set baselineSet) error {
	dir := s.Paths.UIBaseline(app)
	from, tagErr := os.ReadFile(filepath.Clean(filepath.Join(dir, baselineTagFile)))
	key, keyErr := os.ReadFile(filepath.Clean(filepath.Join(dir, baselineKeyFile)))
	if tagErr == nil && keyErr == nil && strings.TrimSpace(string(from)) == set.tag && strings.TrimSpace(string(key)) == set.key {
		return writeShots(dir, set.shots)
	}
	fresh, old := dir+baselineNewDir, dir+baselineOldDir
	if err := errors.Join(os.RemoveAll(fresh), os.RemoveAll(old), os.MkdirAll(fresh, baselineDirPerm)); err != nil {
		return err
	}
	err := errors.Join(writeShots(fresh, set.shots), writeAtomic(filepath.Join(fresh, baselineTagFile), []byte(set.tag+"\n")),
		writeAtomic(filepath.Join(fresh, baselineKeyFile), []byte(set.key+"\n")))
	if err != nil {
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
