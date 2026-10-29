package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	assetURL    = "url"
	pat         = "github_pat_good"
	wrongPAT    = "github_pat_wrong"
	noRuns      = `{"workflow_runs":[]}`
	forged      = "evil"
	behind      = `{"status":"behind"}`
	assetName   = "name"
	garbage     = "garbage"
	repo        = "Owner/deploy-hub"
	oldCommit   = "1111111111111111111111111111111111111111"
	newCommit   = "2222222222222222222222222222222222222222"
	newTag      = TagPrefix + newCommit
	newBinary   = "new binary"
	oldBinary   = "old binary"
	latest      = "/repos/" + repo + "/releases/latest"
	runsPath    = "/repos/" + repo + "/actions/workflows/ci.yml/runs"
	runID       = 77
	builtPath   = "/repos/" + repo + "/actions/runs/77/artifacts"
	zipPath     = "/repos/" + repo + "/actions/artifacts/5/zip"
	assets      = "/repos/" + repo + "/releases/assets/"
	binaryPath  = assets + "11"
	sumPath     = assets + "12"
	comparePath = "/repos/" + repo + "/compare/" + oldCommit + "..." + newCommit
	verify      = "visudo -cf"
	version     = InstalledCommand + " version"
	candidate   = CandidateCommand + " version"
	syncCmd     = InstalledCommand + " sudoers-sync"
	runnerCmd   = InstalledCommand + " ci-runner-sync"
	setupCmd    = SetupCommand + " host"
)

type hub struct {
	mu      sync.Mutex
	routes  map[string]string
	queries []string
	auth    []string
	srv     *httptest.Server
}

func (h *hub) handle(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queries = append(h.queries, r.URL.RawQuery)
	h.auth = append(h.auth, r.Header.Get("Authorization"))
	if r.Header.Get("X-GitHub-Api-Version") != apiVersion {
		http.Error(w, "no API version", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+pat {
		http.Error(w, "Bad credentials", http.StatusUnauthorized)
		return
	}
	body, ok := h.routes[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(body))
}

func sum(data string) string {
	s := sha256.Sum256([]byte(data))
	return hex.EncodeToString(s[:])
}

func releaseJSON(h *hub, tag, target string) string {
	prefix := h.srv.URL + assets
	doc := map[string]any{"tag_name": tag, "target_commitish": target, "assets": []map[string]string{
		{assetName: AssetBinary, assetURL: prefix + "11"},
		{assetName: AssetSum, assetURL: prefix + "12"},
		{assetName: "notes.txt", assetURL: prefix + "13"},
	}}
	data, _ := json.Marshal(doc)
	return string(data)
}

func runsJSON(commit, branch, event, conclusion, path string) string {
	data, _ := json.Marshal(map[string]any{"workflow_runs": []map[string]any{
		{"id": runID, "head_sha": commit, "head_branch": branch, "event": event, "conclusion": conclusion, "path": path},
	}})
	return string(data)
}

func builtJSON(h *hub, name string, expired bool) string {
	data, _ := json.Marshal(map[string]any{"artifacts": []map[string]any{
		{"name": "gate-reports", "expired": false, "archive_download_url": h.srv.URL + "/repos/" + repo + "/actions/artifacts/4/zip"},
		{"name": name, "expired": expired, "archive_download_url": h.srv.URL + zipPath},
	}})
	return string(data)
}

func zipOf(t testing.TB, name, body string) string {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(name)
	testutil.NoErr(t, err)
	_, err = f.Write([]byte(body))
	testutil.NoErr(t, err)
	testutil.NoErr(t, w.Close())
	return buf.String()
}

type fixture struct {
	hub     *hub
	u       Updater
	script  procexectest.Script
	exec    *procexectest.Fake
	metrics *capture
	out     *bytes.Buffer
}

type capture struct{ samples []obs.Sample }

func (c *capture) Push(_ context.Context, s []obs.Sample) { c.samples = append(c.samples, s...) }

func (c *capture) value(name string) float64 {
	for _, s := range c.samples {
		if s.Name == name {
			return s.Value
		}
	}
	return -1
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	h := &hub{}
	h.srv = httptest.NewServer(http.HandlerFunc(h.handle))
	t.Cleanup(h.srv.Close)
	h.routes = map[string]string{
		latest:      releaseJSON(h, newTag, newCommit),
		runsPath:    runsJSON(newCommit, mainBranch, pushEvent, success, ".github/workflows/ci.yml"),
		binaryPath:  newBinary,
		sumPath:     sum(newBinary) + "  deploy-hub-linux-amd64\n",
		comparePath: `{"status":"ahead"}`,
	}
	h.routes[builtPath] = builtJSON(h, AssetBinary, false)
	h.routes[zipPath] = zipOf(t, BuiltEntry, newBinary)
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, "deploy-hub"), oldBinary)
	f := &fixture{hub: h, metrics: &capture{}, out: &bytes.Buffer{}}
	f.script = procexectest.Script{
		candidate: {Res: procexec.Result{Stdout: newCommit + "\n"}},
		version:   {Res: procexec.Result{Stdout: newCommit + "\n"}},
		syncCmd:   {},
		runnerCmd: {},
		setupCmd:  {},
		verify:    {},
	}
	f.exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) { return f.script.Handle(c) }}
	now := func() time.Time { return time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC) }
	f.u = Updater{
		Source:  Source{API: h.srv.URL, Repo: repo, Token: pat, Client: h.srv.Client()},
		Binary:  filepath.Join(dir, "deploy-hub"),
		Current: oldCommit,
		Exec:    f.exec,
		Sudo: Sudoers{Path: filepath.Join(dir, "sudoers"), Binary: DefaultBinary, Exec: f.exec,
			Hub: policy.Hub{SudoUser: "deploy", SudoCommands: []string{policy.SudoCaddySync, policy.SudoHostSync, policy.SudoSelfUpdate}}},
		Runner:  RunnerFiles{Files: runnerTree(), Dir: filepath.Join(dir, "ci-runner"), Exec: f.exec, Runners: idleHost(t, f.exec)},
		Journal: journal.Journal{Path: filepath.Join(dir, "deploys.log"), Now: now},
		Metrics: f.metrics, Now: now, Out: console.New(f.out), Log: slog.New(slog.DiscardHandler),
	}
	return f
}

