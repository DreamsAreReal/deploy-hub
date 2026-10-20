package scan

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var errExit = errors.New("exit status 1")

const (
	dockerSave = "docker save"
	gitList    = "git -C"
	dockerRun  = "docker run"
	gateImage  = "app:gate"
)

type reply struct {
	res procexec.Result
	err error
	run func(c procexec.Command)
}

type script map[string]reply

func (s script) handle(c procexec.Command) (procexec.Result, error) {
	line := procexectest.Line(c)
	best := ""
	for prefix := range s {
		if strings.HasPrefix(line, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return procexec.Result{}, errors.New("unexpected command: " + line)
	}
	if s[best].run != nil {
		s[best].run(c)
	}
	return s[best].res, s[best].err
}

func ok(stdout string) reply { return reply{res: procexec.Result{Stdout: stdout}} }

func workspace(t *testing.T, s script) gate.Workspace {
	t.Helper()
	return gate.Workspace{
		Src: t.TempDir(), Out: t.TempDir(), PolicyDir: "/policy", Module: "example.com/app",
		Exec: &procexectest.Fake{Handler: s.handle}, Log: slog.New(slog.DiscardHandler),
		Policy: policy.Policy{
			Licenses: []string{"MIT", "Apache-2.0", "BSD-3-Clause"}, SASTRules: []string{"p/default", "p/golang"},
			Images: policy.Images{Semgrep: "semgrep/semgrep@sha256:abc", ZAP: "zap@sha256:def"},
		},
	}
}

func reportArg(c procexec.Command) string {
	for i, a := range c.Args {
		if a == "--report-path" {
			return c.Args[i+1]
		}
	}
	return ""
}

func writeReport(content string) func(c procexec.Command) {
	return func(c procexec.Command) { _ = os.WriteFile(reportArg(c), []byte(content), 0o600) }
}

func TestGitleaks(t *testing.T) {
	leak := `[{"RuleID":"aws-access-token","File":"cfg.yml","StartLine":3,"Commit":"0123456789abcdef"}]`
	out := Gitleaks(context.Background(), workspace(t, script{
		GitleaksBinary + " git": {run: writeReport(leak)},
		GitleaksBinary + " dir": {run: writeReport(`[{"RuleID":"jwt","File":"x.env","StartLine":1,"Commit":"abc"}]`)},
	}))
	testutil.Equal(t, out.Findings, []string{"aws-access-token cfg.yml:3 (commit 01234567)", "jwt x.env:1 (commit abc)"})
	clean := Gitleaks(context.Background(), workspace(t, script{GitleaksBinary: {run: writeReport("[]")}}))
	testutil.Equal(t, clean.Status, gate.StatusPass)
	testutil.Contains(t, Gitleaks(context.Background(), workspace(t, script{GitleaksBinary: {err: errExit}})).Reason, GitleaksBinary+" git")
	testutil.Contains(t, Gitleaks(context.Background(), workspace(t, script{GitleaksBinary: ok("")})).Reason, "report unreadable: open ")
	testutil.Contains(t, Gitleaks(context.Background(), workspace(t, script{GitleaksBinary: {run: writeReport("{")}})).Reason, "report unreadable")
}

func TestTrufflehog(t *testing.T) {
	line := `{"DetectorName":"Github","Verified":true,"SourceMetadata":{"Data":{"Git":{"file":"a.go","line":7,"commit":"abcdef0123456"}}}}`
	out := Trufflehog(context.Background(), workspace(t, script{TrufflehogBinary + " git": {res: procexec.Result{Stdout: line + "\n\n"}, err: errExit}}))
	testutil.Equal(t, out.Findings, []string{"Github a.go:7 (commit abcdef01) verified=true"})
	testutil.Equal(t, Trufflehog(context.Background(), workspace(t, script{TrufflehogBinary: ok("")})).Status, gate.StatusPass)
	testutil.Contains(t, Trufflehog(context.Background(), workspace(t, script{TrufflehogBinary: ok("not json")})).Reason, "unreadable")
	long := `{"DetectorName":"` + strings.Repeat("D", bufio.MaxScanTokenSize) + `"}`
	found, err := parseTrufflehog(long)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(found), 1)
	_, err = parseTrufflehog(strings.Repeat("x", maxSecretLine+1))
	testutil.Is(t, err, bufio.ErrTooLong)
	w := workspace(t, script{TrufflehogBinary: ok("")})
	Trufflehog(context.Background(), w)
	testutil.Contains(t, procexectest.LinesOf(w.Exec)[0], "trufflehog git file://"+w.Src+" --results=verified,unverified,unknown")
}

