package policy

import (
	"errors"
	"fmt"
	"slices"
)

const (
	SudoCaddySync  = "caddy-sync"
	SudoHostSync   = "host-sync"
	SudoSelfUpdate = "self-update"
)

var (
	ErrHub       = errors.New("invalid hub section")
	sudoCommands = []string{SudoCaddySync, SudoHostSync, SudoSelfUpdate}
)

type Hub struct {
	SudoUser     string   `yaml:"sudo_user"`
	SudoCommands []string `yaml:"sudo_commands"`
}

func (h Hub) validate() error {
	if h.SudoUser == "" && len(h.SudoCommands) == 0 {
		return nil
	}
	if !hostAccount.MatchString(h.SudoUser) || h.SudoUser == "root" {
		return fmt.Errorf("%w: sudo_user %q must be an unprivileged account name", ErrHub, h.SudoUser)
	}
	seen := map[string]bool{}
	for _, c := range h.SudoCommands {
		if !slices.Contains(sudoCommands, c) || seen[c] {
			return fmt.Errorf("%w: sudo command %q must be one of %v, listed once", ErrHub, c, sudoCommands)
		}
		seen[c] = true
	}
	return nil
}
