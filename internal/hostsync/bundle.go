package hostsync

import (
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/gatedimage"
)

var (
	ErrSymlink    = gatedimage.ErrSymlink
	ErrNotRegular = gatedimage.ErrNotRegular
	ErrUnlisted   = gatedimage.ErrUnlisted
	ErrMissing    = gatedimage.ErrMissing
)

func checkBundle(bundle string, spec policy.Host) error {
	srcs := make([]string, 0, len(spec.Files))
	for _, f := range spec.Files {
		srcs = append(srcs, f.Src)
	}
	return gatedimage.CheckBundle(bundle, srcs)
}
