package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	GoBinary        = "go"
	TarBinary       = "tar"
	downloadTimeout = 5 * time.Minute
	downloadTries   = 3
	maxDownload     = 512 << 20
	binPerm         = 0o700
	cachePerm       = 0o600
	dirPerm         = 0o700
	firstBadStatus  = 300
	gobinEnv        = "GOBIN="
	parentDir       = ".."
	treesDir        = "trees.d"
	stagePattern    = ".unpack-*"
	archiveFile     = "archive"
	rootDir         = "root"
	wholeArchive    = "."
	dirFlag         = "-C"
)

var (
	ErrManifest = errors.New("invalid tools manifest")
	ErrChecksum = errors.New("checksum mismatch")
	ErrMember   = errors.New("archive member not found")
	toolName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	trustedURLs = []*regexp.Regexp{
		regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/releases/download/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`),
		regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/archive/refs/tags/[A-Za-z0-9_.-]+\.tar\.gz$`),
		regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/archive/[0-9a-f]{40}\.tar\.gz$`),
		regexp.MustCompile(`^https://www\.busybox\.net/downloads/busybox-[0-9]+\.[0-9]+\.[0-9]+\.tar\.bz2$`),
		regexp.MustCompile(`^https://nodejs\.org/dist/v[0-9]+\.[0-9]+\.[0-9]+/[A-Za-z0-9_.-]+$`),
		regexp.MustCompile(`^https://builds\.dotnet\.microsoft\.com/dotnet/Sdk/[0-9]+\.[0-9]+\.[0-9]+/[A-Za-z0-9_.-]+$`),
	}
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	memberPath = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	goTool     = regexp.MustCompile(`^[a-z0-9.-]+(/[A-Za-z0-9_.-]+)+@v[0-9]+\.[0-9]+\.[0-9]+$`)
)

type Binary struct {
	Name   string   `yaml:"name"`
	URL    string   `yaml:"url"`
	SHA256 string   `yaml:"sha256"`
	Member string   `yaml:"member"`
	Tree   string   `yaml:"tree"`
	Links  []string `yaml:"links"`
}

type Manifest struct {
	Binaries []Binary `yaml:"binaries"`
	Go       []string `yaml:"go"`
	Sources  []Source `yaml:"sources"`
	dir      string
}

func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(data, filepath.Dir(path))
}

func ParseManifest(data []byte, dir string) (Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifest, err)
	}
	m.dir = dir
	return m, m.Validate()
}

func (m Manifest) Validate() error {
	var errs []error
	for _, b := range m.Binaries {
		errs = append(errs, b.validate())
	}
	for _, g := range m.Go {
		if !goTool.MatchString(g) {
			errs = append(errs, fmt.Errorf("%w: go tool %q must be module/path@vX.Y.Z", ErrManifest, g))
		}
	}
	return errors.Join(append(errs, validateSources(m))...)
}

func (b Binary) validate() error {
	switch {
	case !toolName.MatchString(b.Name):
		return fmt.Errorf("%w: name %q", ErrManifest, b.Name)
	case !trusted(b.URL):
		return fmt.Errorf("%w: %s: url must be a release asset of a trusted host", ErrManifest, b.Name)
	case !sha256Hex.MatchString(b.SHA256):
		return fmt.Errorf("%w: %s: sha256 must be 64 hex characters", ErrManifest, b.Name)
	case b.Member != "" && !inside(b.Member):
		return fmt.Errorf("%w: %s: bad archive member", ErrManifest, b.Name)
	default:
		return b.validateTree()
	}
}

func (b Binary) validateTree() error {
	switch {
	case b.Tree == "" && len(b.Links) == 0:
		return nil
	case b.Member != "" || b.Tree == "" || len(b.Links) == 0:
		return fmt.Errorf("%w: %s: a tree needs links and excludes a member", ErrManifest, b.Name)
	case b.Tree != wholeArchive && !inside(b.Tree):
		return fmt.Errorf("%w: %s: bad tree", ErrManifest, b.Name)
	case slices.ContainsFunc(b.Links, func(l string) bool { return !inside(l) || !toolName.MatchString(path.Base(l)) }):
		return fmt.Errorf("%w: %s: bad link", ErrManifest, b.Name)
	default:
		return nil
	}
}

func trusted(url string) bool {
	return slices.ContainsFunc(trustedURLs, func(re *regexp.Regexp) bool { return re.MatchString(url) })
}

func inside(member string) bool {
	return memberPath.MatchString(member) && path.Clean(member) == member && !strings.HasPrefix(member, parentDir)
}

type Installer struct {
	Client *http.Client
	Dir    string
	Shared string
	Cache  string
	Retry  time.Duration
	Exec   procexec.Runner
	Log    *slog.Logger
}

