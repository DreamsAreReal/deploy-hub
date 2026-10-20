package tokmut

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	jsAndOr = "4 logical && -> ||"
	filePy  = "m.py"
	fileTS  = "m.ts"
	fileSh  = "m.sh"
)

func describe(t *testing.T, name, src string, fam Family) []string {
	t.Helper()
	ms, err := Points(name, []byte(src), fam)
	testutil.NoErr(t, err)
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, fmt.Sprintf("%d %s %s -> %s", m.Line, m.Operator, m.Original, m.Mutated))
		testutil.Equal(t, string(m.Apply([]byte(src)))[:0], "")
	}
	return out
}

func sample(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", name+".txt")))
	testutil.NoErr(t, err)
	return string(data)
}

func TestPythonMutants(t *testing.T) {
	testutil.Equal(t, describe(t, "sample.py", sample(t, "sample.py"), Python), []string{
		"5 negate-condition user is not None and user.id in items -> not (user is not None and user.id in items)",
		"5 comparison-negation is not -> is",
		"5 logical and -> or",
		"5 comparison-negation in -> not in",
		"6 return-value return True -> return None",
		"6 logical True -> False",
		"7 negate-condition user is None or not items -> not (user is None or not items)",
		"7 comparison-negation is -> is not",
		"7 logical or -> and",
		"7 remove-negation not items -> items",
		"10 arithmetic ** -> *",
		"10 arithmetic // -> /",
		"11 error-handling (KeyError, IndexError) -> ()",
		"13 error-handling except: -> except ():",
		"15 negate-condition total > 0 and \"x\" not in items -> not (total > 0 and \"x\" not in items)",
		"15 comparison-boundary > -> >=",
		"15 comparison-negation > -> <=",
		"15 logical and -> or",
		"15 comparison-negation not in -> in",
		"16 arithmetic -= -> +=",
		"17 return-value return total % 2 == 0 -> return None",
		"17 arithmetic % -> *",
		"17 comparison-negation == -> !=",
		"1 remove-statement LIMIT = 3 -> pass",
		"8 remove-statement raise ValueError(\"no access\") -> pass",
		"10 remove-statement total = sum(i ** 2 for i in items) // LIMIT -> pass",
		"12 remove-statement log(exc) -> pass",
		"16 remove-statement total -= 1 -> pass",
	})
}

func TestJavaScriptMutants(t *testing.T) {
	testutil.Equal(t, describe(t, "sample.ts", sample(t, "sample.ts"), JavaScript), []string{
		"4 negate-condition i > 0 && !vip -> !(i > 0 && !vip)",
		"4 comparison-boundary > -> >=",
		"4 comparison-negation > -> <=",
		jsAndOr,
		"4 remove-negation !vip -> vip",
		"5 arithmetic += -> -=",
		"5 arithmetic * -> /",
		"6 negate-condition i === 0 || vip -> !(i === 0 || vip)",
		"6 comparison-negation === -> !==",
		"6 logical || -> &&",
		"7 increment-decrement -- -> ++",
		"12 error-handling catch (err) { -> catch (err) { throw err;",
		"15 negate-condition total >= 100 -> !(total >= 100)",
		"15 comparison-boundary >= -> >",
		"15 comparison-negation >= -> <",
		"17 return-value return total -> return null",
		"5 remove-statement total += i * 2 -> ",
		"7 remove-statement total-- -> ",
		"11 remove-statement check(total) -> ",
		"13 remove-statement log(`failed // ${err}`) -> ",
	})
}

func TestCSharpMutants(t *testing.T) {
	testutil.Equal(t, describe(t, "sample.cs", sample(t, "sample.cs"), CSharp), []string{
		"8 negate-condition i > 0 && !vip -> !(i > 0 && !vip)",
		"8 comparison-boundary > -> >=",
		"8 comparison-negation > -> <=",
		"8 logical && -> ||",
		"8 remove-negation !vip -> vip",
		"8 arithmetic += -> -=",
		"8 arithmetic * -> /",
		"10 error-handling catch (InvalidOperationException e) { -> catch (InvalidOperationException e) { throw;",
		"11 error-handling catch { -> catch { throw;",
		"12 return-value return total -> return default",
		"8 remove-statement total += i * 2 -> ",
		"10 remove-statement Check(total) -> ",
		"10 remove-statement Log(e.Message) -> ",
		"11 remove-statement Check(total) -> ",
		"11 remove-statement Log(\"x\") -> ",
	})
}

func TestShellMutants(t *testing.T) {
	testutil.Equal(t, describe(t, "sample.sh", sample(t, "sample.sh"), Shell), []string{
		"4 negate-condition [ -> ! [",
		"4 comparison-boundary -ge -> -gt",
		jsAndOr,
		"4 remove-negation ! grep -> grep",
		"7 negate-condition [ -> ! [",
		"7 comparison-boundary -lt -> -le",
		"10 comparison-negation = -> !=",
		"10 logical || -> &&",
		"14 return-value return 0 -> return 1",
		"3 remove-statement count=0 -> :",
		"5 remove-statement count=1 -> :",
		"8 remove-statement count=$((count + 1)) -> :",
		"11 remove-statement cat <<EOT\nif heredoc -ge 3\nEOT -> :",
	})
}

