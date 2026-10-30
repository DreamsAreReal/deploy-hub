package stage

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/crawler"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	cleanReport = "# Crawl of every screen\n\n23 of 25 screens.\n\n## Problems\n\n| kind | steps | detail | screenshot |\n| --- | --- | --- | --- |\n\n## Notes\n\n| kind | steps | detail | screenshot |\n| --- | --- | --- | --- |\n| small-target | open /ui | 2 buttons | |\n"
	badReport   = "## Problems\n\n| kind | steps | detail | screenshot |\n| --- | --- | --- | --- |\n| js-error | open /ui/x | boom | shots/001.png |\n| cut-text | open /ui/y @360 | 1 — p | |\n\n## Notes\n\n| alert | open /ui | no | |\n"
	crawlName   = "deploy-hub-gate-web-ui-crawl"
	crawlVol    = crawlName + crawlVolume
	tarBlock    = 512
	tarTop      = "./"
	shotBody    = "png"
	dataDir     = "/data"
	npmRun      = "run"
	cutRow      = "| cut-text | open /ui/y @360 | 1 — p | |"
	errorRow    = "| js-error | open /ui/x | boom | shots/001.png |"
)

type crawlSim struct {
	report   string
	cpErr    error
	volErr   error
	crawlErr error
	seedErr  error
}

type tarEntry struct {
	name string
	body string
	kind byte
}

func uiApp(t *testing.T, sim *crawlSim) gate.Workspace {
	t.Helper()
	ws, docker := app(t, okHandler())
	ws.Out, ws.Src = t.TempDir(), "/repo"
	ws.App.Data = []string{dataDir}
	ws.App.UI = &policy.UI{Map: "web/appmap.json", Seed: []string{seedCmd, flagDB, dataDir + "/app.db"}, Env: []string{"DEV_LOGIN=1"}, Chromium: "/ms/chrome"}
	ws.Policy.Images.Browser = "browser@sha256:2"
	ws.Exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		joined := strings.Join(c.Args, " ")
		switch {
		case strings.HasPrefix(joined, "volume create"):
			return procexec.Result{}, sim.volErr
		case slices.Contains(c.Args, tarBinary):
			out, err := sim.archive(t)
			return procexec.Result{Stdout: out}, err
		case strings.Contains(joined, "--name "+crawlName):
			return procexec.Result{Stderr: "crawl failed"}, sim.crawlErr
		case strings.Contains(joined, "seed --db"):
			return procexec.Result{}, sim.seedErr
		}
		return docker.handle(c)
	}}
	return ws
}

func (s *crawlSim) archive(t *testing.T) (string, error) {
	if s.cpErr != nil {
		return "", s.cpErr
	}
	entries := []tarEntry{{name: tarTop, kind: tar.TypeDir}}
	if s.report != "" {
		entries = append(entries, tarEntry{tarTop + crawlReport, s.report, tar.TypeReg}, tarEntry{"./shots/", "", tar.TypeDir}, tarEntry{"./shots/001.png", shotBody, tar.TypeReg})
	}
	return tarOf(t, entries...), nil
}

