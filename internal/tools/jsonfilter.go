package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	CmakeBinary    = "cmake"
	CCBinary       = "cc"
	LemonBinary    = "jsonpath-lemon"
	JSONFilter     = "jsonfilter"
	sourceJSONC    = "json-c"
	sourceLibubox  = "libubox"
	sourceJSONPath = "jsonpath"
	buildPattern   = ".build-*"
	headerExt      = ".h"
	prefixDir      = "prefix"
	jsoncBuildDir  = "json-c-build"
	outFlag        = "-o"
)

var (
	jsonfilterSources = []string{sourceJSONC, sourceLibubox, sourceJSONPath}
	jsoncFlags        = []string{
		"-DCMAKE_INSTALL_LIBDIR=lib", "-DCMAKE_POLICY_VERSION_MINIMUM=3.5", "-DCMAKE_BUILD_TYPE=Release",
		"-DBUILD_SHARED_LIBS=OFF", "-DBUILD_STATIC_LIBS=ON", "-DBUILD_TESTING=OFF", "-DBUILD_APPS=OFF", "-DDISABLE_WERROR=ON",
	}
	jsonpathFiles = []string{"main.c", "ast.c", "lexer.c", "parser.c", "matcher.c"}
	libuboxFiles  = []string{"avl.c", "avl-cmp.c", "blob.c", "blobmsg.c", "utils.c", "safe_list.c", "vlist.c", "kvlist.c"}
)

type Source struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	Tree   string `yaml:"tree"`
}

func (s Source) validate() error {
	switch {
	case !slices.Contains(jsonfilterSources, s.Name):
		return fmt.Errorf("%w: source %q is not part of a known build", ErrManifest, s.Name)
	case !trusted(s.URL):
		return fmt.Errorf("%w: %s: url must be a release or commit archive of a trusted host", ErrManifest, s.Name)
	case !sha256Hex.MatchString(s.SHA256):
		return fmt.Errorf("%w: %s: sha256 must be 64 hex characters", ErrManifest, s.Name)
	case !inside(s.Tree):
		return fmt.Errorf("%w: %s: bad tree", ErrManifest, s.Name)
	default:
		return nil
	}
}

func (m Manifest) Count() int {
	n := len(m.Binaries) + len(m.Go)
	if len(m.Sources) > 0 {
		n++
	}
	return n
}

func validateSources(m Manifest) error {
	if len(m.Sources) == 0 {
		return nil
	}
	errs := make([]error, 0, len(m.Sources))
	names := make([]string, 0, len(m.Sources))
	for _, s := range m.Sources {
		errs = append(errs, s.validate())
		names = append(names, s.Name)
	}
	slices.Sort(names)
	want := slices.Sorted(slices.Values(jsonfilterSources))
	if !slices.Equal(names, want) {
		errs = append(errs, fmt.Errorf("%w: %s needs exactly the sources %s", ErrManifest, JSONFilter, strings.Join(want, ", ")))
	}
	if !slices.ContainsFunc(m.Binaries, func(b Binary) bool { return b.Name == CmakeBinary }) {
		errs = append(errs, fmt.Errorf("%w: %s needs the %s binary", ErrManifest, JSONFilter, CmakeBinary))
	}
	return errors.Join(errs...)
}

func (i Installer) buildJSONFilter(ctx context.Context, sources []Source) error {
	stage, err := os.MkdirTemp(i.Dir, buildPattern)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	trees := make(map[string]string, len(sources))
	for _, s := range sources {
		tree, uerr := i.unpackSource(ctx, stage, s)
		if uerr != nil {
			return fmt.Errorf("%s: %w", s.Name, uerr)
		}
		trees[s.Name] = tree
	}
	prefix := filepath.Join(stage, prefixDir)
	build := filepath.Join(stage, jsoncBuildDir)
	if err := i.runAll(ctx, jsoncSteps(trees[sourceJSONC], build, prefix)); err != nil {
		return err
	}
	if err := copyHeaders(trees[sourceLibubox], filepath.Join(prefix, "include", sourceLibubox)); err != nil {
		return err
	}
	return i.runAll(ctx, jsonpathSteps(trees[sourceJSONPath], trees[sourceLibubox], prefix, filepath.Join(i.Dir, JSONFilter)))
}

func (i Installer) unpackSource(ctx context.Context, stage string, s Source) (string, error) {
	data, err := i.verified(ctx, s.URL, s.SHA256)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(stage, s.Name)
	if merr := os.Mkdir(dir, dirPerm); merr != nil {
		return "", merr
	}
	root, err := i.untar(ctx, dir, data)
	if err != nil {
		return "", err
	}
	tree := filepath.Join(root, filepath.FromSlash(s.Tree))
	if info, serr := os.Stat(tree); serr != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrMember, s.Tree)
	}
	return tree, nil
}

func jsoncSteps(src, build, prefix string) []procexec.Command {
	return []procexec.Command{
		{Name: CmakeBinary, Args: append([]string{"-S", src, "-B", build, "-DCMAKE_INSTALL_PREFIX=" + prefix}, jsoncFlags...)},
		{Name: CmakeBinary, Args: []string{"--build", build, "--parallel", "2"}},
		{Name: CmakeBinary, Args: []string{"--install", build}},
	}
}

func jsonpathSteps(jsonpath, libubox, prefix, out string) []procexec.Command {
	lemon := filepath.Join(jsonpath, "contrib", "lemon")
	args := []string{"-Os", "-std=gnu99", "-D_GNU_SOURCE", "-DJSONC", "-I" + filepath.Join(prefix, "include"), "-I" + filepath.Join(prefix, "include", sourceJSONC), outFlag, out}
	for _, f := range jsonpathFiles {
		args = append(args, filepath.Join(jsonpath, f))
	}
	for _, f := range libuboxFiles {
		args = append(args, filepath.Join(libubox, f))
	}
	return []procexec.Command{
		{Name: CCBinary, Args: []string{"-std=gnu17", outFlag, lemon, lemon + ".c"}},
		{Name: LemonBinary, Args: []string{"parser.y"}, Dir: jsonpath},
		{Name: CCBinary, Args: append(args, filepath.Join(prefix, "lib", "libjson-c.a"))},
	}
}

func (i Installer) runAll(ctx context.Context, steps []procexec.Command) error {
	for _, c := range steps {
		if _, err := i.Exec.Run(ctx, c); err != nil {
			return fmt.Errorf("%s %s: %w: %s", c.Name, c.Args[0], err, procexec.Output(err))
		}
	}
	return nil
}

func copyHeaders(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if merr := os.MkdirAll(dst, dirPerm); merr != nil {
		return merr
	}
	from, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()
	to, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { _ = to.Close() }()
	for _, e := range entries {
		if !e.Type().IsRegular() || filepath.Ext(e.Name()) != headerExt {
			continue
		}
		data, rerr := from.ReadFile(e.Name())
		if rerr != nil {
			return rerr
		}
		if werr := to.WriteFile(e.Name(), data, binPerm); werr != nil {
			return werr
		}
	}
	return nil
}
