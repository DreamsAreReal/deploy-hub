package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	MakeBinary    = "make"
	BusyboxDir    = "busybox.d"
	sourceBusybox = "busybox"
	dotConfig     = ".config"
	linksFile     = "busybox.links"
	configPerm    = 0o600
)

var (
	appletName  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.+-]{0,40}$`)
	hostHarness = []string{"bash", "nl"}
	toolHarness = []string{"bats", JSONFilter}
	lookPath    = exec.LookPath
)

func (i Installer) buildBusybox(ctx context.Context, s Source, config string) error {
	settings, err := os.ReadFile(filepath.Clean(config))
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(i.Dir, buildPattern)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	tree, err := i.unpackSource(ctx, stage, s)
	if err != nil {
		return err
	}
	if werr := writeIn(tree, dotConfig, settings, configPerm); werr != nil {
		return werr
	}
	if rerr := i.runAll(ctx, busyboxSteps(tree)); rerr != nil {
		return rerr
	}
	dest := filepath.Join(i.Dir, BusyboxDir)
	if perr := placeBusybox(tree, dest); perr != nil {
		return perr
	}
	return harness(i.Dir, dest)
}

func harness(tools, dest string) error {
	if err := linkHost(dest); err != nil {
		return err
	}
	return linkTools(tools, dest)
}

func linkHost(dest string) error {
	for _, name := range hostHarness {
		host, err := lookPath(name)
		if err != nil {
			return err
		}
		if err := os.Symlink(host, filepath.Join(dest, name)); err != nil {
			return err
		}
	}
	return nil
}

func linkTools(tools, dest string) error {
	for _, name := range toolHarness {
		if _, err := os.Stat(filepath.Join(tools, name)); err != nil {
			continue
		}
		if err := os.Symlink(filepath.Join("..", name), filepath.Join(dest, name)); err != nil {
			return err
		}
	}
	return nil
}

func busyboxSteps(tree string) []procexec.Command {
	return []procexec.Command{
		{Name: MakeBinary, Args: []string{dirFlag, tree, "oldconfig"}},
		{Name: MakeBinary, Args: []string{dirFlag, tree, "-j2", sourceBusybox}},
		{Name: MakeBinary, Args: []string{dirFlag, tree, linksFile}},
	}
}

func placeBusybox(tree, dest string) error {
	links, err := readIn(tree, linksFile)
	if err != nil {
		return err
	}
	binary, err := readIn(tree, sourceBusybox)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, dirPerm); err != nil {
		return err
	}
	if err := writeIn(dest, sourceBusybox, binary, binPerm); err != nil {
		return err
	}
	return linkApplets(dest, links)
}

func linkApplets(dest string, links []byte) error {
	for _, line := range strings.Split(string(links), "\n") {
		name := filepath.Base(strings.TrimSpace(line))
		if !appletName.MatchString(name) || name == sourceBusybox {
			continue
		}
		if err := os.Symlink(sourceBusybox, filepath.Join(dest, name)); err != nil {
			return err
		}
	}
	return nil
}

func readIn(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(name)
}

func writeIn(dir, name string, data []byte, perm os.FileMode) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.WriteFile(name, data, perm)
}
