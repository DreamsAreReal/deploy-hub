package dupl

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/lexis"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	fileA = "a.go"
	fileB = "b.go"
	fileC = "c.go"
	fnSum = "sum"
)

const body = `
func sum(values []int) int {
	total := 0
	for _, v := range values {
		if v > 0 {
			total += v * 2
		}
	}
	return total
}
`

func file(pkg, fn string) []byte {
	return []byte("package " + pkg + "\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n" + strings.Replace(body, fnSum, fn, 1))
}

func TestAnalyzeFindsClonesAcrossFiles(t *testing.T) {
	rep := Analyze(map[string][]byte{fileA: file("a", fnSum), fileB: file("b", fnSum), fileC: []byte("package c\n\nvar x = 1\n")}, 20)
	testutil.Equal(t, rep.CodeLines, 19)
	testutil.Equal(t, rep.DuplicatedLines, 18)
	testutil.Equal(t, rep.Findings(), []string{fileA + ": lines 8-16", fileB + ": lines 8-16"})
	testutil.Equal(t, rep.Percent(), float64(18)*100/19)
}

func TestImportsAndPackageClausesAreIgnored(t *testing.T) {
	a := []byte("package a\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"strings\"\n)\n\nvar A = fmt.Sprint(os.Args, strings.Join)\n")
	b := []byte("package a\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"strings\"\n)\n\nvar B = 2\n")
	c := []byte("package a\nimport \"fmt\"\nvar C = fmt.Sprint\n")
	rep := Analyze(map[string][]byte{fileA: a, fileB: b, fileC: c}, 5)
	testutil.Equal(t, rep.DuplicatedLines, 0)
	testutil.Equal(t, rep.CodeLines, 3)
}

const (
	filePy  = "a.py"
	filePy2 = "b.py"
)

const pyBody = `def total(values):
    result = 0
    for v in values:
        if v > 0:
            result += v * 2
    return result
`

func TestClonesAcrossStacks(t *testing.T) {
	py := "import os\nfrom a import b\n" + pyBody
	rep := Analyze(map[string][]byte{filePy: []byte(py), filePy2: []byte("# note\n" + pyBody), "c.js": []byte("const x = 1;\n")}, 20)
	testutil.Equal(t, rep.Findings(), []string{"a.py: lines 3-8", "b.py: lines 2-7"})
	testutil.Equal(t, rep.CodeLines, 13)
	imports := Analyze(map[string][]byte{"a.ts": []byte("import { a, b, c, d } from \"x\";\nexport const p = a;\n"), "b.ts": []byte("import { a, b, c, d } from \"x\";\nexport const q = 2;\n")}, 5)
	testutil.Equal(t, imports.DuplicatedLines, 0)
	testutil.Equal(t, imports.CodeLines, 2)
	includes := Analyze(map[string][]byte{"a.c": []byte("#include <stdio.h>\n#include <stdlib.h>\nint a;\n"), "b.c": []byte("#include <stdio.h>\n#include <stdlib.h>\nint b;\n")}, 4)
	testutil.Equal(t, includes.DuplicatedLines, 0)
}

func TestCheckCoversEveryCodeFile(t *testing.T) {
	files := map[string][]byte{filePy: []byte(pyBody), filePy2: []byte(pyBody), "notes.md": []byte(pyBody), "c.html": []byte(pyBody)}
	out := Check(context.Background(), workspace(t, files, nil))
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Values[ValueCodeLines], float64(12))
}

func TestFixturesAreNotCountedAsClones(t *testing.T) {
	files := map[string][]byte{filePy: []byte(pyBody), "testdata/" + filePy2: []byte(pyBody)}
	out := Check(context.Background(), workspace(t, files, nil))
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Equal(t, out.Values[ValueCodeLines], float64(6))
}

func TestUnreadableSourcesFailTheCheck(t *testing.T) {
	saved := lex
	t.Cleanup(func() { lex = saved })
	lex = func(string, []byte) ([]lexis.Token, bool, error) { return nil, true, errors.New("lexer broke") }
	out := Check(context.Background(), workspace(t, map[string][]byte{"z.py": []byte("x = 1\n"), filePy: []byte("y = 2\n")}, nil))
	testutil.Equal(t, out.Reason, reasonUnreadable)
	testutil.Equal(t, out.Findings, []string{"a.py: lexer broke", "z.py: lexer broke"})
}

func TestWindowLargerThanCode(t *testing.T) {
	rep := Analyze(map[string][]byte{fileA: file("a", fnSum), fileB: file("b", fnSum)}, 1000)
	testutil.Equal(t, rep.DuplicatedLines, 0)
	testutil.Equal(t, Report{}.Percent(), float64(0))
	testutil.Equal(t, len(windowHashes(nil, 0)), 0)
}