func (f *fixture) journal(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(f.u.Journal.Path)
	return string(data)
}

func TestInstallsTheLatestGatedRelease(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.u.Run(context.Background(), ""))
	testutil.Equal(t, testutil.Read(t, f.u.Binary), newBinary)
	testutil.Equal(t, testutil.Read(t, f.u.Binary+previousSuffix), oldBinary)
	testutil.Perm(t, f.u.Binary, binaryPerm)
	testutil.Missing(t, f.u.Binary+candidateSuffix)
	testutil.Equal(t, strings.Join(f.exec.Lines(), "\n"), strings.Join([]string{candidate, version, syncCmd, runnerCmd}, "\n"))
	testutil.Contains(t, f.journal(t), "deploy-hub@2222222 self-update ok")
	testutil.Contains(t, f.out.String(), "self-update: installed "+newTag)
	testutil.Equal(t, f.metrics.value(obs.MetricSelfUpdateSuccess), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricSelfUpdateInstalled), 1.0)
	testutil.Contains(t, strings.Join(f.hub.queries, "&"), "head_sha="+newCommit, "branch=main", "event=push", "status=success")
}

func TestUpToDateKeepsTheSudoersRuleAndTheRunnerFiles(t *testing.T) {
	f := newFixture(t)
	f.u.Current = newCommit
	testutil.NoErr(t, f.u.Run(context.Background(), ""))
	testutil.Contains(t, f.journal(t), "deploy-hub@2222222 self-update ok")
	testutil.Equal(t, f.metrics.value(obs.MetricSelfUpdateInstalled), 1.0)
	testutil.Equal(t, f.exec.Count(setupCmd), 1)
	testutil.NoErr(t, os.Remove(f.u.Journal.Path))
	f.metrics.samples = nil
	f.out.Reset()
	testutil.NoErr(t, f.u.Run(context.Background(), ""))
	testutil.Equal(t, f.exec.Count(setupCmd), 1)
	testutil.Equal(t, testutil.Read(t, f.u.Binary), oldBinary)
	testutil.Equal(t, testutil.Read(t, f.u.Sudo.Path), f.u.Sudo.Render())
	testutil.Equal(t, f.journal(t), "")
	testutil.Contains(t, f.out.String(), "self-update: 2222222 is up to date")
	testutil.Equal(t, f.metrics.value(obs.MetricSelfUpdateInstalled), 0.0)
}