func tarOf(t *testing.T, entries ...tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range entries {
		testutil.NoErr(t, w.WriteHeader(&tar.Header{Name: e.name, Typeflag: e.kind, Size: int64(len(e.body)), Mode: 0o644, Linkname: e.body}))
		if e.kind == tar.TypeReg {
			_, err := w.Write([]byte(e.body))
			testutil.NoErr(t, err)
		}
	}
	testutil.NoErr(t, w.Close())
	return buf.String()
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
		"docker run --rm --network none --security-opt no-new-privileges -e MODE=test -e DEV_LOGIN=1 -e "+frozenAppEnv+FrozenAt+
			" --mount type=volume,src=deploy-hub-gate-web-ui-data-0,dst=/data web:gate seed --db /data/app.db",
		"-e MODE=test -e DEV_LOGIN=1 -e "+frozenAppEnv+FrozenAt+" --mount type=volume,src=deploy-hub-gate-web-ui-data-0,dst=/data web:gate\n",
		"docker run --name "+crawlName+" --network deploy-hub-gate-web-ui-net --cpus 2 --memory 2g --memory-swap 2g --shm-size 1g --pids-limit 1024",
		"-v /repo:/src:ro -v "+filepath.Join(ws.Out, crawlerDir)+":/crawler:ro -v "+crawlVol+":/out -w /src -e E2E_BASE=http://deploy-hub-gate-web-ui:8080 -e E2E_OUT=/out "+
			"-e E2E_CHROMIUM=/ms/chrome -e CRAWL_MODE=quick -e E2E_APPMAP=/src/web/appmap.json -e "+frozenCrawlEnv+FrozenAt+
			" browser@sha256:2 node --test /crawler/crawl.mjs",
		"docker volume create "+crawlVol,
		"docker run --rm -v "+crawlVol+":/out:ro --entrypoint tar browser@sha256:2 -C /out -cf - .")
	testutil.Equal(t, strings.Count(all, "docker rm -f "+crawlName+"\n"), 2)
	testutil.Equal(t, strings.Count(all, "docker volume rm -f "+crawlVol), 2)
	testutil.Equal(t, testutil.Read(t, filepath.Join(ws.Out, crawlDir, "shots", "001.png")), shotBody)
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
	modules, err := crawler.Modules()
	testutil.NoErr(t, err)
	for _, module := range modules {
		written, err := os.ReadFile(filepath.Join(ws.Out, crawlerDir, module.Name))
		testutil.NoErr(t, err)
		testutil.True(t, bytes.Equal(written, module.Body), module.Name+" of the hub's own crawler is laid out for the browser container")
	}
}

func TestUIFailsWhenItCannotLayOutItsCrawler(t *testing.T) {
	ws := uiApp(t, &crawlSim{report: cleanReport})
	blocked := filepath.Join(t.TempDir(), "file")
	testutil.NoErr(t, os.WriteFile(blocked, nil, 0o600))
	ws.Out = blocked
	taken := uiApp(t, &crawlSim{report: cleanReport})
	testutil.Mkdir(t, filepath.Join(taken.Out, crawlerDir, crawler.File))
	for _, app := range []gate.Workspace{ws, taken} {
		out := UI(context.Background(), app)
		testutil.Equal(t, out.Status, gate.StatusFail)
		testutil.Equal(t, out.Reason, reasonNoCrawl)
	}
}

func TestUIFailsWhenItCannotLoadItsCrawler(t *testing.T) {
	loadCrawler = func() ([]crawler.Module, error) { return nil, errors.New("embedded crawler unreadable") }
	t.Cleanup(func() { loadCrawler = crawler.Modules })
	out := UI(context.Background(), uiApp(t, &crawlSim{report: cleanReport}))
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Contains(t, strings.Join(out.Findings, " ")+out.Reason, reasonNoCrawl)
}

