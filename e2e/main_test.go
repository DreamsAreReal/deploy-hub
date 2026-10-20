package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var hubBinary string

func TestMain(m *testing.M) {
	if code, handled := dispatchFake(); handled {
		os.Exit(code)
	}
	os.Exit(buildAndRun(m))
}

func dispatchFake() (int, bool) {
	f := fake{state: os.Getenv(envState)}
	switch filepath.Base(os.Args[0]) {
	case binFakeApp:
		return serve(), true
	case binDocker:
		return f.docker(os.Args[1:]), true
	case binSudo:
		return sudo(os.Args[1:]), true
	case binCaddy, binSysctl:
		f.logCall(os.Args)
		return 0, true
	}
	return 0, false
}

func buildAndRun(m *testing.M) int {
	dir, err := os.MkdirTemp("", "deploy-hub-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	hubBinary = filepath.Join(dir, binHub)
	build := exec.CommandContext(context.Background(), "go", "build", "-o", hubBinary, "../cmd/deploy-hub")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if berr := build.Run(); berr != nil {
		fmt.Fprintln(os.Stderr, "build failed:", berr)
		return 1
	}
	self, err := os.Executable()
	if err == nil {
		err = os.Symlink(self, filepath.Join(dir, binFakeApp))
	}
	if err == nil {
		err = os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}
