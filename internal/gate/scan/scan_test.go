package scan

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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
	gitBinary  = "git"
	dockerSave = "docker save"
	gitList    = "git -C"
	dockerRun  = "docker run"
	gateImage  = "app:gate"
)

type (
	reply  = procexectest.Reply
	script = procexectest.Script
)

func ok(stdout string) reply { return reply{Res: procexec.Result{Stdout: stdout}} }

func workspace(t *testing.T, s script) gate.Workspace {
	t.Helper()
	src := t.TempDir()
	testutil.Mkdir(t, filepath.Join(src, ".github"))
	return gate.Workspace{
		Src: src, Out: t.TempDir(), PolicyDir: "/policy", Module: "example.com/app",
		Exec: &procexectest.Fake{Handler: s.Handle}, Log: slog.New(slog.DiscardHandler),
		Policy: policy.Policy{
			Licenses: []string{mit, apache2, bsd3}, SASTRules: []string{"p/default", "p/golang"},
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
		GitleaksBinary + " git": {Run: writeReport(leak)},
		GitleaksBinary + " dir": {Run: writeReport(`[{"RuleID":"jwt","File":"x.env","StartLine":1,"Commit":"abc"}]`)},
	}))
	testutil.Equal(t, out.Findings, []string{"aws-access-token cfg.yml:3 (commit 01234567)", "jwt x.env:1 (commit abc)"})
	clean := Gitleaks(context.Background(), workspace(t, script{GitleaksBinary: {Run: writeReport("[]")}}))
	testutil.Equal(t, clean.Status, gate.StatusPass)
	testutil.Contains(t, Gitleaks(context.Background(), workspace(t, script{GitleaksBinary: {Err: errExit}})).Reason, GitleaksBinary+" git")
	testutil.Contains(t, Gitleaks(context.Background(), workspace(t, script{GitleaksBinary: ok("")})).Reason, "report unreadable: open ")
	testutil.Contains(t, Gitleaks(context.Background(), workspace(t, script{GitleaksBinary: {Run: writeReport("{")}})).Reason, "report unreadable")
}

func TestTrufflehog(t *testing.T) {
	line := `{"DetectorName":"Github","Verified":true,"SourceMetadata":{"Data":{"Git":{"file":"a.go","line":7,"commit":"abcdef0123456"}}}}`
	out := Trufflehog(context.Background(), workspace(t, script{TrufflehogBinary + " git": {Res: procexec.Result{Stdout: line + "\n\n"}, Err: errExit}}))
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
		dockerRun + " --rm -v": {Res: procexec.Result{Stdout: report}, Err: errExit, Run: func(procexec.Command) {
			staged = testutil.Read(t, filepath.Join(w.Report(semgrepCopy), semgrepIgnore))
		}},
	}.Handle}
	out := SAST(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{"ERROR go.exec main.go:3", "semgrep could not analyse /src/big.go: timeout"})
	testutil.Equal(t, out.Values[ValueSASTScanned], float64(1))
	testutil.Equal(t, staged, "")
	call := procexectest.LinesOf(w.Exec)[1]
	testutil.Contains(t, call, w.Report(semgrepCopy)+":/src:ro", "semgrep/semgrep@sha256:abc semgrep scan", "--config p/default --config p/golang /src")
	clean := workspace(t, script{gitList: ok(""), dockerRun: ok(`{"results":[],"errors":[],"paths":{"scanned":["a"]}}`)})
	testutil.Equal(t, SAST(context.Background(), clean).Status, gate.StatusPass)
	crashed := workspace(t, script{gitList: ok(""), dockerRun: {Res: procexec.Result{Stderr: "pull denied"}, Err: errExit}})
	testutil.Contains(t, SAST(context.Background(), crashed).Reason, "unreadable")
	testutil.Contains(t, SAST(context.Background(), workspace(t, script{gitList: {Err: errExit}})).Reason, "cannot stage sources")
	toolErr := workspace(t, script{gitList: ok(""), dockerRun: {Res: procexec.Result{Stdout: `{"results":[]}`}, Err: errExit}})
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
	out := SCA(context.Background(), workspace(t, script{OSVBinary + " scan source": {Res: procexec.Result{Stdout: report}, Err: errExit}}))
	testutil.Equal(t, out.Findings, []string{"GO-1 golang.org/x/net@0.1.0 (/r/go.mod)", "GHSA-x golang.org/x/net@0.1.0 (/r/go.mod)"})
	testutil.Equal(t, SCA(context.Background(), workspace(t, script{OSVBinary: ok(`{"results":[]}`)})).Status, gate.StatusPass)
	testutil.Contains(t, SCA(context.Background(), workspace(t, script{OSVBinary: {Err: errExit}})).Reason, "unreadable")
}