func TestAPinnedTagIsLookedUpByName(t *testing.T) {
	f := newFixture(t)
	f.hub.routes["/repos/"+repo+"/releases/tags/"+newTag] = f.hub.routes[latest]
	delete(f.hub.routes, latest)
	testutil.NoErr(t, f.u.Run(context.Background(), newTag))
	testutil.Equal(t, testutil.Read(t, f.u.Binary), newBinary)
	f.hub.routes["/repos/"+repo+"/releases/tags/"+TagPrefix+oldCommit] = f.hub.routes["/repos/"+repo+"/releases/tags/"+newTag]
	testutil.Is(t, f.u.Run(context.Background(), TagPrefix+oldCommit), ErrTag)
}

func TestRefusesReleasesThatAreNotGatedBuilds(t *testing.T) {
	cases := map[string]struct {
		mutate func(f *fixture)
		want   error
	}{
		"tag is not a commit":   {func(f *fixture) { f.hub.routes[latest] = releaseJSON(f.hub, "v1.0.0", newCommit) }, ErrTag},
		"tag and target differ": {func(f *fixture) { f.hub.routes[latest] = releaseJSON(f.hub, newTag, oldCommit) }, ErrTag},
		"short tag":             {func(f *fixture) { f.hub.routes[latest] = releaseJSON(f.hub, "sha-2222222", "2222222") }, ErrTag},
		"assets elsewhere": {func(f *fixture) {
			f.hub.routes[latest] = strings.ReplaceAll(f.hub.routes[latest], f.hub.srv.URL, "https://evil.example")
		}, ErrAsset},
		"no successful run": {func(f *fixture) { f.hub.routes[runsPath] = noRuns }, ErrNotGated},
		"failed run": {func(f *fixture) {
			f.hub.routes[runsPath] = runsJSON(newCommit, mainBranch, pushEvent, "failure", ".github/workflows/ci.yml")
		}, ErrNotGated},
		"run of a branch": {func(f *fixture) {
			f.hub.routes[runsPath] = runsJSON(newCommit, "feature", pushEvent, success, ".github/workflows/ci.yml")
		}, ErrNotGated},
		"run of a pull request": {func(f *fixture) {
			f.hub.routes[runsPath] = runsJSON(newCommit, mainBranch, "pull_request", success, ".github/workflows/ci.yml")
		}, ErrNotGated},
		"run of another workflow": {func(f *fixture) {
			f.hub.routes[runsPath] = runsJSON(newCommit, mainBranch, pushEvent, success, ".github/workflows/history-guard.yml")
		}, ErrNotGated},
		"run of another commit": {func(f *fixture) {
			f.hub.routes[runsPath] = runsJSON(oldCommit, mainBranch, pushEvent, success, ".github/workflows/ci.yml")
		}, ErrNotGated},
		"substituted binary": {func(f *fixture) { f.hub.routes[binaryPath] = forged }, ErrChecksum},
		"binary and checksum both replaced": {func(f *fixture) {
			f.hub.routes[binaryPath], f.hub.routes[sumPath] = forged, sum(forged)
		}, ErrNotBuilt},
		"build artifact expired":  {func(f *fixture) { f.hub.routes[builtPath] = builtJSON(f.hub, AssetBinary, true) }, ErrNotBuilt},
		"build artifact missing":  {func(f *fixture) { f.hub.routes[builtPath] = builtJSON(f.hub, "other", false) }, ErrNotBuilt},
		"build artifact not zip":  {func(f *fixture) { f.hub.routes[zipPath] = garbage }, ErrNotBuilt},
		"build artifact is other": {func(f *fixture) { f.hub.routes[zipPath] = zipOf(t, "notes.txt", newBinary) }, ErrNotBuilt},
		"no artifact list":        {func(f *fixture) { delete(f.hub.routes, builtPath) }, ErrHTTP},
		"no artifact download":    {func(f *fixture) { delete(f.hub.routes, zipPath) }, ErrHTTP},
		"artifact elsewhere": {func(f *fixture) {
			f.hub.routes[builtPath] = strings.ReplaceAll(builtJSON(f.hub, AssetBinary, false), f.hub.srv.URL, "https://evil.example")
		}, ErrNotBuilt},
		"checksum not a sha256": {func(f *fixture) { f.hub.routes[sumPath] = "nope" }, ErrSumFormat},
		"binary missing":        {func(f *fixture) { delete(f.hub.routes, binaryPath) }, ErrHTTP},
		"checksum missing":      {func(f *fixture) { delete(f.hub.routes, sumPath) }, ErrHTTP},
		"no release":            {func(f *fixture) { delete(f.hub.routes, latest) }, ErrHTTP},
		"no run list":           {func(f *fixture) { delete(f.hub.routes, runsPath) }, ErrHTTP},
		"no compare":            {func(f *fixture) { delete(f.hub.routes, comparePath) }, ErrHTTP},
		"only the binary": {func(f *fixture) {
			f.hub.routes[latest] = strings.Replace(f.hub.routes[latest], `"name":"deploy-hub-linux-amd64.sha256"`, `"name":"other"`, 1)
		}, ErrAsset},
		"asset url of digits": {func(f *fixture) {
			f.hub.routes[latest] = strings.Replace(f.hub.routes[latest], f.hub.srv.URL+assets+"12", "12", 1)
		}, ErrAsset},
		"main was rewritten":         {func(f *fixture) { f.hub.routes[comparePath] = `{"status":"diverged"}` }, ErrRewritten},
		"release is older":           {func(f *fixture) { f.hub.routes[comparePath] = behind }, ErrRewritten},
		"no token":                   {func(f *fixture) { f.u.Source.Token = "" }, ErrNoToken},
		"wrong token":                {func(f *fixture) { f.u.Source.Token = wrongPAT }, ErrHTTP},
		"candidate is another build": {func(f *fixture) { f.script[candidate] = procexectest.Reply{Res: procexec.Result{Stdout: oldCommit}} }, ErrVersion},
		"candidate does not start":   {func(f *fixture) { f.script[candidate] = procexectest.Reply{Err: errors.New("exec format error")} }, ErrVersion},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tc.mutate(f)
			testutil.Is(t, f.u.Run(context.Background(), ""), tc.want)
			testutil.Equal(t, testutil.Read(t, f.u.Binary), oldBinary)
			testutil.Missing(t, f.u.Binary+candidateSuffix)
			testutil.Equal(t, f.exec.Count(syncCmd), 0)
			testutil.Contains(t, f.journal(t), "self-update fail")
			testutil.Equal(t, f.metrics.value(obs.MetricSelfUpdateSuccess), 0.0)
			installed := 0.0
			if errors.Is(tc.want, ErrVersion) {
				installed = 1
			}
			testutil.Equal(t, f.metrics.value(obs.MetricSelfUpdateInstalled), installed)
		})
	}
}

