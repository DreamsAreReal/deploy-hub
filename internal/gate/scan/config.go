package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
)

const (
	ZizmorBinary          = "zizmor"
	ActionlintBinary      = "actionlint"
	HadolintBinary        = "hadolint"
	TrivyBinary           = "trivy"
	githubDir             = ".github"
	zizmorConfig          = "workflow-audit.yml"
	dockerfileGlob        = "Dockerfile*"
	reasonWorkflows       = "CI/CD workflows have security or correctness findings"
	reasonDockerfile      = "Dockerfiles violate the lint rules"
	reasonIaC             = "infrastructure configuration has misconfigurations"
	reasonDependencyVulns = "dependencies have known vulnerabilities"
	reasonNoDocker        = "the app ships an image but the repository has no Dockerfile"
	zizmorFormat          = "%s %s %s:%d"
	actionlintFormat      = "%s:%d: %s (%s)"
	hadolintFormat        = "%s:%d: %s %s (%s)"
	misconfFormat         = "%s %s %s:%d: %s"
	trivySeverities       = "UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL"
	flagFormat            = "--format"
	flagQuiet             = "--quiet"
	formatJSON            = "json"
)

type zizmorFinding struct {
	Ident          string `json:"ident"`
	Determinations struct {
		Severity string `json:"severity"`
	} `json:"determinations"`
	Locations []struct {
		Symbolic struct {
			Key struct {
				Local struct {
					Path string `json:"verbatim_path"`
				} `json:"Local"`
			} `json:"key"`
		} `json:"symbolic"`
		Concrete struct {
			Location struct {
				Start struct {
					Row int `json:"row"`
				} `json:"start_point"`
			} `json:"location"`
		} `json:"concrete"`
	} `json:"locations"`
}

func Workflows(ctx context.Context, ws gate.Workspace) gate.Outcome {
	dir := filepath.Join(ws.Src, filepath.FromSlash(githubDir))
	zres, zerr := ws.Run(ctx, ZizmorBinary, "--persona=auditor", "--format=json", "--no-progress", "--config", filepath.Join(ws.PolicyDir, zizmorConfig), dir)
	var zf []zizmorFinding
	if err := json.Unmarshal([]byte(zres.Stdout), &zf); err != nil {
		return gate.FromTool(reasonWorkflows, zres, zerr, nil, err)
	}
	findings := make([]string, 0, len(zf))
	for _, f := range zf {
		file, row := "", 0
		if len(f.Locations) > 0 {
			file, row = ws.Rel(f.Locations[0].Symbolic.Key.Local.Path), f.Locations[0].Concrete.Location.Start.Row+1
		}
		findings = append(findings, fmt.Sprintf(zizmorFormat, f.Determinations.Severity, f.Ident, file, row))
	}
	ares, aerr := ws.Run(ctx, ActionlintBinary, "-format", "{{json .}}")
	more, perr := parseActionlint(ares.Stdout)
	findings = append(findings, more...)
	if len(findings) == 0 && aerr != nil {
		return gate.FromTool(reasonWorkflows, ares, aerr, nil, perr)
	}
	return gate.FromTool(reasonWorkflows, zres, nil, findings, perr)
}

func parseActionlint(out string) ([]string, error) {
	var errs []struct {
		Message  string `json:"message"`
		Filepath string `json:"filepath"`
		Line     int    `json:"line"`
		Kind     string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(out), &errs); err != nil {
		return nil, err
	}
	findings := make([]string, 0, len(errs))
	for _, e := range errs {
		findings = append(findings, fmt.Sprintf(actionlintFormat, e.Filepath, e.Line, e.Message, e.Kind))
	}
	return findings, nil
}

func Dockerfiles(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := source.Tracked(ctx, ws.Exec, ws.Src)
	if err != nil {
		return gate.Fail(err.Error())
	}
	var dockerfiles []string
	for _, f := range files {
		if ok, _ := path.Match(dockerfileGlob, path.Base(f)); ok {
			dockerfiles = append(dockerfiles, f)
		}
	}
	if len(dockerfiles) == 0 {
		return gate.Fail(reasonNoDocker)
	}
	res, err := ws.Run(ctx, HadolintBinary, append([]string{"--no-fail", flagFormat, formatJSON}, dockerfiles...)...)
	var issues []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		File    string `json:"file"`
		Line    int    `json:"line"`
		Level   string `json:"level"`
	}
	perr := json.Unmarshal([]byte(res.Stdout), &issues)
	findings := make([]string, 0, len(issues))
	for _, i := range issues {
		findings = append(findings, fmt.Sprintf(hadolintFormat, i.File, i.Line, i.Code, i.Message, i.Level))
	}
	return gate.FromTool(reasonDockerfile, res, err, findings, perr)
}

type trivyReport struct {
	Results []struct {
		Target            string `json:"Target"`
		Misconfigurations []struct {
			ID            string `json:"ID"`
			Title         string `json:"Title"`
			Severity      string `json:"Severity"`
			CauseMetadata struct {
				StartLine int `json:"StartLine"`
			} `json:"CauseMetadata"`
		} `json:"Misconfigurations"`
		Vulnerabilities []struct {
			VulnerabilityID  string `json:"VulnerabilityID"`
			PkgName          string `json:"PkgName"`
			InstalledVersion string `json:"InstalledVersion"`
			Severity         string `json:"Severity"`
		} `json:"Vulnerabilities"`
		Secrets []struct {
			RuleID    string `json:"RuleID"`
			Severity  string `json:"Severity"`
			StartLine int    `json:"StartLine"`
		} `json:"Secrets"`
	} `json:"Results"`
}

func (r trivyReport) findings() []string {
	var out []string
	for _, res := range r.Results {
		for _, m := range res.Misconfigurations {
			out = append(out, fmt.Sprintf(misconfFormat, m.Severity, m.ID, res.Target, m.CauseMetadata.StartLine, m.Title))
		}
		for _, v := range res.Vulnerabilities {
			out = append(out, fmt.Sprintf(osvFormat, v.VulnerabilityID, v.PkgName, v.InstalledVersion, res.Target+" "+v.Severity))
		}
		for _, s := range res.Secrets {
			out = append(out, fmt.Sprintf(misconfFormat, s.Severity, s.RuleID, res.Target, s.StartLine, "secret"))
		}
	}
	return out
}

func trivy(ctx context.Context, ws gate.Workspace, reason, sub string, args ...string) gate.Outcome {
	full := append([]string{sub, flagQuiet, flagFormat, formatJSON, "--exit-code", "0", "--severity", trivySeverities}, args...)
	res, err := ws.Run(ctx, TrivyBinary, full...)
	var rep trivyReport
	perr := json.Unmarshal([]byte(res.Stdout), &rep)
	return gate.FromTool(reason, res, err, rep.findings(), perr)
}

func IaC(ctx context.Context, ws gate.Workspace) gate.Outcome {
	return trivy(ctx, ws, reasonIaC, "config", ws.Src)
}

func DependencyVulnerabilities(ctx context.Context, ws gate.Workspace) gate.Outcome {
	return trivy(ctx, ws, reasonDependencyVulns, "fs", "--scanners", "vuln", "--include-dev-deps", ws.Src)
}