func TestWorkflows(t *testing.T) {
	ziz := `[{"ident":"excessive-permissions","determinations":{"severity":"Medium"},"locations":[{"symbolic":{"key":{"Local":{"verbatim_path":"SRC/.github/workflows/ci.yml"}}},"concrete":{"location":{"start_point":{"row":4}}}}]},{"ident":"x","determinations":{"severity":"Low"},"locations":[]}]`
	w := workspace(t, nil)
	w.Exec = &procexectest.Fake{Handler: script{
		ZizmorBinary:     {Res: procexec.Result{Stdout: strings.ReplaceAll(ziz, "SRC", w.Src)}, Err: errExit},
		ActionlintBinary: {Res: procexec.Result{Stdout: `[{"message":"bad expr","filepath":"ci.yml","line":9,"kind":"expression"}]`}, Err: errExit},
	}.Handle}
	out := Workflows(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{"Medium excessive-permissions .github/workflows/ci.yml:5", "Low x :0", "ci.yml:9: bad expr (expression)"})
	testutil.Contains(t, procexectest.LinesOf(w.Exec)[0], "--persona=auditor", "--config /policy/workflow-audit.yml")
	clean := workspace(t, script{ZizmorBinary: ok("[]"), ActionlintBinary: ok("[]")})
	testutil.Equal(t, Workflows(context.Background(), clean).Status, gate.StatusPass)
	testutil.Contains(t, Workflows(context.Background(), workspace(t, script{ZizmorBinary: {Err: errExit}})).Reason, "unreadable")
	lintCrash := workspace(t, script{ZizmorBinary: ok("[]"), ActionlintBinary: {Res: procexec.Result{Stderr: "no project"}, Err: errExit}})
	testutil.Contains(t, Workflows(context.Background(), lintCrash).Reason, "unreadable")
	lintErr := workspace(t, script{ZizmorBinary: ok("[]"), ActionlintBinary: {Res: procexec.Result{Stdout: "[]"}, Err: errExit}})
	testutil.Contains(t, Workflows(context.Background(), lintErr).Reason, "tool failed")
	bare := workspace(t, nil)
	testutil.NoErr(t, os.Remove(filepath.Join(bare.Src, ".github")))
	none := Workflows(context.Background(), bare)
	testutil.Equal(t, none.Status, gate.StatusNA)
	testutil.Equal(t, none.Reason, reasonNoWorkflows)
	lintGarbage := workspace(t, script{ZizmorBinary: ok(`[{"ident":"a","locations":[]}]`), ActionlintBinary: ok("x")})
	garbled := Workflows(context.Background(), lintGarbage)
	testutil.Equal(t, garbled.Status, gate.StatusFail)
	testutil.Equal(t, garbled.Findings, []string{" a :0"})
	onlyGarbage := workspace(t, script{ZizmorBinary: ok("[]"), ActionlintBinary: ok("x")})
	testutil.Contains(t, Workflows(context.Background(), onlyGarbage).Reason, "unreadable")
}

