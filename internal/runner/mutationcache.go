package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	VerbCacheGet    = "mutation-cache-get"
	VerbCachePut    = "mutation-cache-put"
	cacheFile       = "verdicts.json"
	sessionFile     = "session.json"
	stateFile       = "state.json"
	sumSuffix       = ".sha256"
	cacheDirPerm    = 0o700
	tmpSuffix       = ".tmp"
	MaxCacheEntries = 100000
	MaxCacheBytes   = 16 << 20
	modeFull        = "full"
)

var (
	errBadCache     = errors.New(msgBadCache)
	errCacheCorrupt = errors.New(msgCacheCorrupt)
	errCacheTooBig  = errors.New("mutation cache over its size limit")
)

type cacheCall struct {
	raw   string
	entry registry.Entry
	conf  registry.AppConf
	repo  string
	tag   string
	meta  Meta
}

func (s *Service) cacheRequest(ctx context.Context, raw, verb string, entry registry.Entry, tag string, stdin io.Reader) (cacheCall, int, bool) {
	conf, err := s.loadConf(entry.Dir, entry.Name)
	if err != nil {
		return cacheCall{}, ExitFail, false
	}
	meta, err := ReadMeta(stdin)
	if err == nil {
		err = s.authorize(ctx, verb, entry, conf, tag, meta.OIDC)
	}
	var repo string
	if err == nil {
		repo, err = repoCache(conf)
	}
	if err != nil {
		return cacheCall{}, s.refuse(raw, refusalMessage(err)), false
	}
	return cacheCall{raw: raw, entry: entry, conf: conf, repo: repo, tag: tag, meta: meta}, ExitOK, true
}

func repoCache(conf registry.AppConf) (string, error) {
	name := strings.ToLower(path.Base(conf.Repo))
	if validate.RepoSlug(conf.Repo) != nil || validate.AppName(name) != nil {
		return "", errNoRepo
	}
	return name, nil
}

func (s *Service) cacheGet(ctx context.Context, raw string, entry registry.Entry, tag string, stdin io.Reader) int {
	c, code, ok := s.cacheRequest(ctx, raw, VerbCacheGet, entry, tag, stdin)
	if !ok {
		return code
	}
	st, err := s.loadState(c.repo)
	if err != nil {
		s.Err.Println(msgCacheCorrupt)
		return ExitFail
	}
	name := cacheFile
	if c.meta.Mode == modeFull {
		plan, err := s.planFull(c.repo, &st)
		if err != nil {
			return s.cacheWriteFailed(c.repo, err)
		}
		if plan != planContinue {
			return ExitOK
		}
		name = sessionFile
	}
	s.printDeployed(c)
	s.printLast(st)
	return s.serve(c.repo, name)
}

func (s *Service) printDeployed(c cacheCall) {
	current := registry.LoadState(c.entry.Dir).Current
	if c.conf.Image != "" && current != "" {
		s.Out.Printf(msgCacheDeployed, c.conf.Image, current)
	}
}

func (s *Service) serve(repo, name string) int {
	data, sum, err := s.loadStored(repo, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.Log.Info("no mutation cache yet", "repo", repo, "file", name)
		return ExitOK
	case err != nil:
		s.Err.Println(msgCacheCorrupt)
		return ExitFail
	}
	s.Out.Printf(msgCacheServed, sum, base64.StdEncoding.EncodeToString(data))
	s.Log.Info("mutation cache served", "repo", repo, "file", name, "bytes", len(data), "sha256", sum)
	return ExitOK
}

func (s *Service) printLast(st cacheState) {
	if st.Last == nil {
		return
	}
	data, err := json.Marshal(st.Last)
	if err == nil {
		s.Out.Printf(msgCacheLast, base64.StdEncoding.EncodeToString(data))
	}
}

func (s *Service) loadStored(repo, name string) ([]byte, string, error) {
	root, err := os.OpenRoot(s.Paths.MutationCache(repo))
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(name)
	if err != nil {
		return nil, "", err
	}
	want, err := root.ReadFile(name + sumSuffix)
	sum := mutation.Digest(data)
	intact := err == nil && strings.TrimSpace(string(want)) == sum
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricMutationCacheIntact, Labels: obs.Labels{obs.LabelApp: repo}, Value: obs.Bool(intact)})
	if !intact {
		s.Log.Error("stored mutation cache does not match its sha256", "repo", repo, "file", name, "sha256", sum, "recorded", strings.TrimSpace(string(want)))
		return nil, "", errCacheCorrupt
	}
	return data, sum, nil
}

