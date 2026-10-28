package runner

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/oidc"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	baselinePutCmd = VerbBaselinePut + " " + appName + " " + goodTag
	baselineGetCmd = VerbBaselineGet + " " + appName + " " + goodTag
	nextTag        = "sha-bbbbbbb2222"
	boardShot      = "board.png"
	boardNarrow    = "board-360.png"
)

var (
	keyA = strings.Repeat("a", 64)
	keyB = strings.Repeat("b", 64)
)

func keyed(key string, lines ...string) string {
	return "key=" + key + "\n" + strings.Join(lines, "")
}

func png(body string) []byte {
	return []byte(pngMagic + body)
}

func shotLine(name string, data []byte) string {
	return "shot=" + name + shotSeparator + base64.StdEncoding.EncodeToString(data) + "\n"
}

func (e *env) baseline() (string, map[string]string) {
	e.t.Helper()
	from, shots := "", map[string]string{}
	for _, line := range strings.Split(e.out.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "from="); ok {
			from = v
		}
		if v, ok := strings.CutPrefix(line, "key="); ok {
			from += " " + v
		}
		if v, ok := strings.CutPrefix(line, "shot="); ok {
			name, encoded, _ := strings.Cut(v, shotSeparator)
			data, err := base64.StdEncoding.DecodeString(encoded)
			testutil.NoErr(e.t, err)
			shots[name] = string(data)
		}
	}
	return from, shots
}

func TestUIBaselineIsKeptOnlyForTheDeployedTagAndServedBack(t *testing.T) {
	e := newEnv(t)
	testutil.Equal(t, e.dispatch(baselineGetCmd, ""), ExitOK)
	testutil.Equal(t, e.out.String(), "")
	e.state(registry.DeployState{Current: goodTag})
	testutil.Equal(t, e.dispatch(baselinePutCmd, keyed(keyA, shotLine(boardShot, png("wide")), shotLine(boardNarrow, png("narrow")))), ExitOK)
	testutil.Contains(t, e.out.String(), "ui baseline: 2 shots of "+goodTag+" stored")
	testutil.True(t, e.locker.released, "the lock is released")
	e.out.Reset()
	testutil.Equal(t, e.dispatch(baselineGetCmd, ""), ExitOK)
	from, shots := e.baseline()
	testutil.Equal(t, from, goodTag+" "+keyA)
	testutil.Equal(t, shots, map[string]string{boardShot: string(png("wide")), boardNarrow: string(png("narrow"))})
	want := e.ident.wants[len(e.ident.wants)-1]
	testutil.Equal(t, []string{want.Repository, want.WorkflowRef, want.Commit}, []string{appRepo, hubRef, strings.TrimPrefix(goodTag, "sha-")})
	e.expectLog("ui baseline served")
}

func TestUIBaselineOfTheSameTagMergesAndANewTagReplacesIt(t *testing.T) {
	e := newEnv(t)
	e.state(registry.DeployState{Current: goodTag})
	testutil.Equal(t, e.dispatch(baselinePutCmd, keyed(keyA, shotLine(boardShot, png("wide")))), ExitOK)
	testutil.Equal(t, e.dispatch(baselinePutCmd, keyed(keyA, shotLine(boardNarrow, png("narrow")))), ExitOK)
	e.out.Reset()
	testutil.Equal(t, e.dispatch(baselineGetCmd, ""), ExitOK)
	_, merged := e.baseline()
	testutil.Equal(t, len(merged), 2)
	testutil.Equal(t, e.dispatch(baselinePutCmd, keyed(keyB, shotLine(boardNarrow, png("browser")))), ExitOK)
	e.out.Reset()
	testutil.Equal(t, e.dispatch(baselineGetCmd, ""), ExitOK)
	from, shots := e.baseline()
	testutil.Equal(t, from, goodTag+" "+keyB)
	testutil.Equal(t, shots, map[string]string{boardNarrow: string(png("browser"))})
	e.state(registry.DeployState{Current: nextTag, Previous: goodTag})
	testutil.Equal(t, e.dispatch(VerbBaselinePut+" "+appName+" "+nextTag, keyed(keyB, shotLine(boardShot, png("new")))), ExitOK)
	e.out.Reset()
	testutil.Equal(t, e.dispatch(baselineGetCmd, ""), ExitOK)
	from, shots = e.baseline()
	testutil.Equal(t, from, nextTag+" "+keyB)
	testutil.Equal(t, shots, map[string]string{boardShot: string(png("new"))})
	_, err := os.Stat(e.svc.Paths.UIBaseline(appName) + baselineOldDir)
	testutil.True(t, os.IsNotExist(err), "the replaced set is gone")
}

