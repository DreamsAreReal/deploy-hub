package crawler

import (
	"io/fs"
	"regexp"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var relativeImport = regexp.MustCompile(`from '\./([a-z]+\.mjs)'`)

type brokenFS struct{}

func (brokenFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }

func TestModulesHoldTheEntryAndEveryScriptItImports(t *testing.T) {
	modules, err := Modules()
	testutil.NoErr(t, err)
	names := make([]string, 0, len(modules))
	for _, module := range modules {
		names = append(names, module.Name)
	}
	testutil.True(t, slices.Contains(names, File) && slices.IsSorted(names), "the entry is embedded and the modules come in name order")
	for _, module := range modules {
		testutil.True(t, len(module.Body) > 0, module.Name+" is empty")
		for _, imported := range relativeImport.FindAllStringSubmatch(string(module.Body), -1) {
			testutil.True(t, slices.Contains(names, imported[1]), module.Name+" imports "+imported[1]+", which is not embedded")
		}
	}
}

func TestReadKeepsOnlyScriptsAndFailsOnAnUnreadableOne(t *testing.T) {
	modules, err := read(fstest.MapFS{"b.mjs": {Data: []byte("b")}, "a.mjs": {Data: []byte("a")}, "package.json": {Data: []byte("{}")}})
	testutil.NoErr(t, err)
	testutil.Equal(t, modules, []Module{{Name: "a.mjs", Body: []byte("a")}, {Name: "b.mjs", Body: []byte("b")}})
	for _, src := range []fs.FS{fstest.MapFS{"dir.mjs": {Mode: fs.ModeDir}}, brokenFS{}} {
		_, err := read(src)
		testutil.Fails(t, err)
	}
}
