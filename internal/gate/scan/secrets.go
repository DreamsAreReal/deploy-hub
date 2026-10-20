package scan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	GitleaksBinary   = "gitleaks"
	TrufflehogBinary = "trufflehog"
	gitleaksReport   = "gitleaks-%s.json"
	reasonSecrets    = "secrets found in the repository or its history"
	leakFormat       = "%s %s:%d (commit %s)"
	maxSecretLine    = 1 << 20
	gitScheme        = "file://"
)

type gitleaksFinding struct {
	RuleID    string
	File      string
	StartLine int
	Commit    string
}

func Gitleaks(ctx context.Context, ws gate.Workspace) gate.Outcome {
	var findings []string
	for _, mode := range []string{"git", "dir"} {
		report := ws.Report(fmt.Sprintf(gitleaksReport, mode))
		_, err := ws.Run(ctx, GitleaksBinary, mode, "--redact", "--no-banner", "--exit-code", "0", "--report-format", "json", "--report-path", report, ws.Src)
		if err != nil {
			return gate.Fail("gitleaks " + mode + ": " + gate.Tail(procexec.Output(err)))
		}
		found, err := readGitleaks(report)
		if err != nil {
			return gate.Fail("gitleaks report unreadable: " + err.Error())
		}
		findings = append(findings, found...)
	}
	return gate.Findings(reasonSecrets, findings)
}

func readGitleaks(path string) ([]string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var leaks []gitleaksFinding
	if err := json.Unmarshal(data, &leaks); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(leaks))
	for _, l := range leaks {
		out = append(out, fmt.Sprintf(leakFormat, l.RuleID, l.File, l.StartLine, shortSha(l.Commit)))
	}
	return out, nil
}

func shortSha(sha string) string {
	const short = 8
	return sha[:min(len(sha), short)]
}

type trufflehogFinding struct {
	DetectorName   string
	Verified       bool
	SourceMetadata struct {
		Data struct {
			Git struct {
				File   string `json:"file"`
				Line   int    `json:"line"`
				Commit string `json:"commit"`
			} `json:"Git"`
		} `json:"Data"`
	}
}

func Trufflehog(ctx context.Context, ws gate.Workspace) gate.Outcome {
	res, err := ws.Run(ctx, TrufflehogBinary, "git", gitScheme+ws.Src, "--results=verified,unverified,unknown", "--json", "--no-update")
	findings, perr := parseTrufflehog(res.Stdout)
	return gate.FromTool(reasonSecrets, res, err, findings, perr)
}

func parseTrufflehog(stream string) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxSecretLine)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var f trufflehogFinding
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			return nil, err
		}
		g := f.SourceMetadata.Data.Git
		out = append(out, fmt.Sprintf(leakFormat+" verified=%t", f.DetectorName, g.File, g.Line, shortSha(g.Commit), f.Verified))
	}
	return out, sc.Err()
}
