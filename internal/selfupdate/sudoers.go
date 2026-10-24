package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	DefaultSudoers = "/etc/sudoers.d/deploy-hub"
	VisudoCommand  = "visudo"
	sudoersPerm    = 0o440
	noArguments    = `""`
	anyArguments   = "*"
	sudoersLine    = "%s ALL=(root) NOPASSWD: %s %s %s\n"
	sudoersStaging = ".deploy-hub.new"
)

var ErrSudoers = errors.New("generated sudoers rule failed visudo, the previous one is kept")

type Sudoers struct {
	Path   string
	Binary string
	Hub    policy.Hub
	Exec   procexec.Runner
}

func (s Sudoers) Render() string {
	var b strings.Builder
	for _, c := range s.Hub.SudoCommands {
		args := noArguments
		if c == policy.SudoHostSync {
			args = anyArguments
		}
		fmt.Fprintf(&b, sudoersLine, s.Hub.SudoUser, s.Binary, c, args)
	}
	return b.String()
}

func (s Sudoers) Sync(ctx context.Context) error {
	want := []byte(s.Render())
	if have, err := os.ReadFile(filepath.Clean(s.Path)); err == nil && bytes.Equal(have, want) {
		return nil
	}
	staged := filepath.Join(filepath.Dir(s.Path), sudoersStaging)
	_ = os.Remove(staged)
	if err := writeMode(staged, want, sudoersPerm); err != nil {
		return err
	}
	if _, err := s.Exec.Run(ctx, procexec.Command{Name: VisudoCommand, Args: []string{"-cf", staged}}); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("%w: %s", ErrSudoers, procexec.Output(err))
	}
	return os.Rename(staged, s.Path)
}
