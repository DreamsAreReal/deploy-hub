package golang

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	testMainSuffix = ".test"
	variantMark    = " ["
	closureError   = "go list -deps -test: %w: %s"
)

type BuiltModule struct {
	Path    string
	Version string
	Replace *BuiltModule
}

type Built struct {
	ImportPath      string
	Dir             string
	Standard        bool
	Module          *BuiltModule
	Deps            []string
	GoFiles         []string
	CgoFiles        []string
	CFiles          []string
	CXXFiles        []string
	HFiles          []string
	SFiles          []string
	SysoFiles       []string
	EmbedFiles      []string
	TestGoFiles     []string
	XTestGoFiles    []string
	TestEmbedFiles  []string
	XTestEmbedFiles []string
}

type Closure struct {
	Packages map[string]Built
	Tests    map[string][]string
}

func ListClosure(ctx context.Context, ws gate.Workspace, env []string) (Closure, error) {
	res, err := ws.RunEnv(ctx, env, GoBinary, "list", "-deps", "-test", "-json", allPkgs)
	if err != nil {
		return Closure{}, fmt.Errorf(closureError, err, gate.Tail(procexec.Output(err)))
	}
	return DecodeClosure([]byte(res.Stdout))
}

func DecodeClosure(data []byte) (Closure, error) {
	c := Closure{Packages: map[string]Built{}, Tests: map[string][]string{}}
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var b Built
		err := dec.Decode(&b)
		if errors.Is(err, io.EOF) {
			return c, nil
		}
		if err != nil {
			return Closure{}, err
		}
		c.add(b)
	}
}

func (c Closure) add(b Built) {
	if pkg, ok := strings.CutSuffix(b.ImportPath, testMainSuffix); ok && !strings.Contains(pkg, variantMark) {
		c.Tests[pkg] = testDeps(pkg, b.Deps)
		return
	}
	base, _, variant := strings.Cut(b.ImportPath, variantMark)
	if _, seen := c.Packages[base]; !variant || !seen {
		b.ImportPath = base
		c.Packages[base] = b
	}
}

func testDeps(pkg string, deps []string) []string {
	out := make([]string, 0, len(deps)+1)
	out = append(out, pkg)
	for _, d := range deps {
		base, _, _ := strings.Cut(d, variantMark)
		out = append(out, base)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (b Built) Files() []string {
	lists := [][]string{b.GoFiles, b.CgoFiles, b.CFiles, b.CXXFiles, b.HFiles, b.SFiles, b.SysoFiles,
		b.EmbedFiles, b.TestGoFiles, b.XTestGoFiles, b.TestEmbedFiles, b.XTestEmbedFiles}
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (b Built) Versioned() (string, bool) {
	m := b.Module
	if m != nil && m.Replace != nil {
		m = m.Replace
	}
	if m == nil || m.Version == "" {
		return "", false
	}
	return m.Path + "@" + m.Version, true
}