func (s *Service) store(repo, name string, data []byte) (string, error) {
	dir := s.Paths.MutationCache(repo)
	sum := mutation.Digest(data)
	if len(data) > MaxCacheBytes {
		return sum, fmt.Errorf("%w: %s is %d bytes, the limit is %d", errCacheTooBig, name, len(data), MaxCacheBytes)
	}
	err := os.MkdirAll(dir, cacheDirPerm)
	if err == nil {
		err = writeAtomic(filepath.Join(dir, name), data)
	}
	if err == nil {
		err = writeAtomic(filepath.Join(dir, name+sumSuffix), []byte(sum+"\n"))
	}
	return sum, err
}

func (s *Service) drop(repo, name string) error {
	dir := s.Paths.MutationCache(repo)
	err := errors.Join(os.Remove(filepath.Join(dir, name)), os.Remove(filepath.Join(dir, name+sumSuffix)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Service) cachePut(ctx context.Context, raw string, entry registry.Entry, tag string, stdin io.Reader) int {
	c, code, ok := s.cacheRequest(ctx, raw, VerbCachePut, entry, tag, stdin)
	if !ok {
		return code
	}
	incoming, run, err := decodeUpload(c.meta)
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
	return s.keepUpload(c, incoming, run)
}

func (s *Service) keepUpload(c cacheCall, incoming mutation.Verdicts, run *mutation.RunSummary) int {
	repo := c.repo
	st, err := s.loadState(repo)
	if err != nil {
		st = cacheState{}
	}
	sum, err := s.mergeInto(repo, cacheFile, incoming)
	if err == nil && c.meta.Mode == modeFull && st.SessionStarted != nil {
		_, err = s.mergeInto(repo, sessionFile, incoming)
	}
	if err == nil && run != nil {
		err = s.recordRun(c, &st, *run)
	}
	if err != nil {
		return s.cacheWriteFailed(repo, err)
	}
	s.Log.Info("mutation cache stored", "app", c.entry.Name, "repo", repo, "tag", c.tag, "mode", c.meta.Mode, "received", len(incoming.Entries), "sha256", sum)
	s.Out.Printf(msgCacheStored, len(incoming.Entries), sum)
	return ExitOK
}

func (s *Service) cacheWriteFailed(repo string, err error) int {
	s.Log.Error("mutation cache not stored", "repo", repo, "error", err.Error())
	s.Err.Println(msgCacheWrite)
	return ExitFail
}

func decodeUpload(m Meta) (mutation.Verdicts, *mutation.RunSummary, error) {
	v, err := decodeCache(m.Verdicts)
	if err != nil || m.Run == "" {
		return v, nil, err
	}
	data, err := base64.StdEncoding.DecodeString(m.Run)
	var run mutation.RunSummary
	if err == nil {
		err = json.Unmarshal(data, &run)
	}
	if err == nil {
		err = mutation.Verdicts{Version: 1, Bases: run.Bases}.Check()
	}
	return v, &run, err
}

func decodeCache(encoded string) (mutation.Verdicts, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return mutation.Verdicts{}, errors.Join(errBadCache, err)
	}
	return mutation.DecodeVerdicts(data)
}

func (s *Service) mergeInto(repo, name string, incoming mutation.Verdicts) (string, error) {
	merged := incoming
	data, _, err := s.loadStored(repo, name)
	var existing mutation.Verdicts
	if err == nil {
		existing, err = mutation.DecodeVerdicts(data)
	}
	switch {
	case err == nil && len(existing.Entries)+len(incoming.Entries) <= MaxCacheEntries:
		merged = existing.Merge(incoming)
	case err == nil:
		s.Log.Info("mutation cache over its size limit, keeping only the latest run", "repo", repo, "file", name)
	case !errors.Is(err, fs.ErrNotExist):
		s.Log.Warn("stored mutation cache replaced", "repo", repo, "file", name, "error", err.Error())
	}
	encoded, err := merged.Encode()
	if err != nil {
		return "", err
	}
	return s.store(repo, name, encoded)
}

func writeAtomic(path string, data []byte) error {
	tmp := path + tmpSuffix
	if err := os.WriteFile(tmp, data, fileMode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
