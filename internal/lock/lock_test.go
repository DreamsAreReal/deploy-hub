package lock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func TestAcquireReleaseReacquire(t *testing.T) {
	l := File{Path: filepath.Join(t.TempDir(), "deploy.lock"), Wait: time.Second, Poll: time.Millisecond, Sleep: noSleep}
	waited := false
	release, err := l.Acquire(context.Background(), func() { waited = true })
	if err != nil || waited {
		t.Fatalf("first acquire: %v waited=%v", err, waited)
	}
	release()
	release2, err := l.Acquire(context.Background(), func() {})
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	release2()
}

func TestAcquireTimesOutWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.lock")
	holder := File{Path: path, Wait: 0, Poll: time.Millisecond, Sleep: noSleep}
	release, err := holder.Acquire(context.Background(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	waited := false
	waiter := File{Path: path, Wait: 3 * time.Millisecond, Poll: time.Millisecond, Sleep: noSleep}
	if _, err := waiter.Acquire(context.Background(), func() { waited = true }); !errors.Is(err, ErrTimeout) || !waited {
		t.Fatalf("want timeout after announcing wait, got %v waited=%v", err, waited)
	}
}

func TestAcquireSucceedsAfterHolderReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.lock")
	release, err := File{Path: path, Sleep: noSleep}.Acquire(context.Background(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	sleep := func(ctx context.Context, _ time.Duration) error {
		release()
		return ctx.Err()
	}
	r2, err := File{Path: path, Wait: time.Second, Poll: time.Millisecond, Sleep: sleep}.Acquire(context.Background(), func() {})
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	r2()
}

func TestAcquireStopsOnCancelledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.lock")
	release, err := File{Path: path, Sleep: noSleep}.Acquire(context.Background(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (File{Path: path, Wait: time.Second, Poll: time.Millisecond, Sleep: noSleep}).Acquire(ctx, func() {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancel, got %v", err)
	}
}

func TestAcquireFailsOnBadPath(t *testing.T) {
	if _, err := (File{Path: filepath.Join(t.TempDir(), "missing", "x.lock")}).Acquire(context.Background(), func() {}); err == nil {
		t.Fatal("expected open error")
	}
}