func TestWorkflowsAcceptedFinding(t *testing.T) {
	const (
		deployPath  = ".github/workflows/deploy.yml"
		selfHosted  = "self-hosted-runner"
		medium      = "Medium"
		staleRunsOn = "accepted Medium " + selfHosted + " " + deployPath + " at runs-on is no longer reported"
		deployYML   = "jobs:\n  deploy:\n    runs-on: [self-hosted]\n    timeout-minutes: 5\n  other:\n    runs-on: [self-hosted]\n"
	)
	hit := func(rule, severity, file string, row int) string {
		return `{"ident":"` + rule + `","determinations":{"severity":"` + severity + `"},"locations":[{"symbolic":{"key":{"Local":{"verbatim_path":"SRC/` + file + `"}}},"concrete":{"location":{"start_point":{"row":` + fmt.Sprint(row) + `}}}}]}`
	}
	reported := func(severity, rule, file string, line int) string {
		return fmt.Sprintf(zizmorFormat, severity, rule, file, line)
	}
	accepted := policy.Accepted{Rule: selfHosted, Severity: medium, File: deployPath, Key: "runs-on", Reason: "#600"}
	run := func(t *testing.T, accept []policy.Accepted, hits ...string) gate.Outcome {
		t.Helper()
		w := workspace(t, nil)
		testutil.Mkdir(t, filepath.Join(w.Src, ".github", "workflows"))
		testutil.Write(t, filepath.Join(w.Src, filepath.FromSlash(deployPath)), deployYML)
		w.App.Accepted = accept
		w.Exec = &procexectest.Fake{Handler: script{
			ZizmorBinary:     ok(strings.ReplaceAll("["+strings.Join(hits, ",")+"]", "SRC", w.Src)),
			ActionlintBinary: ok("[]"),
		}.Handle}
		return Workflows(context.Background(), w)
	}
	only := []policy.Accepted{accepted}
	runsOn := hit(selfHosted, medium, deployPath, 2)
	passed := run(t, only, runsOn)
	testutil.Equal(t, passed.Status, gate.StatusPass)
	testutil.Equal(t, passed.Values, map[string]float64{gate.ValueFindings: 0, valueAccepted: 1})
	cases := map[string]struct {
		accept   []policy.Accepted
		hits     []string
		findings []string
	}{
		"not accepted":   {nil, []string{runsOn}, []string{reported(medium, selfHosted, deployPath, 3)}},
		"second runs-on": {only, []string{runsOn, hit(selfHosted, medium, deployPath, 5)}, []string{reported(medium, selfHosted, deployPath, 6)}},
		"other rule":     {only, []string{hit("excessive-permissions", medium, deployPath, 2)}, []string{reported(medium, "excessive-permissions", deployPath, 3), staleRunsOn}},
		"other severity": {only, []string{hit(selfHosted, "High", deployPath, 2)}, []string{reported("High", selfHosted, deployPath, 3), staleRunsOn}},
		"other file":     {only, []string{hit(selfHosted, medium, ".github/workflows/ci.yml", 2)}, []string{reported(medium, selfHosted, ".github/workflows/ci.yml", 3), staleRunsOn}},
		"other key":      {only, []string{hit(selfHosted, medium, deployPath, 3)}, []string{reported(medium, selfHosted, deployPath, 4), staleRunsOn}},
		"past the end":   {only, []string{hit(selfHosted, medium, deployPath, 6)}, []string{reported(medium, selfHosted, deployPath, 7), staleRunsOn}},
		"before start":   {only, []string{hit(selfHosted, medium, deployPath, -1)}, []string{reported(medium, selfHosted, deployPath, 0), staleRunsOn}},
		"stale":          {only, nil, []string{staleRunsOn}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out := run(t, c.accept, c.hits...)
			testutil.Equal(t, out.Status, gate.StatusFail)
			testutil.Equal(t, out.Findings, c.findings)
		})
	}
	missing := accepted
	missing.File = ".github/workflows/gone.yml"
	gone := run(t, []policy.Accepted{missing}, hit(selfHosted, medium, missing.File, 2))
	testutil.Equal(t, gone.Status, gate.StatusFail)
}

