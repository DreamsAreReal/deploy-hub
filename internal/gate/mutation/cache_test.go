package mutation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestWorkerCachesShareTheBaseAndDropMutantBuilds(t *testing.T) {
	base := filepath.Join(t.TempDir(), cacheDir)
	testutil.Write(t, filepath.Join(base, "ab", "ab12-a"), "action")
	testutil.Write(t, filepath.Join(base, "ab", "ab34-d"), "output")
	testutil.Write(t, filepath.Join(base, "trim.txt"), "1")
	testutil.Write(t, filepath.Join(base, "ab", "partial"), "1")
	e := &engine{cacheDir: base}
	caches, err := e.workerCaches(2)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(caches), 2)
	w := caches[1]
	testutil.Equal(t, w.dir, base+"-1")
	linked, err := os.Stat(filepath.Join(w.dir, "ab", "ab12-a"))
	testutil.NoErr(t, err)
	orig, err := os.Stat(filepath.Join(base, "ab", "ab12-a"))
	testutil.NoErr(t, err)
	testutil.True(t, os.SameFile(linked, orig), "cache entries are hard links to the base")
	testutil.Missing(t, filepath.Join(w.dir, "trim.txt"))
	testutil.Missing(t, filepath.Join(w.dir, "ab", "partial"))
	testutil.Write(t, filepath.Join(w.dir, "trim.txt"), "2")
	testutil.Write(t, filepath.Join(w.dir, "cd", "cd56-d"), "mutant build")
	testutil.Write(t, filepath.Join(w.dir, "ab", "ab78-a"), "mutant action")
	testutil.NoErr(t, w.trim())
	testutil.Missing(t, filepath.Join(w.dir, "cd", "cd56-d"))
	testutil.Missing(t, filepath.Join(w.dir, "ab", "ab78-a"))
	testutil.Missing(t, filepath.Join(w.dir, "trim.txt"))
	testutil.Equal(t, testutil.Read(t, filepath.Join(w.dir, "ab", "ab34-d")), "output")
	testutil.Equal(t, testutil.Read(t, filepath.Join(base, "ab", "ab12-a")), "action")
	e.dir = t.TempDir()
	e.dropWorkerCaches()
	testutil.Missing(t, w.dir)
}

func TestWorkerCacheFailures(t *testing.T) {
	missing := &engine{cacheDir: filepath.Join(t.TempDir(), "none")}
	_, err := missing.workerCaches(1)
	testutil.Fails(t, err)
	base := filepath.Join(t.TempDir(), cacheDir)
	testutil.Write(t, filepath.Join(base, "ab", "ab12-a"), "action")
	testutil.Write(t, base+"-0", "file in the way")
	_, err = (&engine{cacheDir: base}).workerCaches(1)
	testutil.Fails(t, err)
	testutil.NoErr(t, os.Remove(base+"-0"))
	testutil.Write(t, filepath.Join(base+"-0", "ab"), "file in the way")
	_, err = (&engine{cacheDir: base}).workerCaches(1)
	testutil.Fails(t, err)
	testutil.NoErr(t, os.Remove(filepath.Join(base+"-0", "ab")))
	vanished := &workerCache{dir: base + "-0", base: map[string]bool{"zz/zz-a": true}}
	testutil.Fails(t, vanished.seed(base))
	gone := &workerCache{dir: filepath.Join(t.TempDir(), "gone")}
	testutil.Fails(t, gone.trim())
}
