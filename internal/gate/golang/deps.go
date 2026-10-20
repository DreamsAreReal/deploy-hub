package golang

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	VulncheckBinary   = "govulncheck"
	ValueModules      = "modules"
	ValueVulns        = "vulnerabilities"
	reasonUnapproved  = "dependencies are not approved by the central policy"
	reasonVulns       = "known vulnerabilities in the code or its dependencies"
	reasonSumDB       = "module integrity checks are weakened"
	unapprovedFormat  = "%s is not in the approved dependency list"
	vulnFormat        = "%s in %s (fixed in %s)"
	envFormat         = "%s=%q must not be set"
	sumDBOff          = "off"
	envGOSUMDB        = "GOSUMDB"
	moduleListFormat  = "{{with .Module}}{{.Path}}{{end}}"
	vulncheckFailFrmt = "govulncheck: %v: %s"
)

var integrityEnv = []string{"GONOSUMDB", "GONOSUMCHECK", "GOINSECURE", "GOPRIVATE", "GOFLAGS"}

func Dependencies(ctx context.Context, ws gate.Workspace) gate.Outcome {
	res, err := ws.Run(ctx, GoBinary, "list", "-deps", "-test", "-f", moduleListFormat, allPkgs)
	if err != nil {
		return gate.Fail("go list -deps: " + gate.Tail(procexec.Output(err)))
	}
	approved := map[string]bool{}
	mark(approved, ws.Policy.Dependencies)
	used := usedModules(res.Stdout, ws.Module)
	var findings []string
	for _, m := range used {
		if !approved[m] {
			findings = append(findings, fmt.Sprintf(unapprovedFormat, m))
		}
	}
	values := map[string]float64{ValueModules: float64(len(used)), gate.ValueFindings: float64(len(findings))}
	return gate.Judge(len(findings) == 0, reasonUnapproved, values, findings)
}

func usedModules(out, self string) []string {
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && l != self {
			seen[l] = true
		}
	}
	mods := make([]string, 0, len(seen))
	for m := range seen {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	return mods
}

func Integrity(ctx context.Context, ws gate.Workspace) gate.Outcome {
	keys := append([]string{envGOSUMDB}, integrityEnv...)
	res, err := ws.Run(ctx, GoBinary, append([]string{"env", flagJSON}, keys...)...)
	if err != nil {
		return gate.Fail("go env: " + gate.Tail(procexec.Output(err)))
	}
	env := map[string]string{}
	if err := json.Unmarshal([]byte(res.Stdout), &env); err != nil {
		return gate.Fail("go env report unreadable: " + err.Error())
	}
	var findings []string
	if env[envGOSUMDB] == "" || env[envGOSUMDB] == sumDBOff {
		findings = append(findings, fmt.Sprintf(envFormat, envGOSUMDB, env[envGOSUMDB]))
	}
	for _, k := range integrityEnv {
		if env[k] != "" {
			findings = append(findings, fmt.Sprintf(envFormat, k, env[k]))
		}
	}
	mods := Modules(ctx, ws)
	findings = append(findings, mods.Findings...)
	return gate.Findings(reasonSumDB, findings)
}

type vulnMessage struct {
	Finding *struct {
		OSV          string `json:"osv"`
		FixedVersion string `json:"fixed_version"`
		Trace        []struct {
			Module string `json:"module"`
		} `json:"trace"`
	} `json:"finding"`
}

func Vulnerabilities(ctx context.Context, ws gate.Workspace) gate.Outcome {
	res, err := ws.Run(ctx, VulncheckBinary, "-format", "json", "-test", allPkgs)
	if err != nil {
		return gate.Fail(fmt.Sprintf(vulncheckFailFrmt, err, gate.Tail(procexec.Output(err))))
	}
	findings, err := parseVulns(res.Stdout)
	if err != nil {
		return gate.Fail(err.Error())
	}
	values := map[string]float64{ValueVulns: float64(len(findings))}
	return gate.Judge(len(findings) == 0, reasonVulns, values, findings)
}

func parseVulns(stream string) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(stream)))
	seen := map[string]bool{}
	var out []string
	for {
		var m vulnMessage
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if m.Finding != nil && !seen[m.Finding.OSV] {
			seen[m.Finding.OSV] = true
			out = append(out, m.describe())
		}
	}
}

func (m vulnMessage) describe() string {
	mod := ""
	if len(m.Finding.Trace) > 0 {
		mod = m.Finding.Trace[0].Module
	}
	return fmt.Sprintf(vulnFormat, m.Finding.OSV, mod, m.Finding.FixedVersion)
}