func TestSAST(t *testing.T) {
	w := workspace(t, nil)
	testutil.Write(t, filepath.Join(w.Src, "main.go"), "package main\n")
	testutil.Write(t, filepath.Join(w.Src, ".semgrepignore"), "*.go\n")
	report := `{"results":[{"check_id":"go.exec","path":"/src/main.go","start":{"line":3},"extra":{"severity":"ERROR"}}],"errors":[{"message":"timeout","path":"/src/big.go"}],"paths":{"scanned":["/src/main.go"]}}`
	var staged string
	w.Exec = &procexectest.Fake{Handler: script{
		gitList: ok("main.go\x00.semgrepignore\x00"),
		dockerRun + " --rm -v": {res: procexec.Result{Stdout: report}, err: errExit, run: func(procexec.Command) {
			staged = testutil.Read(t, filepath.Join(w.Report(semgrepCopy), semgrepIgnore))
		}},
	}.handle}
	out := SAST(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{"ERROR go.exec main.go:3", "semgrep could not analyse /src/big.go: timeout"})
	testutil.Equal(t, out.Values[ValueSASTScanned], float64(1))
	testutil.Equal(t, staged, "")
	call := procexectest.LinesOf(w.Exec)[1]
	testutil.Contains(t, call, w.Report(semgrepCopy)+":/src:ro", "semgrep/semgrep@sha256:abc semgrep scan", "--config p/default --config p/golang /src")
	clean := workspace(t, script{gitList: ok(""), dockerRun: ok(`{"results":[],"errors":[],"paths":{"scanned":["a"]}}`)})
	testutil.Equal(t, SAST(context.Background(), clean).Status, gate.StatusPass)
	crashed := workspace(t, script{gitList: ok(""), dockerRun: {res: procexec.Result{Stderr: "pull denied"}, err: errExit}})
	testutil.Contains(t, SAST(context.Background(), crashed).Reason, "unreadable")
	testutil.Contains(t, SAST(context.Background(), workspace(t, script{gitList: {err: errExit}})).Reason, "cannot stage sources")
	toolErr := workspace(t, script{gitList: ok(""), dockerRun: {res: procexec.Result{Stdout: `{"results":[]}`}, err: errExit}})
	testutil.Contains(t, SAST(context.Background(), toolErr).Reason, "tool failed")
}

func TestSASTStagingFailures(t *testing.T) {
	w := workspace(t, script{gitList: ok("")})
	blocker := filepath.Join(t.TempDir(), "file")
	testutil.Write(t, blocker, "")
	w.Out = blocker
	testutil.Equal(t, SAST(context.Background(), w).Status, gate.StatusFail)
	ignoreBlocked := workspace(t, script{gitList: ok("")})
	ignoreBlocked.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		testutil.Mkdir(t, filepath.Join(ignoreBlocked.Report(semgrepCopy), semgrepIgnore))
		return procexec.Result{}, nil
	}}
	testutil.Equal(t, SAST(context.Background(), ignoreBlocked).Status, gate.StatusFail)
}

func TestSCA(t *testing.T) {
	report := `{"results":[{"source":{"path":"/r/go.mod"},"packages":[{"package":{"name":"golang.org/x/net","version":"0.1.0"},"vulnerabilities":[{"id":"GO-1"},{"id":"GHSA-x"}]}]}]}`
	out := SCA(context.Background(), workspace(t, script{OSVBinary + " scan source": {res: procexec.Result{Stdout: report}, err: errExit}}))
	testutil.Equal(t, out.Findings, []string{"GO-1 golang.org/x/net@0.1.0 (/r/go.mod)", "GHSA-x golang.org/x/net@0.1.0 (/r/go.mod)"})
	testutil.Equal(t, SCA(context.Background(), workspace(t, script{OSVBinary: ok(`{"results":[]}`)})).Status, gate.StatusPass)
	testutil.Contains(t, SCA(context.Background(), workspace(t, script{OSVBinary: {err: errExit}})).Reason, "unreadable")
}

