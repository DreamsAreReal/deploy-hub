package prune

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var errSim = errors.New("simulated")

type images struct {
	tags      map[string][]string
	removed   []string
	failRm    string
	failList  bool
	failPrune bool
	pruned    bool
	listed    []string
}

func (i *images) ImageTags(_ context.Context, image string) ([]string, error) {
	i.listed = append(i.listed, image)
	if i.failList {
		return nil, errSim
	}
	return i.tags[image], nil
}

func (i *images) RemoveImage(_ context.Context, ref string) error {
	if ref == i.failRm {
		return errSim
	}
	i.removed = append(i.removed, ref)
	return nil
}

func (i *images) PruneDangling(context.Context) error {
	i.pruned = true
	if i.failPrune {
		return errSim
	}
	return nil
}

type pusher struct{ samples []obs.Sample }

func (p *pusher) Push(_ context.Context, s []obs.Sample) { p.samples = append(p.samples, s...) }

func TestKeepSet(t *testing.T) {
	tags := []string{tag1, tag2, tag3, tag4, latestTag}
	state := registry.DeployState{Current: tag3, Previous: tag2}
	got := KeepSet(state, []string{"3333333", "2222222", "4444444", "1111111"}, tags)
	if !reflect.DeepEqual(got, []string{tag3, tag2, tag4}) {
		t.Fatalf("got %v", got)
	}
	got = KeepSet(registry.DeployState{Current: tag1, Previous: tag1}, []string{"9999999", "2222222"}, tags)
	if !reflect.DeepEqual(got, []string{tag1, tag2}) {
		t.Fatalf("got %v", got)
	}
}

const (
	tag1      = "sha-1111111aaa"
	tag2      = "sha-2222222bbb"
	tag3      = "sha-3333333ccc"
	tag4      = "sha-4444444ddd"
	tag5      = "sha-5555555eee"
	webImage  = "img/web"
	oldestRef = webImage + ":" + tag1
)

func appDir(t *testing.T, root, name, conf string, state *registry.DeployState) string {
	t.Helper()
	dir := filepath.Join(root, name)
	testutil.Mkdir(t, dir)
	if conf != "" {
		testutil.Write(t, filepath.Join(dir, "app.conf"), conf)
	}
	if state != nil {
		testutil.NoErr(t, state.Save(dir))
	}
	return dir
}

func setup(t *testing.T) (Pruner, *images, *pusher, string, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	list := "web " + appDir(t, root, "web", "image="+webImage+"\n", &registry.DeployState{Current: tag3, Previous: tag2}) + "\n" +
		"fresh " + appDir(t, root, "fresh", "image=img/fresh\n", nil) + "\n" +
		"noimage " + appDir(t, root, "noimage", "profile=bot\n", nil) + "\n" +
		"noconf " + appDir(t, root, "noconf", "", nil) + "\n"
	appsPath := filepath.Join(root, "apps.list")
	testutil.Write(t, appsPath, list)
	jpath := filepath.Join(root, "deploys.log")
	j := journal.Journal{Path: jpath, Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	for _, sha := range []string{"1111111", "4444444", "2222222", "3333333"} {
		testutil.NoErr(t, j.Append(journal.Entry{App: "web", Sha7: sha, Action: journal.ActionDeploy, Result: journal.ResultOK}))
	}
	im := &images{tags: map[string][]string{webImage: {tag1, tag2, tag3, tag4, tag5, latestTag}}}
	m := &pusher{}
	var logs bytes.Buffer
	return Pruner{Apps: registry.Apps{Path: appsPath}, Journal: j, Images: im, Now: time.Now, Log: slog.New(slog.NewJSONHandler(&logs, nil)), Metrics: m}, im, m, jpath, &logs
}

func TestRunRemovesAllButRunningAndTwoPrevious(t *testing.T) {
	p, im, m, jpath, logs := setup(t)
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.Contains(t, logs.String(), `"msg":"prune result","app":"web","result":"ok"`, `"msg":"prune result","app":"fresh","result":"skip"`)
	testutil.Excludes(t, testutil.Read(t, jpath), "noimage@", "noconf@")
	testutil.Equal(t, im.listed, []string{webImage})
	slices.Sort(im.removed)
	if !reflect.DeepEqual(im.removed, []string{oldestRef, webImage + ":" + tag5}) {
		t.Fatalf("removed %v", im.removed)
	}
	if !im.pruned {
		t.Fatal("dangling layers must be pruned")
	}
	j := testutil.Read(t, jpath)
	if !strings.Contains(j, "web@3333333 prune ok 0s removed=2 kept=3") || !strings.Contains(j, "fresh@- prune skip 0s no-deploy-state") {
		t.Fatalf("journal:\n%s", j)
	}
	if len(m.samples) != 2 {
		t.Fatalf("metrics %v", m.samples)
	}
}

func TestRunToleratesFailures(t *testing.T) {
	p, im, _, _, logs := setup(t)
	im.failRm = oldestRef
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.Contains(t, logs.String(), `"msg":"image not removed"`)
	if len(im.removed) != 1 {
		t.Fatalf("removed %v", im.removed)
	}
	p2, im2, _, _, logs2 := setup(t)
	im2.failList = true
	if err := p2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.Contains(t, logs2.String(), `"msg":"image list failed"`)
	p3, _, _, _, logs3 := setup(t)
	p3.Journal.Path = filepath.Join(t.TempDir(), "dir", "missing", "deploys.log")
	if err := p3.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.Contains(t, logs3.String(), `"msg":"journal append failed"`)
	p4, im4, _, _, logs4 := setup(t)
	im4.failPrune = true
	testutil.NoErr(t, p4.Run(context.Background()))
	testutil.Contains(t, logs4.String(), `"msg":"dangling image prune failed"`)
	testutil.Contains(t, logs4.String(), `"msg":"app.conf unreadable, images kept","app":"noconf"`)
	testutil.Excludes(t, logs4.String(), `"app":"noimage"`)
}

func TestRunErrors(t *testing.T) {
	p, _, _, _, _ := setup(t)
	p.Apps.Path = filepath.Join(t.TempDir(), "missing")
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("missing apps.list")
	}
	p2, _, _, _, _ := setup(t)
	p2.Journal.Path = t.TempDir()
	if err := p2.Run(context.Background()); err == nil {
		t.Fatal("unreadable journal")
	}
}
