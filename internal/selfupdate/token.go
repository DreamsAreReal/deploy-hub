package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	DefaultPATFile = "/etc/deploy-hub/github.token"
	tokenOpenBits  = 0o077
	maxToken       = 4 << 10
)

var ErrTokenFile = errors.New("token file must be a regular file of its reader (root on the hub) with mode 600 or 400")

func ReadToken(path string, owner int) (string, error) {
	fi, err := os.Lstat(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: %s is missing", ErrNoToken, path)
	}
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&tokenOpenBits != 0 || !ok || int(st.Uid) != owner || fi.Size() > maxToken {
		return "", fmt.Errorf("%w: %s", ErrTokenFile, path)
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("%w: %s holds no single token", ErrNoToken, path)
	}
	return token, nil
}
