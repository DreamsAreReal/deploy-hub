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
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	GoBinary        = "go"
	downloadTimeout = 5 * time.Minute
	maxDownload     = 512 << 20
	binPerm         = 0o700
	dirPerm         = 0o700
	firstBadStatus  = 300
	gobinEnv        = "GOBIN="
	parentDir       = ".."
)

var (
	ErrManifest = errors.New("invalid tools manifest")
	ErrChecksum = errors.New("checksum mismatch")
	ErrMember   = errors.New("archive member not found")
	toolName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	releaseURL  = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/releases/download/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	sha256Hex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	memberPath  = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	goTool      = regexp.MustCompile(`^[a-z0-9.-]+(/[A-Za-z0-9_.-]+)+@v[0-9]+\.[0-9]+\.[0-9]+$`)
)

type Binary struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	Member string `yaml:"member"`
}

type Manifest struct {
	Binaries []Binary `yaml:"binaries"`
	Go       []string `yaml:"go"`
}

func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifest, err)
	}
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
	return errors.Join(errs...)
}

func (b Binary) validate() error {
	switch {
	case !toolName.MatchString(b.Name):
		return fmt.Errorf("%w: name %q", ErrManifest, b.Name)
	case !releaseURL.MatchString(b.URL):
		return fmt.Errorf("%w: %s: url must be a GitHub release asset", ErrManifest, b.Name)
	case !sha256Hex.MatchString(b.SHA256):
		return fmt.Errorf("%w: %s: sha256 must be 64 hex characters", ErrManifest, b.Name)
	case b.Member != "" && (!memberPath.MatchString(b.Member) || path.Clean(b.Member) != b.Member || strings.HasPrefix(b.Member, parentDir)):
		return fmt.Errorf("%w: %s: bad archive member", ErrManifest, b.Name)
	default:
		return nil
	}
}

type Installer struct {
	Client *http.Client
	Dir    string
	Exec   procexec.Runner
	Log    *slog.Logger
}

func (i Installer) Install(ctx context.Context, m Manifest) error {
	if err := os.MkdirAll(i.Dir, dirPerm); err != nil {
		return err
	}
	for _, b := range m.Binaries {
		if err := i.fetch(ctx, b); err != nil {
			return fmt.Errorf("%s: %w", b.Name, err)
		}
		i.Log.Info("tool installed", "tool", b.Name, "sha256", b.SHA256)
	}
	for _, g := range m.Go {
		if _, err := i.Exec.Run(ctx, procexec.Command{Name: GoBinary, Args: []string{"install", g}, Env: []string{gobinEnv + i.Dir}, Dir: i.Dir}); err != nil {
			return fmt.Errorf("go install %s: %w: %s", g, err, procexec.Output(err))
		}
		i.Log.Info("tool installed", "tool", g)
	}
	return nil
}

func (i Installer) fetch(ctx context.Context, b Binary) error {
	data, err := i.download(ctx, b.URL)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != b.SHA256 {
		return fmt.Errorf("%w: got %s", ErrChecksum, got)
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
