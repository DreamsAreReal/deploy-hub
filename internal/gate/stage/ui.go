package stage

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/crawler"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	ValueProblems   = "problems"
	keyTail         = 2
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
	crawlFixedArgs  = 42
	crawlVolume     = "-out"
	tarBinary       = "tar"
	nodeBinary      = "node"
	nodeTest        = "--test"
	FrozenAt        = "2030-01-15T09:30:00Z"
	frozenAppEnv    = "GATE_FROZEN_AT="
	frozenCrawlEnv  = "E2E_FROZEN_AT="
	crawlBaseline   = "/baseline"
	VisualFile      = ".gate/visual.yaml"
	reasonVisual    = "the list of intended interface changes is invalid"
	screenIDLimit   = 80
	RenderKeyFile   = "render-key"
)

var loadCrawler = crawler.Modules

var screenID = regexp.MustCompile(fmt.Sprintf(`^[a-z0-9][a-z0-9-]{0,%d}$`, screenIDLimit-1))

type visualChange struct {
	Screen string `yaml:"screen"`
	Task   int    `yaml:"task"`
}

type visualList struct {
	Changes []visualChange `yaml:"changes"`
}

func UI(ctx context.Context, ws gate.Workspace) gate.Outcome {
	ui := ws.App.UI
	if ui == nil {
		return gate.Outcome{Status: gate.StatusNA, Reason: reasonNoUI}
	}
	if ws.Image == "" {
		return gate.Fail(reasonNoImage)
	}
	accepted, err := visualChanges(ws.Src)
	if err != nil {
		return gate.Fail(reasonVisual, err.Error())
	}
	ws.App.Env = slices.Concat(ws.App.Env, ui.Env, []string{frozenAppEnv + FrozenAt})
	st, err := StartSeeded(ctx, ws, purposeUI, ui.Seed)
	if err != nil {
		return gate.Fail(err.Error())
	}
	defer st.Stop(ctx)
	return st.crawl(ctx, accepted)
}

func visualChanges(src string) ([]string, error) {
	data, err := readInside(src, VisualFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list visualList
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&list); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", VisualFile, err)
	}
	return acceptedScreens(list.Changes)
}

func readInside(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(name)
}

func acceptedScreens(changes []visualChange) ([]string, error) {
	screens := make([]string, 0, len(changes))
	for _, change := range changes {
		if !screenID.MatchString(change.Screen) || change.Task <= 0 {
			return nil, fmt.Errorf("%s: every change names a screen id and its task number, got screen %q, task %d", VisualFile, change.Screen, change.Task)
		}
		screens = append(screens, change.Screen)
	}
	return screens, nil
}

func (s *Stage) crawl(ctx context.Context, accepted []string) gate.Outcome {
	name := s.Name + crawlSuffix
	vol := name + crawlVolume
	s.dropCrawl(ctx, name, vol)
	defer s.dropCrawl(context.WithoutCancel(ctx), name, vol)
	scripts, err := s.crawlerScripts()
	if err == nil {
		err = s.docker(ctx, "volume", "create", vol)
	}
	if err != nil {
		return gate.Fail(reasonNoCrawl, err.Error())
	}
	res, runErr := s.ws.Run(ctx, DockerBinary, s.crawlArgs(name, vol, scripts, accepted)...)
	dir := filepath.Join(s.ws.Out, crawlDir)
	if err = s.collectCrawl(ctx, vol, dir); err != nil {
		return gate.Fail(reasonNoCrawl, err.Error())
	}
	report, err := os.ReadFile(filepath.Clean(filepath.Join(dir, crawlReport)))
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, RenderKeyFile), []byte(s.renderKey()+"\n"), crawlerFileMode)
	}
	if err != nil {
		return gate.Fail(reasonNoCrawl, err.Error())
	}
	findings := problemRows(string(report))
	if runErr != nil && len(findings) == 0 {
		findings = []string{gate.Tail(procexec.Output(runErr) + res.Combined())}
	}
	return gate.Judge(len(findings) == 0, reasonUI, map[string]float64{ValueProblems: float64(len(findings))}, findings)
}

func (s *Stage) dropCrawl(ctx context.Context, name, vol string) {
	_, _ = s.ws.Run(ctx, DockerBinary, "rm", "-f", name)
	_, _ = s.ws.Run(ctx, DockerBinary, "volume", "rm", "-f", vol)
}

