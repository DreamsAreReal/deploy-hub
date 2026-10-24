package e2e

import (
	"os"
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
	RunsOn      any               `yaml:"runs-on"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflow struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) (workflow, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(filepath.Join(workflows, name)))
	testutil.NoErr(t, err)
	var w workflow
	testutil.NoErr(t, yaml.Unmarshal(data, &w))
	return w, string(data)
}

func TestOnlyTheHubPublishesReleases(t *testing.T) {
	w, raw := readWorkflow(t, "deploy.yml")
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
	w, raw := readWorkflow(t, "deploy.yml")
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
	w, _ := readWorkflow(t, "deploy.yml")
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
