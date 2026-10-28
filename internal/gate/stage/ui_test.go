package stage

import (
	"context"
	"errors"
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

const (
	cleanReport = "# Crawl of every screen\n\n23 of 25 screens.\n\n## Problems\n\n| kind | steps | detail | screenshot |\n| --- | --- | --- | --- |\n\n## Notes\n\n| kind | steps | detail | screenshot |\n| --- | --- | --- | --- |\n| small-target | open /ui | 2 buttons | |\n"
	badReport   = "## Problems\n\n| kind | steps | detail | screenshot |\n| --- | --- | --- | --- |\n| js-error | open /ui/x | boom | shots/001.png |\n| cut-text | open /ui/y @360 | 1 — p | |\n\n## Notes\n\n| alert | open /ui | no | |\n"
	crawlName   = "deploy-hub-gate-web-ui-crawl"
	dataDir     = "/data"
	npmRun      = "run"
	cutRow      = "| cut-text | open /ui/y @360 | 1 — p | |"
	errorRow    = "| js-error | open /ui/x | boom | shots/001.png |"
)

type crawlSim struct {
	report   string
	cpErr    error
	crawlErr error
	seedErr  error
}

func uiApp(t *testing.T, sim *crawlSim) gate.Workspace {
	t.Helper()
	ws, docker := app(t, okHandler())
	ws.Out, ws.Src = t.TempDir(), "/repo"
	ws.App.Data = []string{dataDir}
	ws.App.UI = &policy.UI{Crawl: []string{"npm", npmRun, "-s", "crawl"}, Seed: []string{"seed", "--db", dataDir + "/app.db"}, Env: []string{"DEV_LOGIN=1"}, Chromium: "/ms/chrome"}
	ws.Policy.Images.Browser = "browser@sha256:2"
	ws.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		joined := strings.Join(c.Args, " ")
		switch {
		case c.Args[0] == "cp":
			return procexec.Result{}, sim.copy(c.Args[2])
		case strings.Contains(joined, "--name "+crawlName):
			return procexec.Result{Stderr: "crawl failed"}, sim.crawlErr
		case strings.Contains(joined, "seed --db"):
			return procexec.Result{}, sim.seedErr
		}
		return docker.handle(c)
	}}
	return ws
}

func (s *crawlSim) copy(dir string) error {
	if s.cpErr != nil {
		return s.cpErr
	}
	if err := os.MkdirAll(dir, 0o750); err != nil || s.report == "" {
		return err
	}
	return os.WriteFile(filepath.Join(dir, crawlReport), []byte(s.report), 0o600)
}

func TestUIIsNotApplicableWithoutAnInterfaceAndNeedsAnImage(t *testing.T) {
	ws, _ := app(t, okHandler())
	testutil.Equal(t, UI(context.Background(), ws).Status, gate.StatusNA)
	ws = uiApp(t, &crawlSim{report: cleanReport})
	ws.Image = ""
	out := UI(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Reason, reasonNoImage)
}

func TestUISeedsTheStandAndCrawlsItQuickly(t *testing.T) {
	ws := uiApp(t, &crawlSim{report: cleanReport})
	out := UI(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Equal(t, out.Values[ValueProblems], 0.0)
	lines := procexectest.LinesOf(ws.Exec)
	all := strings.Join(lines, "\n")
	testutil.Contains(t, all,
		"docker run --rm --network none --security-opt no-new-privileges -e MODE=test -e DEV_LOGIN=1 --mount type=volume,src=deploy-hub-gate-web-ui-data-0,dst=/data web:gate seed --db /data/app.db",
		"-e MODE=test -e DEV_LOGIN=1 --mount type=volume,src=deploy-hub-gate-web-ui-data-0,dst=/data web:gate\n",
		"docker run --name "+crawlName+" --network deploy-hub-gate-web-ui-net --cpus 2 --memory 2g --memory-swap 2g --shm-size 1g --pids-limit 1024",
		"-v /repo:/src:ro -w /src -e E2E_BASE=http://deploy-hub-gate-web-ui:8080 -e E2E_OUT=/out -e E2E_CHROMIUM=/ms/chrome -e CRAWL_MODE=quick browser@sha256:2 npm run -s crawl",
		"docker cp "+crawlName+":/out "+filepath.Join(ws.Out, crawlDir))
	testutil.Equal(t, strings.Count(all, "docker rm -f "+crawlName), 2)
	seedAt, standAt := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "seed --db") {
			seedAt = i
		}
		if strings.HasPrefix(line, "docker run -d") {
			standAt = i
		}
	}
	testutil.True(t, seedAt >= 0 && seedAt < standAt, "the seed runs before the stand starts")
}

func TestUIFailsOnTheProblemsOfTheReportAndCrawlsFullyInAFullGate(t *testing.T) {
	ws := uiApp(t, &crawlSim{report: badReport, crawlErr: errExit})
	ws.Mutation.Full = true
	out := UI(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Findings, []string{errorRow, cutRow})
	testutil.Equal(t, out.Values[ValueProblems], 2.0)
	testutil.Contains(t, strings.Join(procexectest.LinesOf(ws.Exec), "\n"), "-e CRAWL_MODE=full")
}

func TestUIFailsWithoutAReportOrWhenTheCrawlBreaks(t *testing.T) {
	noCopy := UI(context.Background(), uiApp(t, &crawlSim{cpErr: errExit}))
	testutil.Equal(t, noCopy.Reason, reasonNoCrawl)
	noFile := UI(context.Background(), uiApp(t, &crawlSim{}))
	testutil.Equal(t, noFile.Reason, reasonNoCrawl)
	broken := UI(context.Background(), uiApp(t, &crawlSim{report: cleanReport, crawlErr: errExit}))
	testutil.Equal(t, broken.Status, gate.StatusFail)
	testutil.Equal(t, len(broken.Findings), 1)
	testutil.Contains(t, broken.Findings[0], "crawl failed")
	seedless := uiApp(t, &crawlSim{seedErr: errors.New("no seed")})
	failed := UI(context.Background(), seedless)
	testutil.Contains(t, failed.Reason, "docker run")
	all := strings.Join(procexectest.LinesOf(seedless.Exec), "\n")
	testutil.True(t, !strings.Contains(all, "docker run -d"), "no stand without its seed")
	testutil.Equal(t, strings.Count(all, "docker network rm deploy-hub-gate-web-ui-net"), 2)
}

func TestProblemRowsReadOnlyTheProblemsTable(t *testing.T) {
	testutil.Equal(t, problemRows(badReport), []string{errorRow, cutRow})
	testutil.Equal(t, len(problemRows(cleanReport)), 0)
	testutil.Equal(t, len(problemRows("no table at all")), 0)
}

func TestStartWithoutASeedRunsNothingBeforeTheStand(t *testing.T) {
	ws, _ := app(t, okHandler())
	st, err := StartSeeded(context.Background(), ws, "test", nil)
	testutil.NoErr(t, err)
	st.Stop(context.Background())
	testutil.True(t, !strings.Contains(strings.Join(procexectest.LinesOf(ws.Exec), "\n"), "--network none"), "no seed container")
}