func TestEdgeCases(t *testing.T) {
	testutil.Equal(t, describe(t, filePy, "x = a -b\nreturn\ny = 1 if a else 2\nreturn None\nif:\n    pass\nz = [v for v in w if v]\n", Python), []string{
		"1 arithmetic - -> +",
		"1 remove-statement x = a -b -> pass",
		"3 remove-statement y = 1 if a else 2 -> pass",
		"7 remove-statement z = [v for v in w if v] -> pass",
	})
	testutil.Equal(t, describe(t, filePy, "y = [i for i in a if i in b]\nz = f(\n    a < b,\n)\nnot\n", Python), []string{
		"1 comparison-negation in -> not in",
		"3 comparison-boundary < -> <=",
		"3 comparison-negation < -> >=",
		"1 remove-statement y = [i for i in a if i in b] -> pass",
		"2 remove-statement z = f(\n    a < b,\n) -> pass",
		"5 remove-statement not -> pass",
	})
	testutil.Equal(t, describe(t, fileTS, "if (x) {\n  f(\n    a);\n}\nwhile x;\n!;\ns = \"a < b\";\n", JavaScript), []string{
		"1 negate-condition x -> !(x)",
		"7 remove-statement s = \"a < b\" -> ",
	})
	testutil.Equal(t, len(describe(t, fileTS, "catch (e) [\n", JavaScript)), 0)
	testutil.Equal(t, describe(t, fileTS, "try {} catch { g(); }\nif () {}\nf(a\n", JavaScript), []string{
		"1 remove-statement g() -> ",
	})
	testutil.Equal(t, describe(t, fileSh, "x=1\nif ! f; then :; fi\nif\nreturn 1\n[ a = b ]\n", Shell), []string{
		"2 remove-negation ! f -> f",
		"4 return-value return 1 -> return 0",
		"5 comparison-negation = -> !=",
		"1 remove-statement x=1 -> :",
	})
	testutil.Equal(t, describe(t, "m.cs", "catch (Exception) { Log(); }\n", CSharp), []string{
		"1 error-handling catch (Exception) { -> catch (Exception) { throw;",
		"1 remove-statement Log() -> ",
	})
}

func TestUnknownAndInvalidSources(t *testing.T) {
	ms, err := Points("notes.unknownext", []byte("a < b"), Python)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(ms), 0)
	_, err = Points(filePy, []byte("a = \"\xff\"\n"), Python)
	testutil.Fails(t, err)
}

func TestOffsetsSurviveWindowsLineEndings(t *testing.T) {
	src := "a = 1\r\nif a > 2:\r\n    b = a\r\n"
	ms, err := Points(filePy, []byte(src), Python)
	testutil.NoErr(t, err)
	var boundaryMutant mutation.Mutant
	for _, m := range ms {
		if m.Operator == mutation.OpBoundary {
			boundaryMutant = m
		}
	}
	testutil.Equal(t, string(boundaryMutant.Apply([]byte(src))), "a = 1\r\nif a >= 2:\r\n    b = a\r\n")
}

func TestScannerTokens(t *testing.T) {
	toks := scan([]byte("a>=1;(b)\n"), make([]bool, 9))
	kinds := make([]kind, 0, len(toks))
	for _, tk := range toks {
		kinds = append(kinds, tk.kind)
	}
	testutil.Equal(t, kinds, []kind{kindWord, kindOp, kindNumber, kindEnd, kindOpen, kindWord, kindClose, kindNewline})
	testutil.Equal(t, toks[1].text, ">=")
	mask := []bool{false, true, true, false}
	split := scan([]byte("<=>="), mask)
	testutil.Equal(t, len(split), 3)
	testutil.Equal(t, split[0].text, "<")
	testutil.Equal(t, split[1].kind, kindText)
	testutil.Equal(t, split[2].text, "=")
	testutil.Equal(t, delta(token{kind: kindEnd}), 0)
}

func FuzzPoints(f *testing.F) {
	f.Add(filePy, "if a <= b and not c:\n    return x\n")
	f.Add(fileTS, "try { f(a > b); } catch (e) { g(!e); }\n")
	f.Add(fileSh, "if [ \"$a\" -ge 1 ]; then return 2; fi\n")
	families := []Family{Python, JavaScript, CSharp, Shell}
	f.Fuzz(func(t *testing.T, name, src string) {
		for _, fam := range families {
			ms, err := Points(name, []byte(src), fam)
			if err != nil {
				return
			}
			for _, m := range ms {
				if m.Line < 1 || m.Original == m.Mutated {
					t.Fatalf("bad mutant %+v", m)
				}
				_ = m.Apply([]byte(src))
			}
		}
	})
}