func TestFailedHealthCheckRestoresThePreviousBinary(t *testing.T) {
	for name, step := range map[string]string{"version": version, "sudoers": syncCmd} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			calls := 0
			f.script[step] = procexectest.Reply{Run: func(procexec.Command) { calls++ }, Err: errors.New("exit status 1")}
			if step == version {
				f.script[step] = procexectest.Reply{Res: procexec.Result{Stdout: garbage}}
			}
			testutil.Is(t, f.u.Run(context.Background(), ""), ErrUpdate)
			testutil.Equal(t, testutil.Read(t, f.u.Binary), oldBinary)
			j := f.journal(t)
			testutil.Contains(t, j, "deploy-hub@1111111 self-update", "restored=")
			testutil.Contains(t, j, "deploy-hub@2222222 self-update fail")
		})
	}
}

func TestRestoreThatFailsIsJournaled(t *testing.T) {
	f := newFixture(t)
	f.script[version] = procexectest.Reply{Res: procexec.Result{Stdout: garbage}}
	f.script[syncCmd] = procexectest.Reply{Err: errors.New("visudo missing")}
	testutil.Is(t, f.u.Run(context.Background(), ""), ErrUpdate)
	testutil.Contains(t, f.journal(t), "self-update fail 0s restored=fail")
}