func TestWorkflows(t *testing.T) {
	ziz := `[{"ident":"excessive-permissions","determinations":{"severity":"Medium"},"locations":[{"symbolic":{"key":{"Local":{"verbatim_path":"SRC/.github/workflows/ci.yml"}}},"concrete":{"location":{"start_point":{"row":4}}}}]},{"ident":"x","determinations":{"severity":"Low"},"locations":[]}]`
	w := workspace(t, nil)
	w.Exec = &procexectest.Fake{Handler: script{
		ZizmorBinary:     {res: procexec.Result{Stdout: strings.ReplaceAll(ziz, "SRC", w.Src)}, err: errExit},
		ActionlintBinary: {res: procexec.Result{Stdout: `[{"message":"bad expr","filepath":"ci.yml","line":9,"kind":"expression"}]`}, err: errExit},
	}.handle}
	out := Workflows(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{"Medium excessive-permissions .github/workflows/ci.yml:5", "Low x :0", "ci.yml:9: bad expr (expression)"})
	testutil.Contains(t, procexectest.LinesOf(w.Exec)[0], "--persona=auditor", "--config /policy/workflow-audit.yml")
	clean := workspace(t, script{ZizmorBinary: ok("[]"), ActionlintBinary: ok("[]")})
	testutil.Equal(t, Workflows(context.Background(), clean).Status, gate.StatusPass)
	testutil.Contains(t, Workflows(context.Background(), workspace(t, script{ZizmorBinary: {err: errExit}})).Reason, "unreadable")
	lintCrash := workspace(t, script{ZizmorBinary: ok("[]"), ActionlintBinary: {res: procexec.Result{Stderr: "no project"}, err: errExit}})
	testutil.Contains(t, Workflows(context.Background(), lintCrash).Reason, "unreadable")
	lintErr := workspace(t, script{ZizmorBinary: ok("[]"), ActionlintBinary: {res: procexec.Result{Stdout: "[]"}, err: errExit}})
	testutil.Contains(t, Workflows(context.Background(), lintErr).Reason, "tool failed")
	lintGarbage := workspace(t, script{ZizmorBinary: ok(`[{"ident":"a","locations":[]}]`), ActionlintBinary: ok("x")})
	garbled := Workflows(context.Background(), lintGarbage)
	testutil.Equal(t, garbled.Status, gate.StatusFail)
	testutil.Equal(t, garbled.Findings, []string{" a :0"})
	onlyGarbage := workspace(t, script{ZizmorBinary: ok("[]"), ActionlintBinary: ok("x")})
	testutil.Contains(t, Workflows(context.Background(), onlyGarbage).Reason, "unreadable")
}

func TestDockerfiles(t *testing.T) {
	report := `[{"code":"DL3007","message":"latest tag","file":"Dockerfile","line":1,"level":"warning"}]`
	w := workspace(t, script{gitList: ok("Dockerfile\x00svc/build.Dockerfile\x00main.go\x00"), HadolintBinary: ok(report)})
	out := Dockerfiles(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{"Dockerfile:1: DL3007 latest tag (warning)"})
	testutil.Equal(t, procexectest.LinesOf(w.Exec)[1], "hadolint --no-fail --format json Dockerfile")
	testutil.Equal(t, Dockerfiles(context.Background(), workspace(t, script{gitList: ok("main.go\x00")})).Reason, reasonNoDocker)
	testutil.Equal(t, Dockerfiles(context.Background(), workspace(t, script{gitList: {err: errExit}})).Status, gate.StatusFail)
	clean := workspace(t, script{gitList: ok("Dockerfile.prod\x00"), HadolintBinary: ok("[]")})
	testutil.Equal(t, Dockerfiles(context.Background(), clean).Status, gate.StatusPass)
}

const trivyFixture = `{"Results":[{"Target":"Dockerfile","Misconfigurations":[{"ID":"DS002","Title":"root user","Severity":"HIGH","CauseMetadata":{"StartLine":1}}]},
{"Target":"app (alpine)","Vulnerabilities":[{"VulnerabilityID":"CVE-1","PkgName":"libc","InstalledVersion":"1.0","Severity":"LOW"}],"Secrets":[{"RuleID":"aws","Severity":"CRITICAL","StartLine":2}]}]}`

func TestIaCAndImage(t *testing.T) {
	w := workspace(t, script{TrivyBinary + " config": ok(trivyFixture)})
	out := IaC(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{"HIGH DS002 Dockerfile:1: root user", "CVE-1 libc@1.0 (app (alpine) LOW)", "CRITICAL aws app (alpine):2: secret"})
	testutil.Contains(t, procexectest.LinesOf(w.Exec)[0], "--severity UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL", w.Src)
	testutil.Equal(t, Image(context.Background(), workspace(t, nil)).Reason, reasonNoImage)
	img := workspace(t, script{TrivyBinary + " image": ok(`{"Results":[]}`), dockerSave: {run: saveImage}})
	img.Image = gateImage
	testutil.Equal(t, Image(context.Background(), img).Status, gate.StatusPass)
	lines := procexectest.LinesOf(img.Exec)
	testutil.Equal(t, lines[0], "docker save -o "+img.Report(ImageArchive)+" "+gateImage)
	testutil.Contains(t, lines[1], "--scanners vuln,secret,misconfig", "--input "+img.Report(ImageArchive))
	testutil.Equal(t, Image(context.Background(), img).Status, gate.StatusPass)
	testutil.Equal(t, len(procexectest.LinesOf(img.Exec)), 3)
	noSave := workspace(t, script{dockerSave: {err: errExit}})
	noSave.Image = gateImage
	testutil.Contains(t, Image(context.Background(), noSave).Reason, "docker save "+gateImage)
	testutil.Contains(t, IaC(context.Background(), workspace(t, script{TrivyBinary: ok("{")})).Reason, "unreadable")
}

