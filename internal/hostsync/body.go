package hostsync

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
)

const (
	MaxFileSize   = 1 << 20
	MaxBinarySize = 64 << 20
	execBits      = 0o111
)

var ErrChanged = errors.New("file changed after it was checked")

type body struct {
	data  []byte
	dir   string
	path  string
	limit int64
	sum   [sha256.Size]byte
}

func bytesBody(data []byte) body {
	return body{data: data, sum: sha256.Sum256(data)}
}

func streamed(f policy.HostFile) bool {
	mode, _ := policy.ParseMode(f.Mode)
	return f.Env == "" && mode&execBits != 0
}

func readBody(path string, f policy.HostFile) (body, error) {
	return readFrom(body{path: path}, f)
}

func readFrom(at body, f policy.HostFile) (body, error) {
	if !streamed(f) {
		var buf bytes.Buffer
		at.limit = MaxFileSize
		err := at.stream(&buf)
		return bytesBody(buf.Bytes()), err
	}
	b := body{dir: at.dir, path: at.path, limit: MaxBinarySize}
	h := sha256.New()
	err := b.stream(h)
	h.Sum(b.sum[:0])
	return b, err
}

func readLimited(path string, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	err := (body{path: path, limit: limit}).stream(&buf)
	return buf.Bytes(), err
}

func (b body) open() (*os.File, error) {
	if b.dir == "" {
		return os.OpenFile(filepath.Clean(b.path), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	}
	r, err := openParent(b.dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return r.OpenFile(b.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func (b body) stream(w io.Writer) error {
	f, err := b.open()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(w, io.LimitReader(f, b.limit+1))
	if err == nil && n > b.limit {
		err = fmt.Errorf("%w: %s", ErrTooLarge, b.path)
	}
	return err
}

func (b body) copyTo(w io.Writer) error {
	if b.path == "" {
		_, err := w.Write(b.data)
		return err
	}
	h := sha256.New()
	if err := b.stream(io.MultiWriter(w, h)); err != nil {
		return err
	}
	return b.verify(h)
}

func (b body) verify(h hash.Hash) error {
	if !bytes.Equal(h.Sum(nil), b.sum[:]) {
		return fmt.Errorf("%w: %s", ErrChanged, b.path)
	}
	return nil
}

func writeBody(path string, b body) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, tempPerm)
	if err != nil {
		return err
	}
	return errors.Join(b.copyTo(f), f.Sync(), f.Close())
}

func (s Syncer) writeIn(r *os.Root, name string, want snapshot) error {
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, tempPerm)
	if err != nil {
		return err
	}
	return errors.Join(want.body.copyTo(f), s.Chown(f, want.uid, want.gid), f.Chmod(want.mode), f.Sync(), f.Close())
}
