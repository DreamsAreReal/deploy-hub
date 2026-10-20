package lexis

import (
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func noteLines(t *testing.T, name, src string) []int {
	t.Helper()
	toks, ok, err := Lex(name, []byte(src))
	testutil.NoErr(t, err)
	testutil.True(t, ok, name+" has a lexer")
	var lines []int
	for _, tk := range toks {
		if tk.Note {
			lines = append(lines, tk.Line)
		}
	}
	return lines
}

func TestCommentsAcrossLanguages(t *testing.T) {
	cases := []struct {
		name, src string
		want      []int
	}{
		{"a.js", "const re = /\\/*x/;\nconst u = `a ${b} // no`;\nconst s = \"http://x\"; // yes\n/* yes */\n", []int{3, 4}},
		{"a.ts", "let n: number = 1;\n", nil},
		{"a.py", "def f():\n    x = \"# no\"  # yes\n    return x\n", []int{2}},
		{"a.sh", "#!/bin/sh\ncat <<EOF > /tmp/x.conf\n# heredoc\nEOF\necho $# ${#x} a#b # yes\n", []int{5}},
		{"a.c", "#include <stdio.h>\n#define X 1\nint main(){ return X; } // yes\n", []int{3}},
		{"a.rs", "fn f<'a>(x: &'a str) -> &'a str { x } // yes\nlet c = '/';\n", []int{1}},
		{"a.css", "a { background: url(http://x/y); } /* yes */\n", []int{1}},
		{"a.html", "<p>don't // text</p><!-- yes -->\n<script>var a = 1; // yes\n</script>\n", []int{1, 2}},
		{"a.cs", "var s = @\"C:\\path\\\"; // yes\n", []int{1}},
		{"a.sql", "SELECT 1; -- yes\n", []int{1}},
		{"Dockerfile", "FROM alpine\n# yes\nRUN echo \"#no\"\n", []int{2}},
		{"tsconfig.json", "{\n  // yes\n  \"a\": \"//b\"\n}\n", []int{2}},
		{"a.ps1", "$x = 1 # yes\n<# yes #>\n", []int{1, 2}},
	}
	for _, c := range cases {
		testutil.Equal(t, noteLines(t, c.name, c.src), c.want)
	}
}

func TestPythonDocstringsAreComments(t *testing.T) {
	src := "\"\"\"module\"\"\"\nimport os\n\n\ndef f():\n    r'''doc'''\n    x = \"\"\"value\"\"\"\n    print(\"\"\"inline\"\"\", x)\n    return \"\"\"multi\nline\"\"\"\n\n\ndef g():\n    \"plain\"\n    '''\n    block\n    '''\n    return 1\n"
	testutil.Equal(t, noteLines(t, "m.py", src), []int{1, 6, 15})
	testutil.Equal(t, len(noteLines(t, "m.js", "'''x'''\n")), 0)
}

func TestTokensCarryKindsAndLines(t *testing.T) {
	toks, ok, err := Lex("dir/x.py", []byte("a = 1\n\nb = 'two'\n"))
	testutil.NoErr(t, err)
	testutil.True(t, ok, "python is known")
	testutil.Equal(t, toks, []Token{
		{Line: 1, Offset: 0, Kind: kindName, Text: "a"},
		{Line: 1, Offset: 2, Kind: kindOperator, Text: "="},
		{Line: 1, Offset: 4, Kind: "LiteralNumberInteger", Text: "1"},
		{Line: 3, Offset: 7, Kind: kindName, Text: "b"},
		{Line: 3, Offset: 9, Kind: kindOperator, Text: "="},
		{Line: 3, Offset: 11, Kind: kindSingle, Text: "'two'"},
	})
}

func TestOffsetsPointIntoTheOriginalBytes(t *testing.T) {
	src := []byte("a = 1\r\nb = 22\r\n\r\nc = 3\n")
	toks, _, err := Lex("x.py", src)
	testutil.NoErr(t, err)
	for _, tk := range toks {
		testutil.Equal(t, string(src[tk.Offset:tk.Offset+len(tk.Text)]), tk.Text)
	}
	testutil.Equal(t, toks[len(toks)-1].Line, 4)
	original := offsetMap([]byte("a\r\nb\r\n"))
	testutil.Equal(t, []int{original(0), original(1), original(2), original(3)}, []int{0, 2, 3, 5})
	testutil.Equal(t, offsetMap([]byte("a\rb\r"))(3), 3)
}

func TestInvalidEncodingIsRejected(t *testing.T) {
	_, known, err := Lex("x.py", []byte("a = \"\xff\"\n"))
	testutil.True(t, known, "python is known")
	testutil.Is(t, err, ErrEncoding)
}

func TestUnknownFilesHaveNoLexer(t *testing.T) {
	toks, ok, err := Lex("notes.unknownext", []byte("# x"))
	testutil.NoErr(t, err)
	testutil.True(t, !ok && toks == nil, "no lexer")
}

const (
	kindName       = "Name"
	kindDouble     = "LiteralStringDouble"
	kindSingle     = "LiteralStringSingle"
	kindOperator   = "Operator"
	tripleX        = "'''x'''"
	tripleTwoLines = "'''b\nc'''"
)

func notes(toks []Token) []bool {
	markDocstrings(toks)
	out := make([]bool, 0, len(toks))
	for _, t := range toks {
		out = append(out, t.Note)
	}
	return out
}

func TestDocstringsMustStandAlone(t *testing.T) {
	testutil.Equal(t, notes([]Token{{Line: 1, Kind: kindName, Text: "a"}, {Line: 1, Kind: kindSingle, Text: tripleX}, {Line: 2, Kind: kindName, Text: "c"}}), []bool{false, false, false})
	testutil.Equal(t, notes([]Token{{Line: 1, Kind: kindSingle, Text: tripleTwoLines}, {Line: 2, Kind: kindName, Text: "d"}}), []bool{false, false})
	testutil.Equal(t, notes([]Token{{Line: 1, Kind: kindSingle, Text: tripleTwoLines}, {Line: 3, Kind: kindName, Text: "d"}}), []bool{true, false})
	testutil.Equal(t, notes([]Token{{Line: 4, Kind: kindSingle, Text: tripleX}}), []bool{true})
	testutil.Equal(t, notes([]Token{{Line: 1, Kind: kindName, Text: "a"}, {Line: 2, Kind: affixKind, Text: "r"}, {Line: 2, Kind: kindSingle, Text: tripleX}, {Line: 3, Kind: affixKind, Text: "b"}}), []bool{false, false, true, false})
	testutil.Equal(t, notes([]Token{{Line: 2, Kind: kindName, Text: "x"}, {Line: 2, Kind: affixKind, Text: "r"}, {Line: 2, Kind: kindSingle, Text: tripleX}}), []bool{false, false, false})
	testutil.Equal(t, notes([]Token{{Line: 1, Kind: kindSingle, Text: tripleX}, {Line: 1, Kind: kindName, Text: "y"}}), []bool{false, false})
	testutil.True(t, isTripleQuoted(Token{Kind: kindDouble, Text: `"""x"""`}), "double")
	testutil.True(t, isTripleQuoted(Token{Kind: kindSingle, Text: tripleX}), "single")
	testutil.True(t, !isTripleQuoted(Token{Kind: kindName, Text: tripleX}), "not a string")
	testutil.True(t, !isTripleQuoted(Token{Kind: kindDouble, Text: `"x"`}), "one quote")
}

func FuzzLex(f *testing.F) {
	f.Add("a.py", "x = '''y'''\n# z\n")
	f.Add("a.js", "/* a */ `b ${c}` // d\n")
	f.Fuzz(func(t *testing.T, name, src string) {
		toks, _, err := Lex(name, []byte(src))
		if err != nil {
			return
		}
		for i := 1; i < len(toks); i++ {
			if toks[i].Line < toks[i-1].Line {
				t.Fatalf("lines go backwards: %+v", toks)
			}
		}
	})
}
