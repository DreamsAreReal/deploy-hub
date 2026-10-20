package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	OSVBinary        = "osv-scanner"
	reasonSCA        = "dependencies have known vulnerabilities"
	osvFormat        = "%s %s@%s (%s)"
	osvNoResult      = 128
	osvNoSources     = "No package sources found"
	reasonNoPackages = "the repository has no package manifests or lockfiles to scan"
)

type osvReport struct {
	Results []struct {
		Source struct {
			Path string `json:"path"`
		} `json:"source"`
		Packages []struct {
			Package struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"package"`
			Vulnerabilities []struct {
				ID string `json:"id"`
			} `json:"vulnerabilities"`
		} `json:"packages"`
	} `json:"results"`
}

func SCA(ctx context.Context, ws gate.Workspace) gate.Outcome {
	res, err := ws.Run(ctx, OSVBinary, "scan", "source", "--recursive", flagFormat, formatJSON, ws.Src)
	if strings.Contains(res.Stderr, osvNoSources) {
		return gate.Outcome{Status: gate.StatusNA, Reason: reasonNoPackages}
	}
	var rep osvReport
	perr := json.Unmarshal([]byte(res.Stdout), &rep)
	var findings []string
	for _, r := range rep.Results {
		for _, p := range r.Packages {
			for _, v := range p.Vulnerabilities {
				findings = append(findings, fmt.Sprintf(osvFormat, v.ID, p.Package.Name, p.Package.Version, r.Source.Path))
			}
		}
	}
	return gate.FromTool(reasonSCA, res, err, findings, perr)
}