func (i Installer) Install(ctx context.Context, m Manifest) error {
	if err := os.MkdirAll(i.Dir, dirPerm); err != nil {
		return err
	}
	for _, step := range []func(context.Context, Manifest) error{i.installBinaries, i.installGo, i.buildSources} {
		if err := step(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (i Installer) installBinaries(ctx context.Context, m Manifest) error {
	for _, b := range m.Binaries {
		if err := i.fetch(ctx, b); err != nil {
			return fmt.Errorf("%s: %w", b.Name, err)
		}
		i.Log.Info("tool installed", "tool", b.Name, "sha256", b.SHA256)
	}
	return nil
}

func (i Installer) installGo(ctx context.Context, m Manifest) error {
	for _, g := range m.Go {
		if _, err := i.Exec.Run(ctx, procexec.Command{Name: GoBinary, Args: []string{"install", g}, Env: []string{gobinEnv + i.Dir}, Dir: i.Dir}); err != nil {
			return fmt.Errorf("go install %s: %w: %s", g, err, procexec.Output(err))
		}
		i.Log.Info("tool installed", "tool", g)
	}
	return nil
}

func (i Installer) buildSources(ctx context.Context, m Manifest) error {
	if sources := m.jsonfilterSources(); len(sources) > 0 {
		if err := i.buildJSONFilter(ctx, sources); err != nil {
			return fmt.Errorf("%s: %w", JSONFilter, err)
		}
		i.Log.Info("tool built", "tool", JSONFilter, "sources", len(sources))
	}
	if s, ok := m.busybox(); ok {
		if err := i.buildBusybox(ctx, s, filepath.Join(m.dir, filepath.FromSlash(s.Config))); err != nil {
			return fmt.Errorf("%s: %w", sourceBusybox, err)
		}
		i.Log.Info("tool built", "tool", sourceBusybox, "sha256", s.SHA256)
	}
	return nil
}

func (i Installer) verified(ctx context.Context, url, want string) ([]byte, error) {
	if data, dir, ok := i.cached(want); ok {
		i.Log.Info("tool taken from the cache", "dir", dir, "sha256", want)
		return data, nil
	}
	data, err := i.downloaded(ctx, url, want)
	if err != nil {
		return nil, err
	}
	i.keep(want, data)
	return data, nil
}

func (i Installer) downloaded(ctx context.Context, url, want string) ([]byte, error) {
	data, err := i.checked(ctx, url, want)
	for try := 2; err != nil && try <= downloadTries; try++ {
		i.Log.Warn("tool download failed, trying again", "url", url, "try", try, "error", err.Error())
		if err = pause(ctx, i.Retry); err == nil {
			data, err = i.checked(ctx, url, want)
		}
	}
	return data, err
}

func (i Installer) checked(ctx context.Context, url, want string) ([]byte, error) {
	data, err := i.download(ctx, url)
	if err != nil {
		return nil, err
	}
	if got := digestOf(data); got != want {
		return nil, fmt.Errorf("%w: got %s", ErrChecksum, got)
	}
	return data, nil
}

func (i Installer) cached(want string) ([]byte, string, bool) {
	for _, dir := range []string{i.Shared, i.Cache} {
		data, err := readCached(dir, want)
		if err == nil {
			return data, dir, true
		}
		if errors.Is(err, ErrChecksum) {
			i.Log.Warn("cached tool ignored", "dir", dir, "sha256", want, "error", err.Error())
		}
	}
	return nil, "", false
}

func readCached(dir, want string) ([]byte, error) {
	if dir == "" {
		return nil, os.ErrNotExist
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(want)
	if err != nil {
		return nil, err
	}
	if got := digestOf(data); got != want {
		return nil, fmt.Errorf("%w: got %s", ErrChecksum, got)
	}
	return data, nil
}

func (i Installer) keep(want string, data []byte) {
	if i.Cache == "" {
		return
	}
	if err := store(i.Cache, want, data, cachePerm); err != nil {
		i.Log.Warn("tool cache not written", "dir", i.Cache, "error", err.Error())
	}
}

func store(dir, want string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, stagePattern)
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, want))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (i Installer) fetch(ctx context.Context, b Binary) error {
	data, err := i.verified(ctx, b.URL, b.SHA256)
	if err != nil {
		return err
	}
	if b.Tree != "" {
		return i.unpack(ctx, b, data)
	}
	if b.Member != "" {
		if data, err = extract(data, b.Member); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(i.Dir, b.Name), data, binPerm)
}

func (i Installer) download(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := i.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= firstBadStatus {
		return nil, fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload))
}

func extract(archive []byte, member string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: %s", ErrMember, member)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == member && hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

func (i Installer) unpack(ctx context.Context, b Binary, data []byte) error {
	stage, err := os.MkdirTemp(i.Dir, stagePattern)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	root, err := i.untar(ctx, stage, data)
	if err != nil {
		return err
	}
	return i.place(b, filepath.Join(root, filepath.FromSlash(b.Tree)))
}

func (i Installer) untar(ctx context.Context, stage string, data []byte) (string, error) {
	archive := filepath.Join(stage, archiveFile)
	root := filepath.Join(stage, rootDir)
	if err := os.WriteFile(archive, data, binPerm); err != nil {
		return "", err
	}
	if err := os.Mkdir(root, dirPerm); err != nil {
		return "", err
	}
	if _, err := i.Exec.Run(ctx, procexec.Command{Name: TarBinary, Args: []string{"-xf", archive, dirFlag, root}, Dir: stage}); err != nil {
		return "", fmt.Errorf("unpack: %w: %s", err, procexec.Output(err))
	}
	return root, nil
}

func (i Installer) place(b Binary, tree string) error {
	dest := filepath.Join(i.Dir, treesDir, b.Name)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), dirPerm); err != nil {
		return err
	}
	if err := os.Rename(tree, dest); err != nil {
		return err
	}
	for _, l := range b.Links {
		if err := i.link(b.Name, l); err != nil {
			return err
		}
	}
	return nil
}

func (i Installer) link(tree, target string) error {
	rel := filepath.Join(treesDir, tree, filepath.FromSlash(target))
	info, err := os.Stat(filepath.Join(i.Dir, rel))
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrMember, target)
	}
	at := filepath.Join(i.Dir, path.Base(target))
	if err := os.RemoveAll(at); err != nil {
		return err
	}
	return os.Symlink(rel, at)
}
