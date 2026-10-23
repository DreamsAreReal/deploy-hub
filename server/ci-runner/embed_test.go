package cirunner

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hostFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestEmbedsEveryHostFileOfTheDirectory(t *testing.T) {
	names := hostFiles(t)
	embedded, err := fs.ReadDir(Files(), ".")
	if err != nil || len(embedded) != len(names) {
		t.Fatalf("embedded %d files, the directory holds %d: %v", len(embedded), len(names), err)
	}
	for _, name := range names {
		disk, rerr := os.ReadFile(filepath.Clean(name))
		got, ferr := fs.ReadFile(Files(), name)
		if rerr != nil || ferr != nil || !bytes.Equal(got, disk) {
			t.Fatalf("%s is not embedded as on disk: %v %v", name, rerr, ferr)
		}
	}
}