func (s *Stage) collectCrawl(ctx context.Context, vol, dir string) error {
	res, err := s.ws.Run(ctx, DockerBinary, dockerRun, "--rm", "-v", vol+":"+crawlOut+readOnly, "--entrypoint", tarBinary,
		s.ws.Policy.Images.Browser, "-C", crawlOut, "-cf", "-", ".")
	if err != nil {
		return err
	}
	return unpack(strings.NewReader(res.Stdout), dir)
}

func unpack(archive io.Reader, dir string) error {
	if err := os.MkdirAll(dir, crawlerMode); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	entries := tar.NewReader(archive)
	header, err := entries.Next()
	for ; err == nil; header, err = entries.Next() {
		if err = unpackEntry(root, entries, header); err != nil {
			return err
		}
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func unpackEntry(root *os.Root, body io.Reader, header *tar.Header) error {
	name := path.Clean(header.Name)
	switch header.Typeflag {
	case tar.TypeDir:
		return root.MkdirAll(name, crawlerMode)
	case tar.TypeReg:
		if err := root.MkdirAll(path.Dir(name), crawlerMode); err != nil {
			return err
		}
		file, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, crawlerFileMode)
		if err != nil {
			return err
		}
		_, err = io.CopyN(file, body, header.Size)
		return errors.Join(err, file.Close())
	}
	return nil
}

func (s *Stage) crawlerScripts() (string, error) {
	dir := filepath.Join(s.ws.Out, crawlerDir)
	if err := os.MkdirAll(dir, crawlerMode); err != nil {
		return "", err
	}
	modules, err := loadCrawler()
	if err != nil {
		return "", err
	}
	s.crawler = modules
	for _, module := range modules {
		if err := os.WriteFile(filepath.Join(dir, module.Name), module.Body, crawlerFileMode); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func (s *Stage) crawlArgs(name, vol, scripts string, accepted []string) []string {
	ui := s.ws.App.UI
	mode := crawlQuick
	if s.ws.Mutation.Full {
		mode = crawlFull
	}
	args := make([]string, 0, crawlFixedArgs)
	args = append(args,
		dockerRun, "--name", name, flagNetwork, s.Network, "--cpus", crawlCPUs, "--memory", crawlMemory, "--memory-swap", crawlMemory,
		"--shm-size", crawlShm, flagPids, crawlPids, flagSecurity, noPrivileges,
		"-v", s.ws.Src+":"+crawlSrc+readOnly, "-v", scripts+":"+crawlerMount+readOnly, "-v", vol+":"+crawlOut, "-w", crawlSrc,
		flagEnv, "E2E_BASE="+strings.TrimSuffix(s.InternalURL(""), "/"), flagEnv, "E2E_OUT="+crawlOut,
		flagEnv, "E2E_CHROMIUM="+ui.Chromium, flagEnv, "CRAWL_MODE="+mode, flagEnv, "E2E_APPMAP="+crawlSrc+"/"+ui.Map,
		flagEnv, frozenCrawlEnv+FrozenAt,
	)
	args = append(args, s.visualArgs(accepted)...)
	return append(args, s.ws.Policy.Images.Browser, nodeBinary, nodeTest, crawlerMount+"/"+crawler.File)
}

func (s *Stage) renderKey() string {
	sum := sha256.New()
	parts := make([][]byte, 0, len(s.crawler)+len(s.crawler)+keyTail)
	for _, module := range s.crawler {
		parts = append(parts, []byte(module.Name), module.Body)
	}
	for _, part := range append(parts, []byte(s.ws.Policy.Images.Browser), []byte(FrozenAt)) {
		sum.Write(part)
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func (s *Stage) baselineFits() bool {
	if s.ws.UIBaseline == "" {
		return false
	}
	key, err := os.ReadFile(filepath.Join(s.ws.UIBaseline, RenderKeyFile))
	fits := err == nil && strings.TrimSpace(string(key)) == s.renderKey()
	if !fits {
		s.ws.Log.Info("screens not compared: no baseline, or it was taken by another crawler or browser", "baseline", s.ws.UIBaseline)
	}
	return fits
}

func (s *Stage) visualArgs(accepted []string) []string {
	var args []string
	if s.baselineFits() {
		args = append(args, "-v", s.ws.UIBaseline+":"+crawlBaseline+readOnly, flagEnv, "E2E_BASELINE="+crawlBaseline)
	}
	if len(accepted) > 0 {
		args = append(args, flagEnv, "E2E_VISUAL_ACCEPT="+strings.Join(accepted, ","))
	}
	return args
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