func TestUnwritableBinaryDirectoryFails(t *testing.T) {
	f := newFixture(t)
	f.u.Binary = filepath.Join(t.TempDir(), "missing", "deploy-hub")
	testutil.Fails(t, f.u.Run(context.Background(), ""))
	g := newFixture(t)
	testutil.NoErr(t, os.Remove(g.u.Binary))
	testutil.Fails(t, g.u.Run(context.Background(), ""))
	testutil.Missing(t, g.u.Binary+candidateSuffix)
}

func TestOversizedOrBrokenDocumentsFail(t *testing.T) {
	f := newFixture(t)
	f.hub.routes[latest] = "{"
	testutil.Fails(t, f.u.Run(context.Background(), ""))
	g := newFixture(t)
	g.hub.routes[runsPath] = "{"
	testutil.Fails(t, g.u.Run(context.Background(), ""))
	h := newFixture(t)
	h.hub.routes[latest] = strings.Repeat(" ", maxDocument+1)
	testutil.Is(t, h.u.Run(context.Background(), ""), ErrTooLarge)
	bad := Source{API: "http://\x7f", Token: pat, Client: http.DefaultClient}
	_, err := bad.Find(context.Background(), "")
	testutil.Fails(t, err)
}

func TestSudoersRuleAllowsOnlyTheListedCommands(t *testing.T) {
	f := newFixture(t)
	testutil.Equal(t, f.u.Sudo.Render(), "deploy ALL=(root) NOPASSWD: /opt/deploy-hub/bin/deploy-hub caddy-sync \"\"\n"+
		"deploy ALL=(root) NOPASSWD: /opt/deploy-hub/bin/deploy-hub host-sync *\n"+
		"deploy ALL=(root) NOPASSWD: /opt/deploy-hub/bin/deploy-hub self-update \"\"\n")
	testutil.NoErr(t, f.u.Sudo.Sync(context.Background()))
	testutil.Perm(t, f.u.Sudo.Path, sudoersPerm)
	testutil.Equal(t, f.exec.Count(verify), 1)
	testutil.NoErr(t, f.u.Sudo.Sync(context.Background()))
	testutil.Equal(t, f.exec.Count(verify), 1)
}

func TestSudoersRefusedByVisudoKeepsThePreviousRule(t *testing.T) {
	f := newFixture(t)
	testutil.Write(t, f.u.Sudo.Path, "previous\n")
	f.script[verify] = procexectest.Reply{Err: errors.New("syntax error")}
	testutil.Is(t, f.u.Sudo.Sync(context.Background()), ErrSudoers)
	testutil.Equal(t, testutil.Read(t, f.u.Sudo.Path), "previous\n")
	testutil.Missing(t, filepath.Join(filepath.Dir(f.u.Sudo.Path), sudoersStaging))
	f.u.Sudo.Path = filepath.Join(t.TempDir(), "missing", "sudoers")
	testutil.Fails(t, f.u.Sudo.Sync(context.Background()))
}

func TestValidTag(t *testing.T) {
	testutil.True(t, ValidTag(newTag), "full sha tag")
	for _, bad := range []string{"", "sha-", "v1", TagPrefix + "222", TagPrefix + "ABCDEF" + newCommit[6:], newCommit} {
		testutil.True(t, !ValidTag(bad), bad)
	}
}

func FuzzReleaseAcceptsOnlyMatchingShaTags(f *testing.F) {
	f.Add(newTag, newCommit)
	f.Add("v1", "main")
	f.Fuzz(func(t *testing.T, tag, target string) {
		s := Source{API: "https://api.github.com", Repo: repo}
		r, err := s.release(releaseDoc{TagName: tag, Target: target}, "")
		if err == nil || (ValidTag(tag) && tag == TagPrefix+target && !errors.Is(err, ErrAsset)) {
			t.Fatalf("%q %q: %+v %v", tag, target, r, err)
		}
	})
}

