package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	workflows    = "../.github/workflows"
	releaseToken = "secrets.RELEASE_TOKEN"
)

type workflowStep struct {
	Name     string            `yaml:"name"`
	If       string            `yaml:"if"`
	Continue any               `yaml:"continue-on-error"`
	Env      map[string]string `yaml:"env"`
	Run      string            `yaml:"run"`
}

type workflowJob struct {
	Env         map[string]string `yaml:"env"`
	RunsOn      any               `yaml:"runs-on"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflow struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

func readDeployWorkflow(t *testing.T) (workflow, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(filepath.Join(workflows, "deploy.yml")))
	testutil.NoErr(t, err)
	var w workflow
	testutil.NoErr(t, yaml.Unmarshal(data, &w))
	return w, string(data)
}

func TestOnlyTheHubPublishesReleases(t *testing.T) {
	w, raw := readDeployWorkflow(t)
	testutil.Equal(t, len(w.Jobs), 1)
	job := w.Jobs["deploy"]
	testutil.Equal(t, job.Permissions["contents"], "read")
	var release []workflowStep
	for _, s := range job.Steps {
		if strings.Contains(s.If+s.Run+strings.Join(mapValues(s.Env), " "), releaseToken) {
			release = append(release, s)
		}
	}
	testutil.Equal(t, len(release), 1)
	testutil.Equal(t, strings.Count(raw, releaseToken), 1)
	step := release[0]
	testutil.Contains(t, step.Run, "gh release create")
	testutil.Contains(t, step.If, "github.repository == 'DreamsAreReal/deploy-hub'", "inputs.app == 'deploy-hub'",
		"github.event_name == 'push'", "github.ref == 'refs/heads/main'", "steps.gate.outcome == 'success'")
	testutil.Excludes(t, step.If, "||", "always()", "!")
	for _, s := range job.Steps {
		if s.Name != step.Name {
			testutil.Excludes(t, s.Run, "gh release")
		}
	}
}

func TestDeployRunsOnlyOnTheEURunner(t *testing.T) {
	w, raw := readDeployWorkflow(t)
	testutil.Equal(t, w.Jobs["deploy"].RunsOn, any(`${{ fromJSON('["self-hosted", "linux", "eu"]') }}`))
	testutil.Excludes(t, raw, "ubuntu-latest", "github-hosted", "repository.fork", "repository.private", "DEPLOY_HUB_RUNNER")
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

type callerJob struct {
	Uses    string            `yaml:"uses"`
	With    map[string]string `yaml:"with"`
	Secrets map[string]string `yaml:"secrets"`
	RunsOn  any               `yaml:"runs-on"`
}

func TestTheHubGatesItselfThroughDeployYML(t *testing.T) {
	data, err := os.ReadFile(filepath.Clean(filepath.Join(workflows, "ci.yml")))
	testutil.NoErr(t, err)
	var ci struct {
		Jobs map[string]callerJob `yaml:"jobs"`
	}
	testutil.NoErr(t, yaml.Unmarshal(data, &ci))
	testutil.Equal(t, len(ci.Jobs), 1)
	for _, job := range ci.Jobs {
		testutil.Equal(t, job.Uses, "DreamsAreReal/deploy-hub/.github/workflows/deploy.yml@main")
		testutil.Equal(t, job.With["app"], "deploy-hub")
		testutil.Equal(t, job.Secrets["RELEASE_TOKEN"], "${{ github.event_name == 'push' && "+releaseToken+" || '' }}")
		testutil.Equal(t, len(job.Secrets), 1)
		testutil.Equal(t, job.RunsOn, nil)
	}
	testutil.Excludes(t, string(data), "runs-on")
}

func TestNothingReachesProductionBeforeTheGatePasses(t *testing.T) {
	w, _ := readDeployWorkflow(t)
	steps := w.Jobs["deploy"].Steps
	gate := -1
	for i, s := range steps {
		testutil.Equal(t, s.Continue, nil)
		if s.Name == "Quality gate" {
			gate = i
		}
	}
	testutil.True(t, gate >= 0, "no Quality gate step")
	for _, name := range []string{"Push the gated image", "Deploy on the VPS", "Publish the hub release"} {
		found := false
		for i, s := range steps {
			if s.Name != name {
				continue
			}
			found = true
			testutil.True(t, i > gate, name+" runs before the gate")
			testutil.Excludes(t, s.If, "always()", "failure()", "cancelled()", "!(", "||")
		}
		testutil.True(t, found, "no step "+name)
	}
}

const umaskPrivateCheckout = `umask 0077
mkdir -p "$CONTEXT/web/templates"
echo x > "$CONTEXT/web/templates/page.html"
echo x > "$CONTEXT/run.sh"
chmod u+x "$CONTEXT/run.sh"
docker() { for a; do ctx=$a; done; find "$ctx" -mindepth 1 -printf '%m %P\n' | sort > "$MODES"; }
`

func TestTheImageIsBuiltFromAContextReadableWhateverTheRunnerUmask(t *testing.T) {
	w, _ := readDeployWorkflow(t)
	build := ""
	for _, s := range w.Jobs["deploy"].Steps {
		if s.Name == "Build image" {
			build = s.Run
		}
	}
	testutil.True(t, build != "", "no Build image step")
	dir := t.TempDir()
	modes := filepath.Join(dir, "modes")
	cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", umaskPrivateCheckout+build)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "MODES=" + modes, "CONTEXT=" + filepath.Join(dir, "src"), "DOCKERFILE=Dockerfile",
		"APP=web", "GITHUB_REPOSITORY_OWNER=Owner", "GITHUB_SHA=abc", "GITHUB_OUTPUT=" + filepath.Join(dir, "out")}
	out, err := cmd.CombinedOutput()
	testutil.NoErr(t, err)
	testutil.Equal(t, string(out), "")
	got, err := os.ReadFile(filepath.Clean(modes))
	testutil.NoErr(t, err)
	testutil.Equal(t, string(got), "644 web/templates/page.html\n755 run.sh\n755 web\n755 web/templates\n")
}

func TestAFullGateRunOnlyGatesAndNeverReachesTheVPS(t *testing.T) {
	w, raw := readDeployWorkflow(t)
	var doc struct {
		On struct {
			Call struct {
				Inputs map[string]struct {
					Type    string `yaml:"type"`
					Default any    `yaml:"default"`
				} `yaml:"inputs"`
			} `yaml:"workflow_call"`
		} `yaml:"on"`
	}
	testutil.NoErr(t, yaml.Unmarshal([]byte(raw), &doc))
	full := doc.On.Call.Inputs["full"]
	testutil.Equal(t, full.Type, "boolean")
	testutil.Equal(t, full.Default, any(false))
	job := w.Jobs["deploy"]
	testutil.True(t, strings.HasPrefix(job.Env["GATE_ONLY"], "${{ inputs.full || "), "a full run is gate only")
	for _, s := range job.Steps {
		if strings.Contains(s.Run, "ssh ") || strings.Contains(s.Run, "docker push") || strings.Contains(s.Run, `"$SSH_KEY"`) {
			testutil.Contains(t, s.If, "env.GATE_ONLY != 'true'")
		}
	}
}