func TestUIBaselineRefusesTagsThatAreNotLiveAndMalformedShots(t *testing.T) {
	e := newEnv(t)
	e.state(registry.DeployState{Current: nextTag})
	testutil.Equal(t, e.dispatch(baselinePutCmd, keyed(keyA, shotLine(boardShot, png("x")))), ExitFail)
	e.expectOut(e.err, msgBaselineStale)
	e.state(registry.DeployState{Current: goodTag})
	for _, stdin := range []string{
		keyed(keyA),
		keyed(keyA, shotLine("Board.png", png("x"))),
		keyed(keyA, shotLine("board.jpg", png("x"))),
		keyed(keyA, shotLine("../board.png", png("x"))),
		keyed(keyA, shotLine(boardShot, []byte("GIF89a"))),
		keyed(keyA, "shot="+boardShot+"\n"),
		keyed(keyA, "shot="+boardShot+shotSeparator+"%%%\n"),
		shotLine(boardShot, png("x")),
		keyed("ABC", shotLine(boardShot, png("x"))),
	} {
		e.err.Reset()
		testutil.Equal(t, e.dispatch(baselinePutCmd, stdin), ExitFail)
		e.expectOut(e.err, msgBadShots)
	}
	_, err := os.Stat(e.svc.Paths.UIBaseline(appName))
	testutil.True(t, os.IsNotExist(err), "nothing stored")
	many := strings.Repeat(shotLine(boardShot, png("x")), MaxBaselineShots+1)
	_, err = ReadMeta(strings.NewReader(many))
	testutil.Equal(t, err, errBadShots)
}

func TestUIBaselineNeedsTheHubWorkflowIdentity(t *testing.T) {
	for _, raw := range []string{baselineGetCmd, baselinePutCmd} {
		e := newEnv(t)
		e.bare = true
		testutil.Equal(t, e.dispatch(raw, keyed(keyA, shotLine(boardShot, png("x")))), ExitFail)
		e.expectOut(e.err, msgNoIdentity)
		e2 := newEnv(t)
		e2.state(registry.DeployState{Current: goodTag})
		e2.ident.err = oidc.ErrClaims
		testutil.Equal(t, e2.dispatch(raw, keyed(keyA, shotLine(boardShot, png("x")))), ExitFail)
		e2.expectOut(e2.err, msgUnverified)
	}
	_, err := ParseRequest(baselineGetCmd + " extra")
	testutil.Equal(t, err, errRefused)
}

func TestUIBaselineSkipsStrangeFilesAndReportsUnreadableOnes(t *testing.T) {
	e := newEnv(t)
	dir := e.svc.Paths.UIBaseline(appName)
	testutil.Mkdir(t, filepath.Join(dir, "folder.png"))
	testutil.Write(t, filepath.Join(dir, baselineTagFile), goodTag+"\n")
	testutil.Write(t, filepath.Join(dir, baselineKeyFile), keyA+"\n")
	testutil.Write(t, filepath.Join(dir, "notes.txt"), "x")
	testutil.Write(t, filepath.Join(dir, boardShot), string(png("wide")))
	testutil.NoErr(t, os.Symlink(filepath.Join(dir, boardShot), filepath.Join(dir, boardNarrow)))
	testutil.Equal(t, e.dispatch(baselineGetCmd, ""), ExitOK)
	_, shots := e.baseline()
	testutil.Equal(t, shots, map[string]string{boardShot: string(png("wide"))})
	testutil.NoErr(t, os.Chmod(filepath.Join(dir, boardShot), 0))
	testutil.Equal(t, e.dispatch(baselineGetCmd, ""), ExitFail)
	e.expectOut(e.err, msgBaselineRead)
}
