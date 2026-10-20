package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
)

const (
	DockerBinary     = "docker"
	semgrepCopy      = "semgrep-src"
	semgrepIgnore    = ".semgrepignore"
	semgrepMount     = "/src"
	reasonSAST       = "static analysis found vulnerabilities"
	sastFormat       = "%s %s %s:%d"
	sastErrorFormat  = "semgrep could not analyse %s: %s"
	ignorePerm       = 0o600
	semgrepRoFormat  = "%s:%s:ro"
	ValueSASTScanned = "files_scanned"
)

type semgrepReport struct {
	Results []struct {
		CheckID string `json:"check_id"`
		Path    string `json:"path"`
		Start   struct {
			Line int `json:"line"`
		} `json:"start"`
		Extra struct {
			Severity string `json:"severity"`
		} `json:"extra"`
	} `json:"results"`
	Errors []struct {
		Message string `json:"message"`
		Path    string `json:"path"`
	} `json:"errors"`
	Paths struct {
		Scanned []string `json:"scanned"`
	} `json:"paths"`
}

func SAST(ctx context.Context, ws gate.Workspace) gate.Outcome {
	copyDir := ws.Report(semgrepCopy)
	if err := os.RemoveAll(copyDir); err != nil {
		return gate.Fail(err.Error())
	}
	if err := source.CopyTracked(ctx, ws, copyDir); err != nil {
		return gate.Fail("cannot stage sources: " + err.Error())
	}
	if err := os.WriteFile(filepath.Join(copyDir, semgrepIgnore), nil, ignorePerm); err != nil {
		return gate.Fail(err.Error())
	}
	args := []string{"run", "--rm", "-v", fmt.Sprintf(semgrepRoFormat, copyDir, semgrepMount), "-w", semgrepMount, ws.Policy.Images.Semgrep,
		"semgrep", "scan", "--metrics=off", "--json", flagQuiet, "--disable-version-check"}
	for _, r := range ws.Policy.SASTRules {
		args = append(args, "--config", r)
	}
	res, err := ws.Run(ctx, DockerBinary, append(args, semgrepMount)...)
	var rep semgrepReport
	if perr := json.Unmarshal([]byte(res.Stdout), &rep); perr != nil {
		return gate.FromTool(reasonSAST, res, err, nil, perr)
	}
	findings := make([]string, 0, len(rep.Results)+len(rep.Errors))
	for _, r := range rep.Results {
		findings = append(findings, fmt.Sprintf(sastFormat, r.Extra.Severity, r.CheckID, strings.TrimPrefix(r.Path, semgrepMount+"/"), r.Start.Line))
	}
	for _, e := range rep.Errors {
		findings = append(findings, fmt.Sprintf(sastErrorFormat, e.Path, e.Message))
	}
	out := gate.FromTool(reasonSAST, res, err, findings, nil)
	if out.Values == nil {
		out.Values = map[string]float64{}
	}
	out.Values[ValueSASTScanned] = float64(len(rep.Paths.Scanned))
	return out
}
