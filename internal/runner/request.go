package runner

import (
	"errors"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/oidc"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	VerbDeploy   = "deploy"
	VerbRollback = "rollback"
	VerbStatus   = "status"
	VerbHistory  = "history"
	VerbReport   = "report"

	fingerprintLimit = 40
)

var (
	errRefused    = errors.New(msgRefused)
	errUnknownApp = errors.New(msgUnknownApp)
	errBadTag     = errors.New(msgBadTag)
)

type Request struct {
	Verb string
	App  string
	Tag  string
}

const (
	statusWords  = 1
	historyWords = 2
	opWords      = 3
)

var maxWords = map[string]int{VerbStatus: statusWords, VerbHistory: historyWords, VerbDeploy: opWords, VerbRollback: opWords, VerbReport: opWords}

func ParseRequest(raw string) (Request, error) {
	f := strings.Fields(raw)
	words := len(f)
	f = append(f, "", "", "")
	r := Request{Verb: f[0], App: f[1], Tag: f[2]}
	if limit, known := maxWords[r.Verb]; known && words > limit {
		return r, errRefused
	}
	switch r.Verb {
	case VerbStatus:
		return Request{Verb: r.Verb}, nil
	case VerbHistory:
		return Request{Verb: r.Verb, App: r.App}, appErr(r.App)
	case VerbDeploy, VerbReport:
		return r, errors.Join(appErr(r.App), tagErr(r.Tag))
	case VerbRollback:
		return r, errors.Join(appErr(r.App), optionalTagErr(r.Tag))
	default:
		return r, errRefused
	}
}

func appErr(app string) error {
	if validate.AppName(app) != nil {
		return errUnknownApp
	}
	return nil
}

func tagErr(tag string) error {
	if validate.Tag(tag) != nil {
		return errBadTag
	}
	return nil
}

func optionalTagErr(tag string) error {
	if tag == "" {
		return nil
	}
	return tagErr(tag)
}

var refusals = []error{errUnknownApp, errBadTag, errNoIdentity, errNoRepo, errNotGated, errBadDigest, errBadVerdict}

func refusalMessage(err error) string {
	for _, known := range refusals {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	if errors.Is(err, oidc.ErrToken) || errors.Is(err, oidc.ErrClaims) || errors.Is(err, oidc.ErrKeys) {
		return msgUnverified
	}
	return msgRefused
}

func refusalReason(msg string) string {
	if reason, ok := refusalReasons[msg]; ok {
		return reason
	}
	return reasonOther
}

func verbOf(raw string) string {
	f := strings.Fields(raw)
	if len(f) == 0 {
		return verbInvalid
	}
	if _, known := maxWords[f[0]]; known {
		return f[0]
	}
	return verbInvalid
}

func Fingerprint(raw string) string {
	var b strings.Builder
	for _, r := range strings.ReplaceAll(raw, " ", "_") {
		if isFingerprintRune(r) {
			b.WriteRune(r)
		}
	}
	out := b.String()
	out = out[:min(len(out), fingerprintLimit)]
	if out == "" {
		return deniedEmpty
	}
	return out
}

func isFingerprintRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
}

var (
	errBadCompose     = errors.New(msgBadCompose)
	errComposeInvalid = errors.New(msgComposeInvalid)
)
