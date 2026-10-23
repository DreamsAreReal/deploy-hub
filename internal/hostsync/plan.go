package hostsync

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const MaxFileSize = 1 << 20

var (
	ErrTooLarge    = errors.New("file is larger than the host-sync limit")
	ErrPlaceholder = errors.New("template placeholder has no safe value")
	ErrParent      = errors.New("destination directory must be a real directory")

	placeholder = regexp.MustCompile(`@([A-Z][A-Z0-9_]{0,40})@`)
	safeValue   = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,256}$`)
)

type snapshot struct {
	content []byte
	mode    fs.FileMode
	uid     int
	gid     int
}

type plannedFile struct {
	spec    policy.HostFile
	path    string
	want    snapshot
	old     snapshot
	existed bool
	changed bool
}

func (s Syncer) hostPath(p string) string {
	return filepath.Join(s.Root, p)
}

func (s Syncer) plan(t target, bundle string) ([]plannedFile, error) {
	if err := checkBundle(bundle, t.spec); err != nil {
		return nil, err
	}
	out := make([]plannedFile, 0, len(t.spec.Files))
	for _, f := range t.spec.Files {
		p, err := s.planFile(t, bundle, f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Dst, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func (s Syncer) planFile(t target, bundle string, f policy.HostFile) (plannedFile, error) {
	content, err := readLimited(filepath.Join(bundle, filepath.FromSlash(f.Src)))
	if err == nil && f.Env != "" {
		content, err = render(content, filepath.Join(t.dir, f.Env))
	}
	if err != nil {
		return plannedFile{}, err
	}
	mode, _ := policy.ParseMode(f.Mode)
	uid, gid, err := s.Accounts(f.Owner, f.Group)
	if err != nil {
		return plannedFile{}, err
	}
	p := plannedFile{spec: f, path: s.hostPath(f.Dst), want: snapshot{content: content, mode: fs.FileMode(mode), uid: uid, gid: gid}}
	if derr := realDir(filepath.Dir(p.path)); derr != nil {
		return plannedFile{}, derr
	}
	p.old, p.existed, err = current(p.path)
	p.changed = !p.existed || !p.old.same(p.want)
	return p, err
}

func (a *snapshot) same(b snapshot) bool {
	return bytes.Equal(a.content, b.content) && a.mode == b.mode && a.uid == b.uid && a.gid == b.gid
}

func readLimited(path string) ([]byte, error) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err == nil && len(data) > MaxFileSize {
		err = fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	return data, err
}

func render(content []byte, envPath string) ([]byte, error) {
	raw, err := readLimited(envPath)
	if err != nil {
		return nil, err
	}
	values := registry.ParseKV(string(raw))
	var missing error
	out := placeholder.ReplaceAllFunc(content, func(m []byte) []byte {
		key := string(m[1 : len(m)-1])
		v, ok := values[key]
		if !ok || !safeValue.MatchString(v) {
			missing = fmt.Errorf("%w: %s", ErrPlaceholder, key)
			return m
		}
		return []byte(v)
	})
	return out, missing
}

func realDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || !info.IsDir() || resolved != dir {
		return fmt.Errorf("%w: %s", ErrParent, dir)
	}
	return nil
}

func current(path string) (snapshot, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return snapshot{}, false, nil
	}
	if err != nil {
		return snapshot{}, false, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return snapshot{}, false, fmt.Errorf("%w: %s", ErrSymlink, path)
	}
	if !info.Mode().IsRegular() {
		return snapshot{}, false, fmt.Errorf("%w: %s", ErrNotRegular, path)
	}
	content, err := readLimited(path)
	if err != nil {
		return snapshot{}, false, err
	}
	st, _ := info.Sys().(*syscall.Stat_t)
	return snapshot{content: content, mode: info.Mode().Perm(), uid: int(st.Uid), gid: int(st.Gid)}, true, nil
}
