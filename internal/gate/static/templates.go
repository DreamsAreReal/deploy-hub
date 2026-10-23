package static

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/static/gotmpl"
)

const (
	goManifest   = "go.mod"
	templatesDir = "static-templates"
	variantForm  = "variant-%04d.html"
	rootDir      = "."
	dirMode      = 0o750
	fileMode     = 0o600
)

type expansion struct {
	pages    []string
	variants map[string]gotmpl.Variant
	findings []string
}

func expand(ws gate.Workspace, files, pages []string) (expansion, error) {
	modules := moduleDirs(files)
	sets := map[string][]gotmpl.Source{}
	x := expansion{variants: map[string]gotmpl.Variant{}}
	for _, p := range pages {
		text, err := templateText(ws, p, modules)
		if err != nil {
			return x, err
		}
		if !gotmpl.IsTemplate(text) {
			x.pages = append(x.pages, p)
			continue
		}
		sets[path.Dir(p)] = append(sets[path.Dir(p)], gotmpl.Source{Path: p, Text: text})
	}
	return x, x.write(ws.Report(templatesDir), sets)
}

func templateText(ws gate.Workspace, page string, modules map[string]bool) (string, error) {
	if !inModule(page, modules) {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(ws.Src, filepath.FromSlash(page)))
	return string(data), err
}

func moduleDirs(files []string) map[string]bool {
	dirs := map[string]bool{}
	for _, f := range files {
		if path.Base(f) == goManifest {
			dirs[path.Dir(f)] = true
		}
	}
	return dirs
}

func inModule(page string, modules map[string]bool) bool {
	for dir := path.Dir(page); !modules[dir]; dir = path.Dir(dir) {
		if dir == rootDir {
			return false
		}
	}
	return true
}

func (x *expansion) write(dir string, sets map[string][]gotmpl.Source) error {
	if err := errors.Join(os.RemoveAll(dir), os.MkdirAll(dir, dirMode)); err != nil {
		return err
	}
	for _, set := range slices.Sorted(maps.Keys(sets)) {
		variants, findings := gotmpl.Expand(sets[set])
		for _, f := range findings {
			x.findings = append(x.findings, fmt.Sprintf(findingForm, f.File, f.Line, f.Rule, f.Message))
		}
		if err := x.save(dir, variants); err != nil {
			return err
		}
	}
	return nil
}

func (x *expansion) save(dir string, variants []gotmpl.Variant) error {
	for _, v := range variants {
		file := filepath.Join(dir, fmt.Sprintf(variantForm, len(x.variants)))
		if err := os.WriteFile(file, []byte(v.HTML), fileMode); err != nil {
			return err
		}
		x.pages = append(x.pages, file)
		x.variants[file] = v
	}
	return nil
}

func (x *expansion) locate(ws gate.Workspace, file string, line, column int) (string, int) {
	v, ok := x.variants[file]
	if !ok {
		return ws.Rel(file), line
	}
	return v.Origin(line, column)
}
