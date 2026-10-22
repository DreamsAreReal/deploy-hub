package runner

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func botEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.sim.ownHealth = true
	e.conf(registry.AppConf{Profile: registry.ProfileBot, Image: image})
	e.sim.startOld(goodTag)
	e.state(registry.DeployState{Current: goodTag})
	return e
}

func indexOf(lines []string, substr string) int {
	return slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, substr) })
}

func lastIndex(lines []string, line string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] == line {
			return i
		}
	}
	return -1
}

func TestPortlessAppOverlapsOldAndNewInstance(t *testing.T) {
	e := botEnv(t)
	if code := e.dispatch("deploy web "+good2Tag, ""); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.err)
	}
	compose := "docker compose -f " + filepath.Join(e.appDir, "docker-compose.yml")
	lines := e.exec.Lines()
	run, up, stop, rm := indexOf(lines, compose+" run -d --no-deps --name web-bg app"), indexOf(lines, compose+" up -d"),
		slices.Index(lines, "docker stop web-bg"), lastIndex(lines, "docker rm -f web-bg")
	testutil.True(t, run >= 0 && up > run && stop > up && rm > stop, "standby starts before the main recreate and is stopped gracefully after it: "+strings.Join(lines, "\n"))
	testutil.Equal(t, e.exec.Count(" -p "), 0)
	testutil.Equal(t, e.sim.images[mainName], image+":"+good2Tag)
	testutil.True(t, !e.sim.running["web-bg"], "standby left running")
	testutil.Equal(t, e.router.syncs, 0)
	testutil.Equal(t, len(e.slept), 0)
	testutil.Equal(t, registry.LoadState(e.appDir), registry.DeployState{Current: good2Tag, Previous: goodTag})
	e.expectOut(e.out, "blue-green: starting standby web-bg next to the live instance (no port)", msgCutover)
	e.expectLog("health gate finished", "standby cleaned up")
}

func TestPortlessBrokenReleaseNeverTouchesTheLiveInstance(t *testing.T) {
	e := botEnv(t)
	if code := e.dispatch("deploy web "+badTag, ""); code != ExitFail {
		t.Fatalf("exit %d", code)
	}
	testutil.True(t, e.sim.running[mainName] && e.sim.images[mainName] == image+":"+goodTag, "live instance must keep running the old image")
	testutil.Equal(t, e.exec.Count(" up -d"), 0)
	testutil.Equal(t, e.exec.Count("docker stop"), 0)
	e.expectOut(e.err, msgStandbyUnhealthy, msgZeroDowntime, reasonCompose)
	e.expectJournal("web@dead000 deploy fail")
}

func TestOverlapLabelFalseRecreatesInPlace(t *testing.T) {
	for name, setup := range map[string]func(e *env){
		"compose label": func(e *env) {
			e.write(filepath.Join(e.appDir, "docker-compose.yml"), "services:\n  app:\n    labels:\n      deploy-hub.overlap: \"false\"\n")
		},
		"image label": func(e *env) { e.sim.imageLabel = labelNo },
		"image label, compose silent": func(e *env) {
			e.write(filepath.Join(e.appDir, "docker-compose.yml"), "services:\n  app: {}\n")
			e.sim.imageLabel = labelNo
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, e := range []*env{botEnv(t), newEnv(t)} {
				e.sim.startOld(goodTag)
				setup(e)
				if code := e.dispatch("deploy web "+good2Tag, ""); code != ExitOK {
					t.Fatalf("exit %d\n%s", code, e.err)
				}
				testutil.Equal(t, e.exec.Count(" run -d"), 0)
				testutil.Equal(t, e.sim.images[mainName], image+":"+good2Tag)
				e.expectOut(e.out, msgOverlapOff)
				testutil.Contains(t, e.logs.String(), `"reason":"overlap disabled by label"`)
			}
		})
	}
}

func TestBYOCWorkerOverlapsItsMainService(t *testing.T) {
	e := newEnv(t)
	e.sim.mainName = appName
	e.sim.startOld(goodTag)
	e.conf(registry.AppConf{Mode: registry.ModeBYOC, Main: "worker", HMode: registry.HealthProcess, Image: image})
	e.write(filepath.Join(e.appDir, "docker-compose.yml"), "services:\n  worker:\n    labels: [deploy-hub.overlap=true]\n")
	if code := e.dispatch("deploy web "+good2Tag, ""); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.err)
	}
	testutil.Equal(t, e.exec.Count(" run -d --no-deps --name web-bg worker"), 1)
	testutil.Equal(t, e.sim.images[appName], image+":"+good2Tag)
}

func TestComposeOverlapLabelWinsOverTheImage(t *testing.T) {
	e := botEnv(t)
	e.write(filepath.Join(e.appDir, "docker-compose.yml"), "services:\n  app:\n    labels: [deploy-hub.overlap=true]\n")
	e.sim.imageLabel = labelNo
	if code := e.dispatch("deploy web "+good2Tag, ""); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.err)
	}
	testutil.Equal(t, e.exec.Count(" run -d --no-deps --name web-bg app"), 1)
}

func TestPortlessStatefulAppIsRecreatedInPlace(t *testing.T) {
	e := botEnv(t)
	e.sim.rwMounts = rwData
	if code := e.dispatch("deploy web "+good2Tag, ""); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, e.err)
	}
	_, ran := e.exec.Find(" run ")
	testutil.True(t, !ran, "no standby next to an instance that keeps state")
	testutil.Equal(t, e.sim.images[mainName], image+":"+good2Tag)
	e.expectOut(e.out, "blue-green: "+mainName+" keeps state on a writable mount — recreating in place")
}
