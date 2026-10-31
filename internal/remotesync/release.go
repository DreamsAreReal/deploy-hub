package remotesync

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/gatedimage"
)

const (
	MaxFileSize     = 4 << 20
	InstalledEngine = "/usr/bin/routerbot-apply"
	ReleaseRoot     = "/tmp/dh-release/"
	manifestFile    = "MANIFEST"
	restartFile     = "RESTART"
	rootDir         = "root"
	dirMode         = 0o755
	fileMode        = 0o644
)

var ErrTooLarge = errors.New("file is larger than the remote-sync limit")

type entry struct {
	name string
	mode int64
	data []byte
}

func (s Syncer) shipment(ctx context.Context, j job, bundle string) (Shipment, error) {
	src := s.source()
	ref, err := src.Pinned(ctx, j.image, j.tag)
	if err != nil {
		return Shipment{}, err
	}
	if err = src.Extract(ctx, ref, policy.RemoteBundleDir, bundle); err != nil {
		return Shipment{}, err
	}
	srcs := make([]string, 0, len(j.spec.Files))
	for _, f := range j.spec.Files {
		srcs = append(srcs, f.Src)
	}
	if err = gatedimage.CheckBundle(bundle, srcs); err != nil {
		return Shipment{}, err
	}
	entries, err := releaseEntries(bundle, j.spec)
	if err != nil {
		return Shipment{}, err
	}
	archive, err := pack(entries)
	sh := Shipment{Target: j.spec.Target, Sha7: j.sha7, Engine: EngineOnTarget, Archive: archive}
	if slices.ContainsFunc(j.spec.Files, func(f policy.RemoteFile) bool { return f.Dst == InstalledEngine }) {
		sh.Engine = EngineBundled
	}
	return sh, err
}

func releaseEntries(bundle string, spec policy.Remote) ([]entry, error) {
	var manifest strings.Builder
	out := []entry{}
	for _, f := range spec.Files {
		data, err := readLimited(filepath.Join(bundle, filepath.FromSlash(f.Src)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Src, err)
		}
		mode, err := policy.ParseMode(f.Mode)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&manifest, "%s %s %s\n", hex.EncodeToString(sum[:]), f.Mode, f.Dst)
		out = append(out, entry{name: rootDir + f.Dst, mode: int64(mode), data: data})
	}
	restart := strings.Join(spec.Restart, "\n")
	if restart != "" {
		restart += "\n"
	}
	return append(out, entry{name: manifestFile, mode: fileMode, data: []byte(manifest.String())},
		entry{name: restartFile, mode: fileMode, data: []byte(restart)}), nil
}

func readLimited(file string) ([]byte, error) {
	f, err := os.Open(filepath.Clean(file))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err == nil && len(data) > MaxFileSize {
		err = fmt.Errorf("%w (%d bytes)", ErrTooLarge, MaxFileSize)
	}
	return data, err
}

func pack(entries []entry) ([]byte, error) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for _, e := range entries {
		if err := writeEntry(w, e, dirs); err != nil {
			return nil, err
		}
	}
	err := w.Close()
	return buf.Bytes(), err
}

func writeEntry(w *tar.Writer, e entry, dirs map[string]bool) error {
	for _, d := range parents(e.name) {
		if dirs[d] {
			continue
		}
		dirs[d] = true
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: dirMode}); err != nil {
			return err
		}
	}
	if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: e.name, Mode: e.mode, Size: int64(len(e.data))}); err != nil {
		return err
	}
	_, err := w.Write(e.data)
	return err
}

func parents(name string) []string {
	var out []string
	for d := path.Dir(name); d != "."; d = path.Dir(d) {
		out = append(out, d)
	}
	slices.Reverse(out)
	return out
}