func TestNoTokenRefusesBeforeAnyRequest(t *testing.T) {
	f := newFixture(t)
	f.u.Source.Token = ""
	testutil.Is(t, f.u.Run(context.Background(), ""), ErrNoToken)
	testutil.Equal(t, len(f.hub.auth), 0)
}

func TestWrongTokenIsReportedAsUnauthorized(t *testing.T) {
	f := newFixture(t)
	f.u.Source.Token = wrongPAT
	err := f.u.Run(context.Background(), "")
	testutil.Is(t, err, ErrHTTP)
	testutil.Contains(t, err.Error(), "401")
	testutil.Equal(t, testutil.Read(t, f.u.Binary), oldBinary)
}

func TestEveryRequestCarriesTheToken(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.u.Run(context.Background(), ""))
	testutil.Equal(t, len(f.hub.auth), 7)
	for _, a := range f.hub.auth {
		testutil.Equal(t, a, "Bearer "+pat)
	}
}

func TestPinnedRollbackSkipsTheAncestryCheck(t *testing.T) {
	f := newFixture(t)
	f.hub.routes["/repos/"+repo+"/releases/tags/"+newTag] = f.hub.routes[latest]
	f.hub.routes[comparePath] = behind
	testutil.NoErr(t, f.u.Run(context.Background(), newTag))
	testutil.Equal(t, testutil.Read(t, f.u.Binary), newBinary)
}

func TestUnknownCurrentCommitSkipsTheAncestryCheck(t *testing.T) {
	f := newFixture(t)
	f.u.Current = "dev"
	delete(f.hub.routes, comparePath)
	testutil.NoErr(t, f.u.Run(context.Background(), ""))
}

func TestAssetRedirectDropsTheToken(t *testing.T) {
	var seen []string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(newBinary))
	}))
	t.Cleanup(storage.Close)
	f := newFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc(binaryPath, func(w http.ResponseWriter, r *http.Request) {
		testutil.Equal(t, r.Header.Get("Accept"), acceptBinary)
		http.Redirect(w, r, storage.URL+"/signed", http.StatusFound)
	})
	mux.HandleFunc("/", f.hub.handle)
	f.hub.srv.Config.Handler = mux
	testutil.NoErr(t, f.u.Run(context.Background(), ""))
	testutil.Equal(t, testutil.Read(t, f.u.Binary), newBinary)
	testutil.Equal(t, len(seen), 1)
	testutil.Equal(t, seen[0], "")
}

func TestEndlessAssetRedirectsFail(t *testing.T) {
	f := newFixture(t)
	hops := 0
	mux := http.NewServeMux()
	mux.HandleFunc(binaryPath, func(w http.ResponseWriter, r *http.Request) {
		hops++
		testutil.Equal(t, r.Header.Get("Authorization"), "Bearer "+pat)
		http.Redirect(w, r, binaryPath, http.StatusFound)
	})
	mux.HandleFunc("/", f.hub.handle)
	f.hub.srv.Config.Handler = mux
	testutil.Is(t, f.u.Run(context.Background(), ""), ErrRedirects)
	testutil.Equal(t, hops, maxRedirects)
}

func TestADocumentOfExactlyTheLimitIsRead(t *testing.T) {
	f := newFixture(t)
	doc := f.hub.routes[latest]
	f.hub.routes[latest] = doc + strings.Repeat(" ", maxDocument-len(doc))
	testutil.NoErr(t, f.u.Run(context.Background(), ""))
	testutil.Equal(t, f.metrics.value(obs.MetricSelfUpdateInstalled), 1.0)
}

func TestABrokenCompareFails(t *testing.T) {
	f := newFixture(t)
	f.hub.routes[comparePath] = "{"
	testutil.Fails(t, f.u.Run(context.Background(), ""))
	testutil.Equal(t, testutil.Read(t, f.u.Binary), oldBinary)
}

func TestFilesKeepTheirModeUnderAStrictUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	f := newFixture(t)
	testutil.NoErr(t, f.u.Run(context.Background(), ""))
	testutil.Perm(t, f.u.Binary, binaryPerm)
	testutil.NoErr(t, f.u.Sudo.Sync(context.Background()))
	testutil.Perm(t, f.u.Sudo.Path, sudoersPerm)
}