func keyFor(t *testing.T, ws gate.Workspace) string {
	t.Helper()
	modules, err := crawler.Modules()
	testutil.NoErr(t, err)
	return (&Stage{ws: ws, crawler: modules}).renderKey()
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
	noVolume := UI(context.Background(), uiApp(t, &crawlSim{report: cleanReport, volErr: errExit}))
	testutil.Equal(t, noVolume.Reason, reasonNoCrawl)
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

func visualApp(t *testing.T, list string) gate.Workspace {
	t.Helper()
	ws := uiApp(t, &crawlSim{report: cleanReport})
	ws.Src = t.TempDir()
	testutil.Mkdir(t, filepath.Join(ws.Src, filepath.Dir(VisualFile)))
	testutil.Write(t, filepath.Join(ws.Src, VisualFile), list)
	return ws
}

func TestUIComparesWithTheDeployedShotsAndPassesTheIntendedChanges(t *testing.T) {
	ws := visualApp(t, "changes:\n  - screen: board\n    task: 809\n  - screen: memory-topic\n    task: 782\n")
	ws.UIBaseline = t.TempDir()
	key := keyFor(t, ws)
	testutil.Write(t, filepath.Join(ws.UIBaseline, RenderKeyFile), key+"\n")
	out := UI(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Contains(t, strings.Join(procexectest.LinesOf(ws.Exec), "\n"), "-e "+frozenCrawlEnv+FrozenAt+" -v "+ws.UIBaseline+
		":/baseline:ro -e E2E_BASELINE=/baseline -e E2E_VISUAL_ACCEPT=board,memory-topic browser@sha256:2 node --test /crawler/crawl.mjs")
	testutil.Equal(t, testutil.Read(t, filepath.Join(ws.Out, crawlDir, RenderKeyFile)), key+"\n")
	for _, baseline := range []string{filepath.Join(t.TempDir(), "never-served"), t.TempDir()} {
		other := visualApp(t, "")
		other.UIBaseline = baseline
		if exists(baseline) {
			testutil.Write(t, filepath.Join(baseline, RenderKeyFile), strings.Repeat("0", len(key)))
		}
		testutil.Equal(t, UI(context.Background(), other).Status, gate.StatusPass)
		all := strings.Join(procexectest.LinesOf(other.Exec), "\n")
		testutil.True(t, !strings.Contains(all, "E2E_BASELINE") && !strings.Contains(all, "E2E_VISUAL_ACCEPT"), "no baseline of this crawler and browser, nothing accepted")
	}
	browser := visualApp(t, "")
	browser.Policy.Images.Browser = "browser@sha256:3"
	testutil.True(t, keyFor(t, browser) != key, "a new browser image changes the key")
	modules, err := crawler.Modules()
	testutil.NoErr(t, err)
	for index := range modules {
		edited := slices.Clone(modules)
		edited[index].Body = append(slices.Clone(edited[index].Body), ' ')
		testutil.True(t, (&Stage{ws: ws, crawler: edited}).renderKey() != key, "a change of "+modules[index].Name+" changes the key")
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestUIRefusesABrokenListOfIntendedChanges(t *testing.T) {
	for _, list := range []string{
		"changes:\n  - screen: board\n",
		"changes:\n  - screen: Board!\n    task: 1\n",
		"changes:\n  - screen: board\n    task: -3\n",
		"changes:\n  - screen: " + strings.Repeat("a", screenIDLimit+1) + "\n    task: 1\n",
		"changes:\n  - screen: board\n    task: 1\n    why: looks nicer\n",
		"changes: [",
	} {
		ws := visualApp(t, list)
		out := UI(context.Background(), ws)
		testutil.Equal(t, out.Status, gate.StatusFail)
		testutil.Equal(t, out.Reason, reasonVisual)
		testutil.True(t, !strings.Contains(strings.Join(procexectest.LinesOf(ws.Exec), "\n"), "docker run"), "no stand for a broken list")
	}
	escaped := visualApp(t, "")
	outside := filepath.Join(t.TempDir(), "visual.yaml")
	testutil.Write(t, outside, "changes:\n  - screen: board\n    task: 1\n")
	testutil.NoErr(t, os.Remove(filepath.Join(escaped.Src, VisualFile)))
	testutil.NoErr(t, os.Symlink(outside, filepath.Join(escaped.Src, VisualFile)))
	testutil.Equal(t, UI(context.Background(), escaped).Reason, reasonVisual)
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

func TestUnpackKeepsTheCrawlInsideItsFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	good := tarOf(t, tarEntry{tarTop, "", tar.TypeDir}, tarEntry{"./shots/a.png", shotBody, tar.TypeReg}, tarEntry{"./deep/", "", tar.TypeDir},
		tarEntry{"./deep/b.txt", "b", tar.TypeReg}, tarEntry{"./link", "/etc/passwd", tar.TypeSymlink})
	testutil.NoErr(t, unpack(strings.NewReader(good), dir))
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, "shots", "a.png")), shotBody)
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, "deep", "b.txt")), "b")
	testutil.Missing(t, filepath.Join(dir, "link"))
	for name, archive := range map[string]string{
		"an entry outside the folder": tarOf(t, tarEntry{"../escape.txt", "x", tar.TypeReg}),
		"a folder outside the folder": tarOf(t, tarEntry{"../up/", "", tar.TypeDir}),
		"a file under a file":         tarOf(t, tarEntry{"./a", "a", tar.TypeReg}, tarEntry{"./a/b", "b", tar.TypeReg}),
		"a file in place of a folder": tarOf(t, tarEntry{"./d/", "", tar.TypeDir}, tarEntry{"./d", "d", tar.TypeReg}),
		"a cut file":                  good[:2*tarBlock+2],
		"text instead of an archive":  strings.Repeat("not a tar archive ", 64),
	} {
		testutil.True(t, unpack(strings.NewReader(archive), t.TempDir()) != nil, name+" fails")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	testutil.NoErr(t, os.WriteFile(blocked, nil, 0o600))
	testutil.True(t, unpack(strings.NewReader(good), filepath.Join(blocked, "sub")) != nil, "a folder that cannot be made fails")
}