func TestDockerfiles(t *testing.T) {
	report := `[{"code":"DL3007","message":"latest tag","file":"Dockerfile","line":1,"level":"warning"}]`
	w := workspace(t, script{gitList: ok("Dockerfile\x00svc/build.Dockerfile\x00main.go\x00"), HadolintBinary: ok(report)})
	out := Dockerfiles(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{"Dockerfile:1: DL3007 latest tag (warning)"})
	testutil.Equal(t, procexectest.LinesOf(w.Exec)[1], "hadolint --no-fail --format json Dockerfile")
	testutil.Equal(t, Dockerfiles(context.Background(), workspace(t, script{gitList: ok("main.go\x00")})).Reason, reasonNoDocker)
	testutil.Equal(t, Dockerfiles(context.Background(), workspace(t, script{gitList: {Err: errExit}})).Status, gate.StatusFail)
	clean := workspace(t, script{gitList: ok("Dockerfile.prod\x00"), HadolintBinary: ok("[]")})
	testutil.Equal(t, Dockerfiles(context.Background(), clean).Status, gate.StatusPass)
}

const trivyFixture = `{"Results":[{"Target":"Dockerfile","Misconfigurations":[{"ID":"DS002","Title":"root user","Severity":"HIGH","CauseMetadata":{"StartLine":1}}]},
{"Target":"app (alpine)","Vulnerabilities":[{"VulnerabilityID":"CVE-1","PkgName":"libc","InstalledVersion":"1.0","Severity":"LOW"}],"Secrets":[{"RuleID":"aws","Severity":"CRITICAL","StartLine":2}]}]}`

func TestLicenseNamesAndDeclaredLicenses(t *testing.T) {
	pol := policy.Policy{Licenses: []string{mit, apache2, bsd3}, Declared: map[string]string{"pkg:pypi/colorama": "BSD-3-Clause", "pkg:pypi/rogue": "GPL-3.0-only"}}
	comps := []component{
		{Purl: "pkg:pypi/sortedcontainers@2.4.0", Licenses: licenses("Apache 2.0")},
		{Purl: "pkg:pypi/colorama@0.4.6"},
		{Purl: "pkg:pypi/rogue@1"},
		{Purl: "pkg:pypi/declared-but-known@1", Licenses: licenses("GPL-2.0-only")},
		{Purl: "pkg:pypi/mystery@1"},
	}
	testutil.Equal(t, licenseFindings(comps, pol, nil), []string{
		"pkg:pypi/rogue@1: license GPL-3.0-only is not approved",
		"pkg:pypi/declared-but-known@1: license GPL-2.0-only is not approved",
		"pkg:pypi/mystery@1: license unknown",
	})
	testutil.Equal(t, canonical(" The MIT License "), "MIT")
	testutil.Equal(t, canonical("3-Clause BSD License"), "BSD-3-Clause")
	testutil.Equal(t, canonical("Custom"), "Custom")
	testutil.Equal(t, purlName("pkg:npm/%40scope/web@2.0.0"), "pkg:npm/%40scope/web")
}

func licenses(names ...string) []licenseEntry {
	out := make([]licenseEntry, len(names))
	for i, n := range names {
		out[i].License.Name = n
	}
	return out
}

func licensesAt(pairs ...string) []licenseEntry {
	out := make([]licenseEntry, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		var e licenseEntry
		e.License.Name, e.License.URL = pairs[i], pairs[i+1]
		out = append(out, e)
	}
	return out
}

