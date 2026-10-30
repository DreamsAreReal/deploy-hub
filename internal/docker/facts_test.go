package docker

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	networkShared = "shared"
)

const (
	runningEnv = `["PATH=/usr/bin","PORT=8787","SENTRY_DSN=x","SENTRY_DSN=x","LEFTOVER=1"]`
	bakedEnv   = `["PATH=/usr/bin","PORT=8080"]`
	imageID    = "sha256:abc"
)

func factsDocker(fail string) func(procexec.Command) (procexec.Result, error) {
	return func(cmd procexec.Command) (procexec.Result, error) {
		line := strings.Join(cmd.Args, " ")
		if fail != "" && strings.Contains(line, fail) {
			return procexec.Result{}, errBoom
		}
		switch {
		case strings.Contains(line, formatImage):
			return procexec.Result{Stdout: imageID + "\n"}, nil
		case strings.HasPrefix(line, "image inspect"):
			return procexec.Result{Stdout: bakedEnv}, nil
		case strings.Contains(line, formatEnv):
			return procexec.Result{Stdout: runningEnv}, nil
		}
		return procexec.Result{Stdout: "8787/tcp 9002\n8787/tcp 9002\n9090/tcp 9003\n\n"}, nil
	}
}

func TestRunEnvListsWhatTheContainerGotBeyondItsImage(t *testing.T) {
	c, f := client(factsDocker(""))
	names, err := c.RunEnv(context.Background(), "tasks")
	testutil.NoErr(t, err)
	testutil.Equal(t, names, []string{"PORT", "SENTRY_DSN", "LEFTOVER"})
	want := []string{"docker inspect --format {{.Image}} tasks", "docker inspect --format {{json .Config.Env}} tasks", "docker image inspect --format {{json .Config.Env}} sha256:abc"}
	if !reflect.DeepEqual(f.Lines(), want) {
		t.Fatalf("calls %v", f.Lines())
	}
	ports, err := c.BoundPorts(context.Background(), "tasks")
	testutil.NoErr(t, err)
	testutil.Equal(t, ports, []int{8787, 9090})
}

func TestRunFactsFailWhenDockerDoes(t *testing.T) {
	for _, step := range []string{formatImage, "inspect --format {{json", "image inspect"} {
		c, _ := client(factsDocker(step))
		if _, err := c.RunEnv(context.Background(), "tasks"); err == nil {
			t.Errorf("%s: no error", step)
		}
	}
	c, _ := client(reply("", errBoom))
	if _, err := c.BoundPorts(context.Background(), "tasks"); err == nil {
		t.Error("ports without docker")
	}
	broken, _ := client(reply("not json", nil))
	if _, err := broken.envOf(context.Background(), "inspect", "tasks"); err == nil {
		t.Error("garbage env accepted")
	}
}

func TestProjectNetworksGroupsTheNetworksOfEveryComposeProject(t *testing.T) {
	c, f := client(reply("tasks\tshared,tasks_default\nobservability\tobservability_default\nobservability\tshared\n\tbridge\nrouter-bot\tshared,shared\n\n", nil))
	networks, err := c.ProjectNetworks(context.Background())
	testutil.NoErr(t, err)
	testutil.Equal(t, networks, map[string][]string{"tasks": {networkShared, "tasks_default"}, "observability": {"observability_default", networkShared}, "router-bot": {networkShared}})
	testutil.Equal(t, f.Lines(), []string{"docker ps --format " + formatNetworks})
	broken, _ := client(reply("", errBoom))
	_, err = broken.ProjectNetworks(context.Background())
	testutil.Fails(t, err)
}
