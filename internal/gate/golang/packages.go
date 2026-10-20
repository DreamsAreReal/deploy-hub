package golang

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	GoBinary  = "go"
	allPkgs   = "./..."
	mainPkg   = "main"
	listError = "go list: %w: %s"
)

type Package struct {
	ImportPath   string
	Name         string
	Dir          string
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
	TestImports  []string
	XTestImports []string
	Deps         []string
}

type Packages []Package

func List(ctx context.Context, ws gate.Workspace) (Packages, error) {
	res, err := ws.Run(ctx, GoBinary, "list", "-json", allPkgs)
	if err != nil {
		return nil, fmt.Errorf(listError, err, gate.Tail(procexec.Output(err)))
	}
	return decodePackages([]byte(res.Stdout))
}

func decodePackages(data []byte) (Packages, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var out Packages
	for {
		var p Package
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
}

func mark(set map[string]bool, paths []string) {
	for _, p := range paths {
		set[p] = true
	}
}

func (ps Packages) TestSupport() map[string]bool {
	imported := map[string]bool{}
	for _, p := range ps {
		mark(imported, p.Imports)
		mark(imported, p.TestImports)
		mark(imported, p.XTestImports)
	}
	for ps.dropProductionImports(imported) {
	}
	out := map[string]bool{}
	for _, p := range ps {
		if p.Name != mainPkg && imported[p.ImportPath] {
			out[p.ImportPath] = true
		}
	}
	return out
}

func (ps Packages) dropProductionImports(support map[string]bool) bool {
	dropped := false
	for _, p := range ps {
		for _, imp := range p.Imports {
			if !support[p.ImportPath] && support[imp] {
				delete(support, imp)
				dropped = true
			}
		}
	}
	return dropped
}

func (ps Packages) Production() Packages {
	support := ps.TestSupport()
	var out Packages
	for _, p := range ps {
		if len(p.GoFiles) > 0 && !support[p.ImportPath] {
			out = append(out, p)
		}
	}
	return out
}

func (ps Packages) Paths() []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ImportPath)
	}
	sort.Strings(out)
	return out
}

func (p Package) HasTests() bool {
	return len(p.TestGoFiles)+len(p.XTestGoFiles) > 0
}

func (ps Packages) Dependents(path string) []string {
	var out []string
	for _, p := range ps {
		if p.ImportPath == path || !p.HasTests() {
			continue
		}
		if contains(p.Deps, path) || contains(p.TestImports, path) || contains(p.XTestImports, path) {
			out = append(out, p.ImportPath)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func Module(ctx context.Context, ws gate.Workspace) (string, error) {
	res, err := ws.Run(ctx, GoBinary, "list", "-m", "-f", "{{.Path}}")
	if err != nil {
		err = fmt.Errorf("go list -m: %w: %s", err, gate.Tail(procexec.Output(err)))
	}
	return strings.TrimSpace(res.Stdout), err
}
