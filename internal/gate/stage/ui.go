package stage

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/crawler"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	ValueProblems   = "problems"
	purposeUI       = "ui"
	crawlSuffix     = "-crawl"
	crawlSrc        = "/src"
	crawlOut        = "/out"
	crawlReport     = "crawl.md"
	crawlDir        = "ui-crawl"
	crawlerDir      = "ui-crawler"
	crawlerMount    = "/crawler"
	crawlerMode     = 0o750
	crawlerFileMode = 0o644
	crawlCPUs       = "2"
	crawlMemory     = "2g"
	crawlShm        = "1g"
	crawlPids       = "1024"
	crawlQuick      = "quick"
	crawlFull       = "full"
	reasonNoUI      = "the app has no interface to crawl"
	reasonUI        = "the crawl of the interface found problems"
	reasonNoCrawl   = "the crawl left no report"
	problemsHeading = "## Problems"
	tableRow        = "| "
	tableHead       = "| kind |"
	tableRule       = "| --- |"
	readOnly        = ":ro"
	crawlFixedArgs  = 40
	nodeBinary      = "node"
	nodeTest        = "--test"
)

func UI(ctx context.Context, ws gate.Workspace) gate.Outcome {
	ui := ws.App.UI
	if ui == nil {
		return gate.Outcome{Status: gate.StatusNA, Reason: reasonNoUI}
	}
	if ws.Image == "" {
		return gate.Fail(reasonNoImage)
	}
	ws.App.Env = append(slices.Clone(ws.App.Env), ui.Env...)
	st, err := StartSeeded(ctx, ws, purposeUI, ui.Seed)
	if err != nil {
		return gate.Fail(err.Error())
	}
	defer st.Stop(ctx)
	return st.crawl(ctx)
}

func (s *Stage) crawl(ctx context.Context) gate.Outcome {
	name := s.Name + crawlSuffix
	_, _ = s.ws.Run(ctx, DockerBinary, "rm", "-f", name)
	defer func() { _, _ = s.ws.Run(context.WithoutCancel(ctx), DockerBinary, "rm", "-f", name) }()
	scripts, err := s.crawlerScripts()
	if err != nil {
		return gate.Fail(reasonNoCrawl, err.Error())
	}
	res, runErr := s.ws.Run(ctx, DockerBinary, s.crawlArgs(name, scripts)...)
	dir := filepath.Join(s.ws.Out, crawlDir)
	if err := s.docker(ctx, "cp", name+":"+crawlOut, dir); err != nil {
		return gate.Fail(reasonNoCrawl, err.Error())
	}
	report, err := os.ReadFile(filepath.Clean(filepath.Join(dir, crawlReport)))
	if err != nil {
		return gate.Fail(reasonNoCrawl, err.Error())
	}
	findings := problemRows(string(report))
	if runErr != nil && len(findings) == 0 {
		findings = []string{gate.Tail(procexec.Output(runErr) + res.Combined())}
	}
	return gate.Judge(len(findings) == 0, reasonUI, map[string]float64{ValueProblems: float64(len(findings))}, findings)
}

func (s *Stage) crawlerScripts() (string, error) {
	dir := filepath.Join(s.ws.Out, crawlerDir)
	if err := os.MkdirAll(dir, crawlerMode); err != nil {
		return "", err
	}
	return dir, os.WriteFile(filepath.Join(dir, crawler.File), crawler.Script, crawlerFileMode)
}

func (s *Stage) crawlArgs(name, scripts string) []string {
	ui := s.ws.App.UI
	mode := crawlQuick
	if s.ws.Mutation.Full {
		mode = crawlFull
	}
	args := make([]string, 0, crawlFixedArgs)
	return append(args,
		"run", "--name", name, flagNetwork, s.Network, "--cpus", crawlCPUs, "--memory", crawlMemory, "--memory-swap", crawlMemory,
		"--shm-size", crawlShm, flagPids, crawlPids, flagSecurity, noPrivileges,
		"-v", s.ws.Src+":"+crawlSrc+readOnly, "-v", scripts+":"+crawlerMount+readOnly, "-w", crawlSrc,
		flagEnv, "E2E_BASE="+strings.TrimSuffix(s.InternalURL(""), "/"), flagEnv, "E2E_OUT="+crawlOut,
		flagEnv, "E2E_CHROMIUM="+ui.Chromium, flagEnv, "CRAWL_MODE="+mode, flagEnv, "E2E_APPMAP="+crawlSrc+"/"+ui.Map,
		s.ws.Policy.Images.Browser, nodeBinary, nodeTest, crawlerMount+"/"+crawler.File,
	)
}

func problemRows(report string) []string {
	_, problems, found := strings.Cut(report, problemsHeading)
	if !found {
		return nil
	}
	var rows []string
	for _, line := range strings.Split(problems, "\n") {
		if strings.HasPrefix(line, "## ") {
			break
		}
		if strings.HasPrefix(line, tableRow) && !strings.HasPrefix(line, tableHead) && !strings.HasPrefix(line, tableRule) {
			rows = append(rows, strings.TrimSpace(line))
		}
	}
	return rows
}
