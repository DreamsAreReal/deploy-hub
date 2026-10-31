package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

type fakeSecrets struct {
	keys []string
	err  error
	laid []string
}

func (f *fakeSecrets) Lay(_ context.Context, app, dir string) ([]string, error) {
	f.laid = append(f.laid, app+" "+dir)
	return f.keys, f.err
}

func TestDeployLaysTheAppSecretsFirst(t *testing.T) {
	e := newEnv(t)
	layer := &fakeSecrets{keys: []string{"API_TOKEN", "DSN"}}
	e.svc.Secrets = layer
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitOK)
	testutil.Equal(t, layer.laid, []string{appName + " " + e.appDir})
	testutil.True(t, strings.Contains(e.logs.String(), `"msg":"secrets laid","app":"web","keys":"API_TOKEN,DSN"`), e.logs.String())
}

func TestDeployWaitsWhenTheSecretsStoreIsLocked(t *testing.T) {
	e := newEnv(t)
	e.svc.Secrets = &fakeSecrets{err: errors.New("secrets store is locked")}
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitFail)
	e.expectOut(e.err, msgSecretsFail+"secrets store is locked")
	e.expectJournal("web@aaaaaaa deploy fail")
	testutil.Equal(t, e.exec.Count("docker pull"), 0)
	quiet := newEnv(t)
	quiet.svc.Secrets = &fakeSecrets{}
	testutil.Equal(t, quiet.dispatch("deploy web "+goodTag, meta()), ExitOK)
	testutil.True(t, !strings.Contains(quiet.logs.String(), "secrets laid"), "nothing laid, nothing logged")
}

func TestKeyBoundToOneAppCannotTouchAnother(t *testing.T) {
	e := newEnv(t)
	e.svc.Only = []string{appOther}
	testutil.Equal(t, e.dispatch("deploy web "+goodTag, meta()), ExitFail)
	e.expectOut(e.err, msgOtherApp)
	testutil.Equal(t, e.exec.Count("docker pull"), 0)
	bound := newEnv(t)
	bound.svc.Only = []string{appOther, appName}
	testutil.Equal(t, bound.dispatch("deploy web "+goodTag, meta()), ExitOK)
	testutil.Equal(t, refusalReason(msgOtherApp), reasonOtherApp)
}