func TestGoTestdataAndReviewedLicenseTexts(t *testing.T) {
	notice, logo, single, unknown := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64)
	pol := policy.Policy{
		Licenses:      []string{mit, bsd3, "blessing"},
		ReviewedTexts: map[string]string{notice: "MIT AND blessing", logo: policy.NotALicense, single: mit},
	}
	mod := "file://$GOPATH/pkg/mod/example.org/libc@v1/"
	comps := []component{
		{Purl: "pkg:golang/example.org/libc@v1", Licenses: licensesAt(bsd3, mod+"LICENSE", "GPL-2.0-only", mod+"testdata/crlibm/COPYING", notice, mod+"LICENSE-3RD-PARTY.md")},
		{Purl: "pkg:golang/example.org/memory@v1", Licenses: licensesAt(bsd3, mod+"LICENSE", logo, mod+"LICENSE-LOGO")},
		{Purl: "pkg:golang/example.org/sqlite@v1", Licenses: licensesAt(single, mod+"LICENSE-SQLITE")},
		{Purl: "pkg:golang/example.org/logo-only@v1", Licenses: licensesAt(logo, mod+"LICENSE-LOGO")},
		{Purl: "pkg:golang/example.org/unreviewed@v1", Licenses: licensesAt(unknown, mod+"NOTICE")},
		{Purl: "pkg:golang/example.org/gpl@v1", Licenses: licensesAt("GPL-2.0-only", mod+"src/COPYING")},
		{Purl: "pkg:npm/fixture@1", Licenses: licensesAt("GPL-2.0-only", "file:///node_modules/fixture/testdata/COPYING")},
	}
	testutil.Equal(t, licenseFindings(comps, pol, nil), []string{
		"pkg:golang/example.org/logo-only@v1: license unknown",
		"pkg:golang/example.org/unreviewed@v1: license " + unknown + " is not approved",
		"pkg:golang/example.org/gpl@v1: license GPL-2.0-only is not approved",
		"pkg:npm/fixture@1: license GPL-2.0-only is not approved",
	})
}

func TestDependencyVulnerabilities(t *testing.T) {
	w := workspace(t, script{TrivyBinary + " fs": ok(trivyFixture)})
	out := DependencyVulnerabilities(context.Background(), w)
	testutil.Equal(t, out.Reason, reasonDependencyVulns)
	testutil.Contains(t, procexectest.LinesOf(w.Exec)[0], "fs --quiet", "--scanners vuln --include-dev-deps "+w.Src)
}

