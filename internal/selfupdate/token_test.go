package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func tokenFile(t *testing.T, body string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github.token")
	testutil.NoErr(t, os.WriteFile(path, []byte(body), perm))
	testutil.NoErr(t, os.Chmod(path, perm))
	return path
}

func TestReadTokenTrimsTheLine(t *testing.T) {
	got, err := ReadToken(tokenFile(t, pat+"\n", 0o600), os.Geteuid())
	testutil.NoErr(t, err)
	testutil.Equal(t, got, pat)
	got, err = ReadToken(tokenFile(t, pat, 0o400), os.Geteuid())
	testutil.NoErr(t, err)
	testutil.Equal(t, got, pat)
}

func TestReadTokenRefusesMissingOrEmptyTokens(t *testing.T) {
	_, err := ReadToken(filepath.Join(t.TempDir(), "github.token"), os.Geteuid())
	testutil.Is(t, err, ErrNoToken)
	for name, body := range map[string]string{"empty": "", "blank": " \n", "bearer line": "Authorization: Bearer " + pat, "two lines": pat + "\n" + pat} {
		t.Run(name, func(t *testing.T) {
			_, err := ReadToken(tokenFile(t, body, 0o600), os.Geteuid())
			testutil.Is(t, err, ErrNoToken)
		})
	}
}

func TestReadTokenRefusesOpenOrOddFiles(t *testing.T) {
	for name, perm := range map[string]os.FileMode{"group": 0o640, "other": 0o604, "world": 0o644} {
		t.Run(name, func(t *testing.T) {
			_, err := ReadToken(tokenFile(t, pat, perm), os.Geteuid())
			testutil.Is(t, err, ErrTokenFile)
		})
	}
	dir := t.TempDir()
	_, err := ReadToken(dir, os.Geteuid())
	testutil.Is(t, err, ErrTokenFile)
	link := filepath.Join(dir, "link")
	testutil.NoErr(t, os.Symlink(tokenFile(t, pat, 0o600), link))
	_, err = ReadToken(link, os.Geteuid())
	testutil.Is(t, err, ErrTokenFile)
	_, err = ReadToken(tokenFile(t, strings.Repeat("a", maxToken+1), 0o600), os.Geteuid())
	testutil.Is(t, err, ErrTokenFile)
	_, err = ReadToken(tokenFile(t, strings.Repeat("a", maxToken), 0o600), os.Geteuid())
	testutil.NoErr(t, err)
}

func refused(t *testing.T, path string, owner int, want error) {
	t.Helper()
	got, err := ReadToken(path, owner)
	testutil.Is(t, err, want)
	testutil.Equal(t, got, "")
}

func TestReadTokenReturnsNothingOnEveryRefusal(t *testing.T) {
	dir := t.TempDir()
	refused(t, filepath.Join(dir, "missing"), os.Geteuid(), ErrNoToken)
	refused(t, tokenFile(t, " \n", 0o600), os.Geteuid(), ErrNoToken)
	refused(t, tokenFile(t, pat, 0o644), os.Geteuid(), ErrTokenFile)
	file := tokenFile(t, pat, 0o600)
	got, err := ReadToken(filepath.Join(file, "inside"), os.Geteuid())
	testutil.Is(t, err, syscall.ENOTDIR)
	testutil.Equal(t, got, "")
	if os.Geteuid() != 0 {
		got, err = ReadToken(tokenFile(t, pat, 0o000), os.Geteuid())
		testutil.Is(t, err, os.ErrPermission)
		testutil.Equal(t, got, "")
	}
}

func TestReadTokenRefusesAFileOfAnotherUser(t *testing.T) {
	_, err := ReadToken(tokenFile(t, pat, 0o600), os.Geteuid()+1)
	testutil.Is(t, err, ErrTokenFile)
}
