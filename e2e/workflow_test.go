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
	stepDeploy   = "Deploy on the VPS"
	systemPath   = "PATH=/usr/bin:/bin"
	appWeb       = "APP=web"
	stepPush     = "Push the gated image"
	shaABC       = "GITHUB_SHA=abc"
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
	for _, name := range []string{stepPush, stepDeploy, "Publish the hub release"} {
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
docker() {
	if [ "$1" = image ]; then echo sha256:built; return; fi
	for a; do ctx=$a; done; find "$ctx" -mindepth 1 -printf '%m %P\n' | sort > "$MODES"
}
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
	cmd.Env = []string{systemPath, "MODES=" + modes, "CONTEXT=" + filepath.Join(dir, "src"), "DOCKERFILE=Dockerfile",
		appWeb, "GITHUB_REPOSITORY_OWNER=Owner", shaABC, "GITHUB_OUTPUT=" + filepath.Join(dir, "out")}
	out, err := cmd.CombinedOutput()
	testutil.NoErr(t, err)
	testutil.Equal(t, string(out), "")
	got, err := os.ReadFile(filepath.Clean(modes))
	testutil.NoErr(t, err)
	testutil.Equal(t, string(got), "644 web/templates/page.html\n755 run.sh\n755 web\n755 web/templates\n")
}

const fakeGateTools = `deploy-hub() { printf '%s\n' "$*" > "$ARGS"; }
sleep() { return 1; }
`

func gateArgs(t *testing.T, gate workflowStep, built string) string {
	t.Helper()
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", fakeGateTools+gate.Run)
	cmd.Env = []string{systemPath, "TOOLCHAIN=" + dir, "ARGS=" + args, "BUILT=" + built, appWeb,
		"IMAGE=ghcr.io/owner/web:sha-abc", "GITHUB_REPOSITORY=Owner/web", shaABC, "POLICY=/p.yaml",
		"GITHUB_WORKSPACE=/src", "RUNNER_TEMP=" + dir}
	out, err := cmd.CombinedOutput()
	testutil.Equal(t, string(out), "")
	testutil.NoErr(t, err)
	got, err := os.ReadFile(filepath.Clean(args))
	testutil.NoErr(t, err)
	return string(got)
}

const fakeGateWitness = `deploy-hub() {
	env > "$SEEN"
	if [ -e "$RUNNER_TEMP/ssh/deploy_key" ]; then echo key >> "$SEEN"; fi
}
sleep() { return 1; }
`

func stepsByName(t *testing.T) ([]workflowStep, map[string]int) {
	t.Helper()
	w, _ := readDeployWorkflow(t)
	steps := w.Jobs["deploy"].Steps
	index := map[string]int{}
	for i, s := range steps {
		index[s.Name] = i
	}
	return steps, index
}

func TestTheAppCodeRunsWithoutTheDeployKeyOrAnIDTokenRequest(t *testing.T) {
	steps, index := stepsByName(t)
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, "ssh", "deploy_key"), "key")
	seen := filepath.Join(dir, "seen")
	cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", fakeGateWitness+steps[index["Quality gate"]].Run)
	cmd.Env = []string{systemPath, "TOOLCHAIN=" + dir, "SEEN=" + seen, "BUILT=false", appWeb, "GITHUB_REPOSITORY=Owner/web", shaABC,
		"POLICY=/p.yaml", "GITHUB_WORKSPACE=/src", "RUNNER_TEMP=" + dir,
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-token", "ACTIONS_ID_TOKEN_REQUEST_URL=https://token.example"}
	out, err := cmd.CombinedOutput()
	testutil.NoErr(t, err)
	testutil.Equal(t, string(out), "")
	env, err := os.ReadFile(filepath.Clean(seen))
	testutil.NoErr(t, err)
	testutil.Contains(t, string(env), "RUNNER_TEMP="+dir)
	testutil.Excludes(t, string(env), "ACTIONS_ID_TOKEN_REQUEST", "\nkey\n")
	again := steps[index["Connect to the VPS after the gate"]]
	testutil.True(t, index["Connect to the VPS after the gate"] == index["Quality gate"]+1, "the key comes back right after the gate")
	testutil.Equal(t, again.If, "always() && env.GATE_ONLY != 'true'")
	for _, name := range []string{"Upload the mutation verdicts to the hub", "Report the verdict to the hub", stepPush, stepDeploy} {
		testutil.True(t, index[name] > index["Connect to the VPS after the gate"], name+" runs before the key is back")
	}
}