func TestHashCollisionsAreVerified(t *testing.T) {
	units := []unit{{name: "a", tokens: []tok{{id: 1, line: 1}, {id: 2, line: 1}}}, {name: "b", tokens: []tok{{id: 3, line: 1}, {id: 4, line: 1}}}}
	occ := []occurrence{{unit: 0, start: 0}, {unit: 1, start: 0}}
	testutil.True(t, !matchesAnother(units, occ, 0, 2), "different tokens with an equal hash are not clones")
	testutil.True(t, equalWindow(units[0].tokens, units[0].tokens), "identical windows match")
}

func TestRanges(t *testing.T) {
	testutil.Equal(t, ranges([]int{1, 2, 3, 7, 9, 10}), "1-3, 7-7, 9-10")
	testutil.Equal(t, ranges(nil), "")
}

func workspace(t *testing.T, files map[string][]byte, listErr error) gate.Workspace {
	t.Helper()
	dir := t.TempDir()
	var listing strings.Builder
	for name, data := range files {
		testutil.Write(t, filepath.Join(dir, name), string(data))
		listing.WriteString(name + "\x00")
	}
	fake := &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stdout: listing.String()}, listErr
	}}
	return gate.Workspace{Src: dir, Exec: fake, Log: slog.New(slog.DiscardHandler),
		Policy: policy.Policy{Thresholds: policy.Thresholds{DuplicateTokens: 20, MaxDuplication: 2}}}
}

func TestCheck(t *testing.T) {
	dup := Check(context.Background(), workspace(t, map[string][]byte{fileA: file("a", fnSum), fileB: file("b", fnSum), "README.md": []byte("x")}, nil))
	testutil.Equal(t, dup.Status, gate.StatusFail)
	testutil.Equal(t, dup.Values[ValueDupLines], float64(18))
	renamed := Check(context.Background(), workspace(t, map[string][]byte{fileA: file("a", fnSum), fileB: file("b", "other")}, nil))
	testutil.Equal(t, renamed.Status, gate.StatusFail)
	unique := Check(context.Background(), workspace(t, map[string][]byte{fileA: file("a", fnSum)}, nil))
	testutil.Equal(t, unique.Status, gate.StatusPass)
	testutil.Equal(t, unique.Values[ValueDuplication], float64(0))
	testutil.Equal(t, Check(context.Background(), workspace(t, nil, errors.New("not a repo"))).Status, gate.StatusFail)
	missing := workspace(t, nil, nil)
	missing.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{Stdout: "gone.go\x00"}, nil }}
	testutil.Equal(t, Check(context.Background(), missing).Status, gate.StatusFail)
}

const fuzzWindows = 64

func FuzzAnalyze(f *testing.F) {
	f.Add([]byte(body), []byte(body), 10)
	f.Fuzz(func(t *testing.T, a, b []byte, window int) {
		size := 1 + max(window%fuzzWindows, -window%fuzzWindows)
		rep := Analyze(map[string][]byte{fileA: a, fileB: b}, size)
		if rep.DuplicatedLines > rep.CodeLines || rep.Percent() > 100 {
			t.Fatalf("impossible report: %+v", rep)
		}
	})
}

func TestClonesAtDifferentOffsetsAreFound(t *testing.T) {
	shifted := []byte("package b\n\nvar pad = []int{1, 2, 3}\n" + body)
	rep := Analyze(map[string][]byte{fileA: file("a", fnSum), fileB: shifted}, 20)
	testutil.Equal(t, rep.Findings(), []string{fileA + ": lines 8-16", fileB + ": lines 5-13"})
}

func TestExactWindowAndOrderedFindings(t *testing.T) {
	snippetSrc := "package x\n\nvar v = f(1, 2)\n"
	exact := map[string][]byte{"d.go": []byte(snippetSrc), "c.go": []byte(snippetSrc), "b.go": []byte(snippetSrc), "a.go": []byte(snippetSrc)}
	window := len(goTokens("a.go", []byte(snippetSrc), map[string]uint64{}).tokens)
	rep := Analyze(exact, window)
	testutil.Equal(t, rep.Findings(), []string{"a.go: lines 3-3", "b.go: lines 3-3", "c.go: lines 3-3", "d.go: lines 3-3"})
	testutil.Equal(t, Analyze(exact, window+1).DuplicatedLines, 0)
}

func TestLimitIsStrict(t *testing.T) {
	files := map[string][]byte{fileA: file("a", fnSum), fileB: file("b", fnSum), fileC: []byte("package c\n\nvar x = 1\n")}
	w := workspace(t, files, nil)
	w.Policy.Thresholds.MaxDuplication = Analyze(files, w.Policy.Thresholds.DuplicateTokens).Percent()
	testutil.Equal(t, Check(context.Background(), w).Status, gate.StatusFail)
	w.Policy.Thresholds.MaxDuplication += 0.001
	testutil.Equal(t, Check(context.Background(), w).Status, gate.StatusPass)
}