func saveImage(c procexec.Command) {
	_ = os.WriteFile(c.Args[2], []byte("tar"), 0o600)
}

func writeSBOM(content string) func(c procexec.Command) {
	return func(c procexec.Command) {
		for _, a := range c.Args {
			if path, ok := strings.CutPrefix(a, cycloneOutput); ok {
				_ = os.WriteFile(path, []byte(content), 0o600)
			}
		}
	}
}

func TestSBOMAndLicenses(t *testing.T) {
	source := `{"components":[
{"name":"self","purl":"pkg:golang/example.com/app@v0.0.0"},
{"name":"self-binary","purl":"pkg:golang/example.com/app"},
{"name":"lookalike","purl":"pkg:golang/example.com/application@v1","licenses":[{"license":{"id":"MIT"}}]},
{"name":"yaml","purl":"pkg:golang/go.yaml.in/yaml/v3@v3.0.5","licenses":[{"license":{"id":"Apache-2.0"}},{"license":{"id":"MIT"}}]},
{"name":"yaml","purl":"pkg:golang/go.yaml.in/yaml/v3@v3.0.5","licenses":[{"license":{"id":"Apache-2.0"}}]},
{"name":"gpl","purl":"pkg:golang/gpl.example/lib@v1","licenses":[{"license":{"id":"GPL-3.0-only"}}]},
{"name":"either","purl":"pkg:golang/either.example/lib@v1","licenses":[{"expression":"(GPL-2.0-only OR MIT)"}]},
{"name":"both","purl":"pkg:golang/both.example/lib@v1","licenses":[{"expression":"MIT AND GPL-2.0-only"}]},
{"name":"named","purl":"pkg:npm/named@1","licenses":[{"license":{"name":"BSD-3-Clause"}}]},
{"name":"none","purl":"pkg:pypi/none@1"},
{"name":"action","purl":"pkg:github/actions/checkout@v4"},
{"name":"os","purl":"pkg:deb/debian/libc@1"}]}`
	w := workspace(t, script{SyftBinary + " scan dir:": {run: writeSBOM(source)}, SyftBinary + " scan docker-archive:": {run: writeSBOM(`{"components":[{"name":"busybox","purl":"pkg:apk/busybox@1"}]}`)}, dockerSave: {run: saveImage}})
	w.Image = gateImage
	out := SBOM(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{
		"pkg:golang/gpl.example/lib@v1: license GPL-3.0-only is not approved",
		"pkg:golang/both.example/lib@v1: license MIT AND GPL-2.0-only is not approved",
		"pkg:pypi/none@1: license unknown",
	})
	testutil.Equal(t, out.Values[ValueComponents], float64(13))
	testutil.Contains(t, testutil.Read(t, w.Report(SBOMImage)), "busybox")
	calls := procexectest.CallsOf(w.Exec)
	testutil.Equal(t, calls[0].Name, DockerBinary)
	testutil.Equal(t, calls[1].Env, syftLicenseEnv)
	unsaved := workspace(t, script{SyftBinary: {run: writeSBOM(source)}, dockerSave: {err: errExit}})
	unsaved.Image = gateImage
	testutil.Contains(t, SBOM(context.Background(), unsaved).Reason, "docker save")
	empty := workspace(t, script{SyftBinary: {run: writeSBOM(`{"components":[]}`)}})
	testutil.Equal(t, SBOM(context.Background(), empty).Reason, reasonEmptySBOM)
	testutil.Contains(t, SBOM(context.Background(), workspace(t, script{SyftBinary: {err: errExit}})).Reason, "syft dir:")
	testutil.Equal(t, SBOM(context.Background(), workspace(t, script{SyftBinary: ok("")})).Status, gate.StatusFail)
	testutil.Contains(t, SBOM(context.Background(), workspace(t, script{SyftBinary: {run: writeSBOM("{")}})).Reason, "SBOM unreadable")
	clean := workspace(t, script{SyftBinary: {run: writeSBOM(`{"components":[{"purl":"pkg:golang/a@v1","licenses":[{"license":{"id":"MIT"}}]}]}`)}})
	clean.Module = ""
	testutil.Equal(t, SBOM(context.Background(), clean).Status, gate.StatusPass)
}
