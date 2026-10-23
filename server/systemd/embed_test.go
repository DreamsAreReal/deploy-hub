package systemd

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestEmbedsEveryUnitOfTheDirectoryAndNothingElse(t *testing.T) {
	units, err := filepath.Glob("deploy-hub-*.*")
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := fs.Glob(Files(), "*")
	if err != nil || !slices.Equal(embedded, units) {
		t.Fatalf("embedded %v, the directory holds the units %v: %v", embedded, units, err)
	}
	for _, name := range units {
		disk, _ := os.ReadFile(filepath.Clean(name))
		got, _ := fs.ReadFile(Files(), name)
		if string(got) != string(disk) {
			t.Fatalf("%s is not embedded as on disk", name)
		}
	}
}
