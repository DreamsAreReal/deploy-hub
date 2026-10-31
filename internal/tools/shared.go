package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// SharedCache holds every archive of the released manifest under its sha256.
// Only the root side of the hub writes it; CI users read it.
const (
	SharedCache   = "/var/cache/deploy-hub/tools"
	sharedPerm    = 0o644
	sharedDirPerm = 0o755
)

type download struct {
	name, url, sha256 string
}

func (m Manifest) archives() []download {
	all := make([]download, 0, len(m.Binaries)+len(m.Sources))
	for _, b := range m.Binaries {
		all = append(all, download{b.Name, b.URL, b.SHA256})
	}
	for _, s := range m.Sources {
		all = append(all, download{s.Name, s.URL, s.SHA256})
	}
	return all
}

// Fill downloads the archives missing from dir, verifies each against its
// sha256 and stores it world-readable under that sha256. A file in dir whose
// content does not match its name is replaced.
func (i Installer) Fill(ctx context.Context, m Manifest, dir string) (int, error) {
	if err := os.MkdirAll(dir, sharedDirPerm); err != nil {
		return 0, err
	}
	if err := os.Chmod(dir, sharedDirPerm); err != nil {
		return 0, err
	}
	added := 0
	var errs []error
	for _, a := range m.archives() {
		if _, err := readCached(dir, a.sha256); err == nil {
			continue
		}
		data, err := i.downloaded(ctx, a.url, a.sha256)
		if err == nil {
			err = store(dir, a.sha256, data, sharedPerm)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.name, err))
			continue
		}
		added++
		i.Log.Info("tool cached", "tool", a.name, "dir", dir, "sha256", a.sha256)
	}
	return added, errors.Join(errs...)
}
