package mutation

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
)

const (
	testdataDir   = "testdata"
	currentDir    = "."
	logGetenv     = "getenv"
	logOpen       = "open"
	logStat       = "stat"
	logChdir      = "chdir"
	digestAbsent  = "absent"
	digestDir     = "dir"
	digestStd     = "std"
	digestModule  = "module"
	digestFiles   = "files"
	fieldSep      = "\x00"
	parentDirElem = ".."
	digestData    = "data"
	goExt         = ".go"
)

var ErrClosure = errors.New("cannot hash the build closure")

var setupMarkers = []string{"func init(", "func TestMain("}

type KeyBase struct {
	Engine    string `json:"engine"`
	Policy    string `json:"policy"`
	Toolchain string `json:"toolchain"`
	Module    string `json:"module"`
	Flags     string `json:"flags"`
	Host      string `json:"host"`
}

func (k KeyBase) Digest() string {
	data, _ := json.Marshal(k)
	return hashOf(string(data))
}

type digests struct {
	root     string
	base     string
	built    golang.Closure
	reads    map[string]string
	volatile map[string]bool
	linked   map[string][]string
	pkgs     map[string]string
	initCode map[string]bool
	tested   map[string]string
	files    map[string]string
	data     string
}

func newDigests(root, base string, built golang.Closure, reads map[string]string) *digests {
	d := &digests{root: root, base: base, built: built, reads: reads, volatile: map[string]bool{}, linked: map[string][]string{},
		pkgs: map[string]string{}, initCode: map[string]bool{}, tested: map[string]string{}, files: map[string]string{}}
	for test, deps := range built.Tests {
		for _, dep := range deps {
			d.linked[dep] = append(d.linked[dep], test)
		}
	}
	for _, tests := range d.linked {
		slices.Sort(tests)
	}
	return d
}

func (d *digests) linkers(pkg string) []string {
	return d.linked[pkg]
}

func (d *digests) key(t target) (string, error) {
	parts := []string{d.base, mutantIdentity(d.root, t.mutant)}
	for _, p := range d.linkers(t.pkg.ImportPath) {
		if d.volatile[p] {
			return "", nil
		}
		td, err := d.testBinary(p)
		if err != nil {
			return "", err
		}
		parts = append(parts, p, td)
	}
	return hashOf(parts...), nil
}

func mutantIdentity(root string, m Mutant) string {
	rel, _ := filepath.Rel(root, m.File)
	edits, _ := json.Marshal(m.edits)
	return hashOf(filepath.ToSlash(rel), fmt.Sprint(m.Line), m.Operator, m.Original, m.Mutated, string(edits))
}

func (d *digests) testBinary(pkg string) (string, error) {
	if v, ok := d.tested[pkg]; ok {
		return v, nil
	}
	parts := []string{d.reads[pkg]}
	setup := false
	for _, dep := range d.built.Tests[pkg] {
		v, err := d.pkg(dep)
		if err != nil {
			return "", err
		}
		parts = append(parts, dep, v)
		setup = setup || d.initCode[dep]
	}
	if setup {
		parts = append(parts, d.moduleData())
	}
	d.tested[pkg] = hashOf(parts...)
	return d.tested[pkg], nil
}

func (d *digests) moduleData() string {
	if d.data != "" {
		return d.data
	}
	parts := []string{digestData}
	_ = filepath.WalkDir(d.root, func(path string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() && filepath.Ext(path) != goExt {
			parts = append(parts, d.rel(path), d.content(path))
		}
		return nil
	})
	d.data = hashOf(parts...)
	return d.data
}

func (d *digests) pkg(path string) (string, error) {
	if v, ok := d.pkgs[path]; ok {
		return v, nil
	}
	b, ok := d.built.Packages[path]
	if !ok {
		return "", fmt.Errorf("%w: package %s is missing from go list -deps -test", ErrClosure, path)
	}
	v, err := d.pkgDigest(b)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrClosure, path, err)
	}
	d.pkgs[path] = v
	return v, nil
}