const fakePushTools = `docker() {
	if [ "$1 $2" = "image inspect" ]; then echo "$NOW_ID"; return; fi
	echo "$*" >> "$CALLS"
}
`

func pushWith(t *testing.T, builtID, nowID string) (string, error) {
	t.Helper()
	steps, index := stepsByName(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	testutil.Write(t, calls, "")
	cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", fakePushTools+steps[index[stepPush]].Run)
	cmd.Env = []string{systemPath, "CALLS=" + calls, "BUILT_ID=" + builtID, "NOW_ID=" + nowID, "IMAGE=ghcr.io/owner/web", "TAG=sha-abc",
		"GH_TOKEN=token", "ACTOR=owner", "RUNNER_TEMP=" + dir, "GITHUB_OUTPUT=" + filepath.Join(dir, "out")}
	out, err := cmd.CombinedOutput()
	got, rerr := os.ReadFile(filepath.Clean(calls))
	testutil.NoErr(t, rerr)
	return string(out) + string(got), err
}

func TestThePushRefusesAnImageThatChangedAfterTheBuild(t *testing.T) {
	pushed, err := pushWith(t, "sha256:built", "sha256:built")
	testutil.NoErr(t, err)
	testutil.Contains(t, pushed, "push ghcr.io/owner/web:sha-abc")
	for _, ids := range [][2]string{{"sha256:built", "sha256:swapped"}, {"", ""}} {
		refused, err := pushWith(t, ids[0], ids[1])
		testutil.True(t, err != nil, "an image changed after the build was pushed")
		testutil.Contains(t, refused, "nothing is pushed")
		testutil.Excludes(t, refused, "push ghcr.io", "login")
	}
	steps, index := stepsByName(t)
	testutil.Equal(t, steps[index[stepPush]].Env["BUILT_ID"], "${{ steps.image.outputs.id }}")
	testutil.Contains(t, steps[index["Build image"]].Run, `echo "id=$id"`)
}

func TestAKindBinaryAppIsGatedWithoutAnImageAndNeverDeployed(t *testing.T) {
	w, raw := readDeployWorkflow(t)
	testutil.Contains(t, raw, "      image:\n", "        type: boolean\n        default: true\n")
	steps := map[string]workflowStep{}
	for _, s := range w.Jobs["deploy"].Steps {
		steps[s.Name] = s
	}
	testutil.Equal(t, steps["Build image"].If, "env.HUB != 'true' && inputs.image")
	gate := steps["Quality gate"]
	testutil.Equal(t, gate.Env["BUILT"], "${{ steps.image.outcome == 'success' }}")
	testutil.Contains(t, gateArgs(t, gate, "true"), "--image ghcr.io/owner/web:sha-abc")
	testutil.Excludes(t, gateArgs(t, gate, "false"), "--image")
	testutil.Contains(t, raw, "GATE_ONLY: ${{ github.event_name == 'pull_request' || !inputs.image || ")
	for _, name := range []string{stepPush, stepDeploy, "Connect to the VPS"} {
		testutil.Contains(t, steps[name].If, "env.GATE_ONLY != 'true'")
	}
}

func TestOnlyADeployRunOfMainFeedsTheMutationCacheAndTheFullRun(t *testing.T) {
	w, _ := readDeployWorkflow(t)
	steps := map[string]workflowStep{}
	for _, s := range w.Jobs["deploy"].Steps {
		steps[s.Name] = s
	}
	for _, name := range []string{"Upload the mutation verdicts to the hub", "Keep the source for the hub's full mutation run"} {
		testutil.Contains(t, steps[name].If, "env.GATE_ONLY != 'true'")
	}
	testutil.Contains(t, steps["Keep the source for the hub's full mutation run"].If, "runner.environment == 'self-hosted'")
}
