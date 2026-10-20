package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	dirPerm  = 0o750
	filePerm = 0o600
)

func Write(t testing.TB, path, content string) {
	t.Helper()
	Mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), filePerm); err != nil {
		t.Fatal(err)
	}
}

func Mkdir(t testing.TB, path string) {
	t.Helper()
	if err := os.MkdirAll(path, dirPerm); err != nil {
		t.Fatal(err)
	}
}

func Read(t testing.TB, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func Missing(t testing.TB, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s must not exist (stat err: %v)", path, err)
	}
}

func Perm(t testing.TB, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s: mode %v, want %v", path, info.Mode().Perm(), want)
	}
}

func Contains(t testing.TB, got string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(got, p) {
			t.Fatalf("missing %q in:\n%s", p, got)
		}
	}
}

func Excludes(t testing.TB, got string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(got, p) {
			t.Fatalf("unexpected %q in:\n%s", p, got)
		}
	}
}

func Equal(t testing.TB, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func NoErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func Fails(t testing.TB, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
}

func Is(t testing.TB, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error %v, want %v", err, target)
	}
}

func True(t testing.TB, cond bool, msg string) {
	t.Helper()
	if !cond {
		t.Fatal(msg)
	}
}
