package lock

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

const filePerm = 0o600

var ErrTimeout = errors.New("lock wait timed out")

type File struct {
	Path  string
	Wait  time.Duration
	Poll  time.Duration
	Sleep func(ctx context.Context, d time.Duration) error
}

func (l File) Acquire(ctx context.Context, onWait func()) (func(), error) {
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, err
	}
	release := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	if tryLock(f) {
		return release, nil
	}
	onWait()
	for waited := time.Duration(0); waited < l.Wait; waited += l.Poll {
		if err := l.Sleep(ctx, l.Poll); err != nil {
			_ = f.Close()
			return nil, err
		}
		if tryLock(f) {
			return release, nil
		}
	}
	_ = f.Close()
	return nil, ErrTimeout
}

func tryLock(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}
