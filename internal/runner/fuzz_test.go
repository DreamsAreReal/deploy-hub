package runner

import (
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	reqInjection = "deploy web $(id)"
	reqRollback  = "rollback web"
)

func FuzzParseRequest(f *testing.F) {
	for _, seed := range []string{"deploy web " + goodTag, reqRollback, "status", "history web", "report web " + goodTag, reqInjection, ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if r, err := ParseRequest(raw); err == nil {
			checkRequest(t, raw, r)
		}
	})
}

func checkRequest(t *testing.T, raw string, r Request) {
	t.Helper()
	if r.Verb != VerbStatus && validate.AppName(r.App) != nil {
		t.Fatalf("accepted bad app %q from %q", r.App, raw)
	}
	if r.Tag != "" && validate.Tag(r.Tag) != nil {
		t.Fatalf("accepted bad tag %q from %q", r.Tag, raw)
	}
	if strings.ContainsAny(r.App+r.Tag, " ;|&$`") {
		t.Fatalf("shell metacharacters accepted: %+v", r)
	}
}

func FuzzReadMeta(f *testing.F) {
	f.Add(identity() + "token=x\nactor=dev\nsubject=feat\x07\nstart=1760875000\ncompose=AA==\nmain=api\nhport=9001\nhmode=http\n")
	f.Add("subject=" + strings.Repeat("é", 200) + "\n")
	f.Fuzz(func(t *testing.T, stdin string) {
		if m, err := ReadMeta(strings.NewReader(stdin)); err == nil {
			checkMeta(t, m)
		}
	})
}

func checkMeta(t *testing.T, m Meta) {
	t.Helper()
	if len([]rune(m.Subject)) > subjectLimit || strings.ContainsAny(m.Subject, "\n\r\x00") {
		t.Fatalf("subject not sanitized: %q", m.Subject)
	}
	if m.Start != "" && validate.StartEpoch(m.Start) != nil {
		t.Fatalf("bad start accepted: %q", m.Start)
	}
	if m.ValidateDeploy() == nil && (validate.Digest(m.Digest) != nil || m.OIDC == "") {
		t.Fatalf("deploy metadata accepted without identity or digest: %+v", m)
	}
}
