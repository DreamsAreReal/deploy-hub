package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	VerbCacheGet    = "mutation-cache-get"
	VerbCachePut    = "mutation-cache-put"
	cacheFile       = "verdicts.json"
	cacheSumFile    = "verdicts.sha256"
	cacheDirPerm    = 0o700
	tmpSuffix       = ".tmp"
	MaxCacheEntries = 100000
	MaxCacheBytes   = 16 << 20
)

var (
	errBadCache     = errors.New(msgBadCache)
	errCacheCorrupt = errors.New(msgCacheCorrupt)
	errCacheTooBig  = errors.New("mutation cache over its size limit")
)

func (s *Service) cacheRequest(ctx context.Context, raw, verb string, entry registry.Entry, tag string, stdin io.Reader) (Meta, int, bool) {
	conf, err := s.loadConf(entry.Dir, entry.Name)
	if err != nil {
		return Meta{}, ExitFail, false
	}
	meta, err := ReadMeta(stdin)
	if err == nil {
		err = s.authorize(ctx, verb, entry, conf, tag, meta.OIDC)
	}
	if err != nil {
		return meta, s.refuse(raw, refusalMessage(err)), false
	}
	return meta, ExitOK, true
}

func (s *Service) cacheGet(ctx context.Context, raw string, entry registry.Entry, tag string, stdin io.Reader) int {
	if _, code, ok := s.cacheRequest(ctx, raw, VerbCacheGet, entry, tag, stdin); !ok {
		return code
	}
	data, sum, err := s.loadCache(entry.Name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.Log.Info("no mutation cache yet", "app", entry.Name)
		return ExitOK
	case err != nil:
		s.Err.Println(msgCacheCorrupt)
		return ExitFail
	}
	s.Out.Printf(msgCacheServed, sum, base64.StdEncoding.EncodeToString(data))
	s.Log.Info("mutation cache served", "app", entry.Name, "tag", tag, "bytes", len(data), "sha256", sum)
	return ExitOK
}

func (s *Service) loadCache(app string) ([]byte, string, error) {
	root, err := os.OpenRoot(s.Paths.MutationCache(app))
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(cacheFile)
	if err != nil {
		return nil, "", err
	}
	want, err := root.ReadFile(cacheSumFile)
	sum := mutation.Digest(data)
	intact := err == nil && strings.TrimSpace(string(want)) == sum
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricMutationCacheIntact, Labels: obs.Labels{obs.LabelApp: app}, Value: obs.Bool(intact)})
	if !intact {
		s.Log.Error("stored mutation cache does not match its sha256", "app", app, "sha256", sum, "recorded", strings.TrimSpace(string(want)))
		return nil, "", errCacheCorrupt
	}
	return data, sum, nil
}

func (s *Service) cachePut(ctx context.Context, raw string, entry registry.Entry, tag string, stdin io.Reader) int {
	meta, code, ok := s.cacheRequest(ctx, raw, VerbCachePut, entry, tag, stdin)
	if !ok {
		return code
	}
	incoming, err := decodeCache(meta.Verdicts)
	if err != nil {
		s.Log.Warn("mutation verdicts rejected", "app", entry.Name, "tag", tag, "error", err.Error())
		return s.refuse(raw, msgBadCache)
	}
	release, err := s.CacheLocker.Acquire(ctx, func() {})
	if err != nil {
		s.Err.Println(msgCacheBusy)
		return ExitFail
	}
	defer release()
	merged := s.mergedCache(entry.Name, incoming)
	sum, err := s.saveCache(entry.Name, merged)
	if err != nil {
		s.Log.Error("mutation cache not stored", "app", entry.Name, "error", err.Error())
		s.Err.Println(msgCacheWrite)
		return ExitFail
	}
	s.Log.Info("mutation cache stored", "app", entry.Name, "tag", tag, "received", len(incoming.Entries), "verdicts", len(merged.Entries), "sha256", sum)
	s.Out.Printf(msgCacheStored, len(merged.Entries), sum)
	return ExitOK
}

func decodeCache(encoded string) (mutation.Verdicts, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return mutation.Verdicts{}, errors.Join(errBadCache, err)
	}
	return mutation.DecodeVerdicts(data)
}

func (s *Service) mergedCache(app string, incoming mutation.Verdicts) mutation.Verdicts {
	data, _, err := s.loadCache(app)
	var existing mutation.Verdicts
	if err == nil {
		existing, err = mutation.DecodeVerdicts(data)
	}
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.Log.Warn("stored mutation cache replaced", "app", app, "error", err.Error())
		}
		return incoming
	}
	merged := existing.Merge(incoming)
	if len(merged.Entries) > MaxCacheEntries {
		s.Log.Info("mutation cache over its size limit, keeping only the latest run", "app", app, "verdicts", len(merged.Entries))
		return incoming
	}
	return merged
}

func (s *Service) saveCache(app string, v mutation.Verdicts) (string, error) {
	dir := s.Paths.MutationCache(app)
	data, err := v.Encode()
	if err == nil && len(data) > MaxCacheBytes {
		err = fmt.Errorf("%w: %d bytes, the limit is %d", errCacheTooBig, len(data), MaxCacheBytes)
	}
	if err == nil {
		err = os.MkdirAll(dir, cacheDirPerm)
	}
	sum := mutation.Digest(data)
	if err == nil {
		err = writeAtomic(filepath.Join(dir, cacheFile), data)
	}
	if err == nil {
		err = writeAtomic(filepath.Join(dir, cacheSumFile), []byte(sum+"\n"))
	}
	return sum, err
}

func writeAtomic(path string, data []byte) error {
	tmp := path + tmpSuffix
	if err := os.WriteFile(tmp, data, fileMode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
