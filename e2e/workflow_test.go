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
	Name string            `yaml:"name"`
	If   string            `yaml:"if"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
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

// The hub release is published from the one job of deploy.yml that every app
// calls. Only a push to main of the hub repo with a passed gate may reach the
// step, and only that step sees RELEASE_TOKEN; the GITHUB_TOKEN of app jobs
// stays read-only.
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

// The one job of deploy.yml runs on the EU runner, with no GitHub-hosted branch
// left for public repos, forks or a repo variable.
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
