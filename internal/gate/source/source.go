package source

import (
	"bytes"
	"context"
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/lexis"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	GitBinary     = "git"
	extGo         = ".go"
	extYML        = ".yml"
	extYAML       = ".yaml"
	goModFile     = "go.mod"
	goModComment  = "//"
	goModIndirect = "indirect"
	shebang       = "#!"
	hashComment   = "#"
	binarySniff   = 8000
	locationForm  = "%s:%d"
	copyDirPerm   = 0o700
	copyFilePerm  = 0o600
	fixtureDir    = "testdata"
)

var lex = lexis.Lex

var hashCommented = map[string]bool{
	".sh": true, ".bash": true, ".py": true, ".toml": true, ".mk": true,
	".service": true, ".timer": true, ".conf": true, ".env": true,
	".gitignore": true, ".dockerignore": true,
}

var prose = map[string]bool{".md": true, ".markdown": true, ".txt": true, ".rst": true, ".adoc": true}

var generated = map[string]bool{
	"go.sum": true, "poetry.lock": true, "Cargo.lock": true, "yarn.lock": true, "uv.lock": true, "pnpm-lock.yaml": true,
	"package-lock.json": true, "packages.lock.json": true, "Gemfile.lock": true, "composer.lock": true, "Pipfile.lock": true,
	"gradle.lockfile": true, "flake.lock": true,
}

var requirementsFile = regexp.MustCompile(`^(requirements|constraints).*\.txt$`)

var hashCommentedNames = regexp.MustCompile(`^(Dockerfile.*|.*\.Dockerfile|Makefile|\.gitignore|\.dockerignore|\.env.*)$`)

func Tracked(ctx context.Context, ex procexec.Runner, dir string) ([]string, error) {
	res, err := ex.Run(ctx, procexec.Command{Name: GitBinary, Args: []string{"-C", dir, "ls-files", "-z"}})
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w: %s", err, procexec.Output(err))
	}
	var files []string
	for _, f := range strings.Split(res.Stdout, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

func Ext(dir, rel string) string {
	if ext := path.Ext(rel); ext != "" {
		return ext
	}
	f, err := os.Open(filepath.Clean(filepath.Join(dir, filepath.FromSlash(rel))))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, binarySniff)
	n, _ := io.ReadFull(f, head)
	return lexis.Ext(rel, head[:n])
}

func Fixture(rel string) bool {
	return slices.Contains(strings.Split(path.Dir(rel), "/"), fixtureDir)
}

func Code(rel string) bool {
	return !prose[path.Ext(rel)] && !generated[path.Base(rel)] && !Fixture(rel)
}

func Select(ctx context.Context, ws gate.Workspace, keep func(string) bool) ([]string, error) {
	files, err := Tracked(ctx, ws.Exec, ws.Src)
	return slices.DeleteFunc(files, func(f string) bool { return !keep(f) }), err
}

type textFile struct {
	rel  string
	data []byte
}

func readText(dir string, files []string) []textFile {
	out := make([]textFile, 0, len(files))
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, filepath.FromSlash(rel))))
		if err != nil || bytes.IndexByte(data[:min(len(data), binarySniff)], 0) >= 0 {
			continue
		}
		out = append(out, textFile{rel: rel, data: data})
	}
	return out
}

func withTracked(ctx context.Context, ws gate.Workspace, reason string, scan func([]textFile) []string) gate.Outcome {
	files, err := Tracked(ctx, ws.Exec, ws.Src)
	if err != nil {
		return gate.Fail(err.Error())
	}
	return gate.Findings(reason, scan(readText(ws.Src, files)))
}

func Comments(ctx context.Context, ws gate.Workspace) gate.Outcome {
	return withTracked(ctx, ws, "comments are not allowed in source code", func(files []textFile) []string {
		findings := make([]string, 0, len(files))
		for _, f := range files {
			findings = append(findings, commentsIn(f)...)
		}
		return findings
	})
}

func commentsIn(f textFile) []string {
	if scan := scannerFor(f.rel); scan != nil {
		return scan(f)
	}
	return nil
}

func scannerFor(rel string) func(textFile) []string {
	base, ext := path.Base(rel), path.Ext(rel)
	switch {
	case requirementsFile.MatchString(base):
		return hashComments
	case generated[base] || prose[ext]:
		return nil
	case base == goModFile:
		return goModComments
	case ext == extGo:
		return goComments
	case ext == extYML || ext == extYAML:
		return yamlComments
	}
	return lexedComments
}

func lexedComments(f textFile) []string {
	toks, known, err := lex(f.rel, f.data)
	switch {
	case err != nil:
		return []string{f.rel + ": " + err.Error()}
	case known:
		return noteLines(f.rel, toks)
	case hashCommented[path.Ext(f.rel)] || hashCommentedNames.MatchString(path.Base(f.rel)):
		return hashComments(f)
	}
	return nil
}

func goModComments(f textFile) []string {
	var findings []string
	for i, line := range strings.Split(string(f.data), "\n") {
		_, note, found := strings.Cut(line, goModComment)
		if found && strings.TrimSpace(note) != goModIndirect {
			findings = append(findings, fmt.Sprintf(locationForm, f.rel, i+1))
		}
	}
	return findings
}