func TestIaCAndImage(t *testing.T) {
	w := workspace(t, script{TrivyBinary + " config": ok(trivyFixture)})
	out := IaC(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{"HIGH DS002 Dockerfile:1: root user", "CVE-1 libc@1.0 (app (alpine) LOW)", "CRITICAL aws app (alpine):2: secret"})
	testutil.Contains(t, procexectest.LinesOf(w.Exec)[0], "--severity UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL", w.Src)
	testutil.Equal(t, Image(context.Background(), workspace(t, nil)).Reason, reasonNoImage)
	img := workspace(t, script{TrivyBinary + " image": ok(`{"Results":[]}`), dockerSave: {Run: saveImage}})
	img.Image = gateImage
	testutil.Equal(t, Image(context.Background(), img).Status, gate.StatusPass)
	lines := procexectest.LinesOf(img.Exec)
	testutil.Equal(t, lines[0], "docker save -o "+img.Report(ImageArchive)+" "+gateImage)
	testutil.Contains(t, lines[1], "--scanners vuln,secret,misconfig", "--input "+img.Report(ImageArchive))
	testutil.Equal(t, Image(context.Background(), img).Status, gate.StatusPass)
	testutil.Equal(t, len(procexectest.LinesOf(img.Exec)), 3)
	noSave := workspace(t, script{dockerSave: {Err: errExit}})
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
{"name":"own-python","purl":"pkg:pypi/my-app@0.1.0"},
{"name":"own-web","purl":"pkg:npm/%40scope/web@2.0.0"},
{"name":"own-template","purl":"pkg:golang/example.com/starter"},
{"name":"action","purl":"pkg:github/actions/checkout@v4"},
{"name":"os","purl":"pkg:deb/debian/libc@1"}]}`
	w := workspace(t, script{
		SyftBinary + " scan dir:": {Run: writeSBOM(source)}, SyftBinary + " scan docker-archive:": {Run: writeSBOM(`{"components":[{"name":"busybox","purl":"pkg:apk/busybox@1"}]}`)},
		dockerSave: {Run: saveImage}, gitBinary: ok("pyproject.toml\x00web/package.json\x00templates/service/go.mod\x00README.md\x00"),
	})
	testutil.Write(t, filepath.Join(w.Src, "pyproject.toml"), "[tool.x]\nname = \"no\"\n\n[project]\nversion = \"0.1.0\"\nname = \"My_App\"\n")
	testutil.Write(t, filepath.Join(w.Src, "web", "package.json"), `{"name":"@scope/web"}`)
	testutil.Write(t, filepath.Join(w.Src, "templates", "service", "go.mod"), "// starter\nmodule \"example.com/starter\" // template\n\ngo 1.24\n")
	testutil.Write(t, filepath.Join(w.Src, "README.md"), "x")
	w.Image = gateImage
	out := SBOM(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{
		"pkg:golang/gpl.example/lib@v1: license GPL-3.0-only is not approved",
		"pkg:golang/both.example/lib@v1: license MIT AND GPL-2.0-only is not approved",
		"pkg:pypi/none@1: license unknown",
	})
	testutil.Equal(t, out.Values[ValueComponents], float64(16))
	testutil.Contains(t, testutil.Read(t, w.Report(SBOMImage)), "busybox")
	fake, isFake := w.Exec.(*procexectest.Fake)
	testutil.True(t, isFake, "fake runner")
	syft, _ := fake.Find(SyftBinary + " scan dir:")
	testutil.Equal(t, syft.Env, syftLicenseEnv)
	testutil.Equal(t, syft.Args[1], dirScheme+w.Report(trackedCopy))
	testutil.Missing(t, w.Report(trackedCopy))
	testutil.Equal(t, manifestPurls("package.json", []byte("{")), []string(nil))
	testutil.Equal(t, manifestPurls("pyproject.toml", []byte("[tool]\n")), []string(nil))
	testutil.Equal(t, manifestPurls("go.mod", []byte("go 1.24\n")), []string(nil))
	unlisted := workspace(t, script{SyftBinary: {Run: writeSBOM(source)}, gitBinary: {Err: errExit}})
	testutil.Equal(t, SBOM(context.Background(), unlisted).Status, gate.StatusFail)
	missing := workspace(t, script{SyftBinary: {Run: writeSBOM(source)}, gitBinary: ok("gone.toml\x00")})
	testutil.Contains(t, SBOM(context.Background(), missing).Reason, "gone.toml")
	unsaved := workspace(t, script{SyftBinary: {Run: writeSBOM(source)}, dockerSave: {Err: errExit}, gitBinary: ok("")})
	unsaved.Image = gateImage
	testutil.Contains(t, SBOM(context.Background(), unsaved).Reason, "docker save")
	empty := workspace(t, script{SyftBinary: {Run: writeSBOM(`{"components":[]}`)}, gitBinary: ok("")})
	testutil.Equal(t, SBOM(context.Background(), empty).Reason, reasonEmptySBOM)
	testutil.Contains(t, SBOM(context.Background(), workspace(t, script{SyftBinary: {Err: errExit}, gitBinary: ok("")})).Reason, "syft dir:")
	testutil.Equal(t, SBOM(context.Background(), workspace(t, script{SyftBinary: ok(""), gitBinary: ok("")})).Status, gate.StatusFail)
	testutil.Contains(t, SBOM(context.Background(), workspace(t, script{SyftBinary: {Run: writeSBOM("{")}, gitBinary: ok("")})).Reason, "SBOM unreadable")
	clean := workspace(t, script{SyftBinary: {Run: writeSBOM(`{"components":[{"purl":"pkg:golang/a@v1","licenses":[{"license":{"id":"MIT"}}]}]}`)}, gitBinary: ok("")})
	clean.Module = ""
	testutil.Equal(t, SBOM(context.Background(), clean).Status, gate.StatusPass)
}