func (d *digests) pkgDigest(b golang.Built) (string, error) {
	if b.Standard {
		return hashOf(digestStd, b.ImportPath), nil
	}
	if ver, ok := b.Versioned(); ok && !d.inside(b.Dir) {
		return hashOf(digestModule, b.ImportPath, ver), nil
	}
	root, err := os.OpenRoot(b.Dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	files, err := packageFiles(root, b)
	if err != nil {
		return "", err
	}
	parts := []string{digestFiles, b.ImportPath}
	for _, f := range files {
		parts = append(parts, d.rel(filepath.Join(b.Dir, f)), rootContent(root, f))
		d.initCode[b.ImportPath] = d.initCode[b.ImportPath] || setupCode(root, f)
	}
	return hashOf(parts...), nil
}

func packageFiles(root *os.Root, b golang.Built) ([]string, error) {
	out := b.Files()
	entries, err := fs.ReadDir(root.FS(), currentDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	err = fs.WalkDir(root.FS(), testdataDir, func(path string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			out = append(out, path)
		}
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	slices.Sort(out)
	return slices.Compact(out), err
}

func (d *digests) inside(path string) bool {
	rel, err := filepath.Rel(d.root, path)
	return err == nil && rel != parentDirElem && !strings.HasPrefix(rel, parentDirElem+string(filepath.Separator))
}

func (d *digests) rel(path string) string {
	rel, err := filepath.Rel(d.root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func (d *digests) content(path string) string {
	if v, ok := d.files[path]; ok {
		return v
	}
	root, err := os.OpenRoot(d.root)
	if err != nil || !d.inside(path) {
		return digestAbsent
	}
	defer func() { _ = root.Close() }()
	d.files[path] = rootContent(root, d.rel(path))
	return d.files[path]
}

func rootContent(root *os.Root, rel string) string {
	info, err := root.Stat(rel)
	switch {
	case err != nil:
		return digestAbsent
	case info.IsDir():
		return dirDigest(root, rel)
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return digestAbsent
	}
	return hashOf(string(data))
}

func dirDigest(root *os.Root, rel string) string {
	entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(rel))
	if err != nil {
		return digestAbsent
	}
	names := make([]string, 0, len(entries)+1)
	names = append(names, digestDir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return hashOf(names...)
}

type testLog struct {
	d       *digests
	lookup  func(string) (string, bool)
	cwd     string
	records []string
}

func (d *digests) readsDigest(pkgDir, log string, lookup func(string) (string, bool)) string {
	l := &testLog{d: d, lookup: lookup, cwd: pkgDir}
	sc := bufio.NewScanner(strings.NewReader(log))
	for sc.Scan() {
		op, arg, ok := strings.Cut(sc.Text(), " ")
		if ok {
			l.record(op, arg)
		}
	}
	slices.Sort(l.records)
	return hashOf(slices.Compact(l.records)...)
}

func (l *testLog) record(op, arg string) {
	switch op {
	case logGetenv:
		value, set := l.lookup(arg)
		l.records = append(l.records, strings.Join([]string{op, arg, fmt.Sprint(set), value}, fieldSep))
	case logOpen, logStat:
		l.file(l.abs(arg))
	case logChdir:
		l.cwd = l.abs(arg)
	}
}

func (l *testLog) abs(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(l.cwd, path)
}

func (l *testLog) file(path string) {
	if l.d.inside(path) {
		l.records = append(l.records, l.d.rel(path)+fieldSep+l.d.content(path))
	}
}

func setupCode(root *os.Root, rel string) bool {
	if filepath.Ext(rel) != goExt {
		return false
	}
	data, err := root.ReadFile(rel)
	return err == nil && slices.ContainsFunc(setupMarkers, func(m string) bool { return strings.Contains(string(data), m) })
}

func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte(fieldSep))
	}
	return hex.EncodeToString(h.Sum(nil))
}