func noteLines(rel string, toks []lexis.Token) []string {
	var findings []string
	last := 0
	for _, t := range toks {
		if t.Note && t.Line != last {
			findings = append(findings, fmt.Sprintf(locationForm, rel, t.Line))
			last = t.Line
		}
	}
	return findings
}

func goComments(f textFile) []string {
	fset := token.NewFileSet()
	file := fset.AddFile(f.rel, fset.Base(), len(f.data))
	var findings []string
	var s scanner.Scanner
	s.Init(file, f.data, func(pos token.Position, msg string) {
		findings = append(findings, fmt.Sprintf(locationForm+": %s", f.rel, pos.Line, msg))
	}, scanner.ScanComments)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return findings
		}
		if tok == token.COMMENT && !lexis.IsDirective(lit) {
			findings = append(findings, fmt.Sprintf(locationForm, f.rel, fset.Position(pos).Line))
		}
	}
}

func yamlComments(f textFile) []string {
	var root yaml.Node
	if err := yaml.Unmarshal(f.data, &root); err != nil {
		return []string{f.rel + ": " + err.Error()}
	}
	var findings []string
	walkYAML(&root, func(n *yaml.Node) {
		if n.HeadComment != "" || n.LineComment != "" || n.FootComment != "" {
			findings = append(findings, fmt.Sprintf(locationForm, f.rel, n.Line))
		}
	})
	return findings
}

func walkYAML(n *yaml.Node, visit func(*yaml.Node)) {
	visit(n)
	for _, c := range n.Content {
		walkYAML(c, visit)
	}
}

func hashComments(f textFile) []string {
	var findings []string
	for i, line := range strings.Split(string(f.data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, hashComment) && (i > 0 || !strings.HasPrefix(trimmed, shebang)) {
			findings = append(findings, fmt.Sprintf(locationForm, f.rel, i+1))
		}
	}
	return findings
}

func Markers(ctx context.Context, ws gate.Workspace) gate.Outcome {
	re := markerPattern(ws.Policy.Markers)
	return withTracked(ctx, ws, "work markers are not allowed", func(files []textFile) []string {
		var findings []string
		for _, f := range files {
			for i, line := range strings.Split(string(f.data), "\n") {
				if m := re.FindString(line); m != "" {
					findings = append(findings, fmt.Sprintf(locationForm+": %s", f.rel, i+1, m))
				}
			}
		}
		return findings
	})
}

func markerPattern(markers []string) *regexp.Regexp {
	quoted := make([]string, 0, len(markers))
	for _, m := range markers {
		quoted = append(quoted, regexp.QuoteMeta(strings.ToUpper(m)))
	}
	return regexp.MustCompile(`\b(` + strings.Join(quoted, "|") + `)\b`)
}

func Language(ctx context.Context, ws gate.Workspace) gate.Outcome {
	return withTracked(ctx, ws, "tracked files must be written in English", func(files []textFile) []string {
		findings := make([]string, 0, len(files))
		for _, f := range files {
			if !ws.App.IsLocalized(f.rel) {
				findings = append(findings, foreignLetter(f)...)
			}
		}
		return findings
	})
}

func foreignLetter(f textFile) []string {
	line := 1
	for _, r := range string(f.data) {
		if r == '\n' {
			line++
		}
		if unicode.IsLetter(r) && !unicode.Is(unicode.Latin, r) {
			return []string{fmt.Sprintf(locationForm+": %q", f.rel, line, r)}
		}
	}
	return nil
}

func Hygiene(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := Tracked(ctx, ws.Exec, ws.Src)
	if err != nil {
		return gate.Fail(err.Error())
	}
	var findings []string
	for _, rel := range files {
		if !ws.App.AllowsFile(rel) {
			findings = append(findings, forbidden(rel, ws.Policy.ForbiddenFiles)...)
		}
	}
	return gate.Findings("forbidden files are tracked", findings)
}

func forbidden(rel string, patterns []string) []string {
	base := strings.ToLower(path.Base(rel))
	for _, p := range patterns {
		if ok, _ := path.Match(strings.ToLower(p), base); ok {
			return []string{rel + " matches " + p}
		}
	}
	return nil
}

func CopyTracked(ctx context.Context, ws gate.Workspace, dest string) error {
	from, err := os.OpenRoot(ws.Src)
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()
	files, err := Tracked(ctx, ws.Exec, ws.Src)
	if err == nil {
		err = os.MkdirAll(dest, copyDirPerm)
	}
	if err != nil {
		return err
	}
	to, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = to.Close() }()
	for _, rel := range files {
		if cerr := copyFile(from, to, filepath.FromSlash(rel)); cerr != nil {
			return cerr
		}
	}
	return nil
}

func copyFile(from, to *os.Root, rel string) error {
	info, err := from.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return err
	}
	data, err := from.ReadFile(rel)
	if err != nil {
		return err
	}
	if err := to.MkdirAll(filepath.Dir(rel), copyDirPerm); err != nil {
		return err
	}
	return to.WriteFile(rel, data, copyFilePerm)
}
