package runner

import (
	"bufio"
	"errors"
	"io"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	subjectLimit  = 120
	metaMain      = "main"
	maxMetaLine   = 8 << 20
	initialBuffer = 64 << 10
	metaShot      = "shot"
)

var (
	errBadMain  = errors.New(msgBadMain)
	errBadHPort = errors.New(msgBadHPort)
	errBadHMode = errors.New(msgBadHMode)
)

type Meta struct {
	Token    string
	Actor    string
	Subject  string
	Start    string
	Compose  string
	Main     string
	HPort    string
	HMode    string
	OIDC     string
	Digest   string
	Verdict  string
	Verdicts string
	Mode     string
	Run      string
	Shots    []string
}

func ReadMeta(r io.Reader) (Meta, error) {
	var m Meta
	fields := map[string]*string{
		"token": &m.Token, "actor": &m.Actor, "subject": &m.Subject, "start": &m.Start,
		"compose": &m.Compose, metaMain: &m.Main, "hport": &m.HPort, "hmode": &m.HMode,
		"oidc": &m.OIDC, "digest": &m.Digest, "verdict": &m.Verdict, "verdicts": &m.Verdicts, "mode": &m.Mode, "run": &m.Run,
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, initialBuffer), maxMetaLine)
	shotBytes := 0
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if dst, known := fields[key]; ok && known {
			*dst = value
		}
		if key == metaShot && ok && m.addShot(value, &shotBytes) != nil {
			return m, errBadShots
		}
	}
	m.Subject = journal.Sanitize(m.Subject, subjectLimit)
	if validate.StartEpoch(m.Start) != nil {
		m.Start = ""
	}
	return m, sc.Err()
}

func (m *Meta) addShot(value string, total *int) error {
	*total += len(value)
	m.Shots = append(m.Shots, value)
	if len(m.Shots) > MaxBaselineShots || *total > MaxBaselineBytes {
		return errBadShots
	}
	return nil
}

func (m Meta) BYOC() bool {
	return m.Compose != ""
}

func (m Meta) ValidateDeploy() error {
	if m.OIDC == "" {
		return errNoIdentity
	}
	if validate.Digest(m.Digest) != nil {
		return errBadDigest
	}
	return m.Validate()
}

func (m Meta) Validate() error {
	if !m.BYOC() {
		return nil
	}
	if validate.Service(m.Main) != nil {
		return errBadMain
	}
	if validate.OptionalPort(m.HPort) != nil {
		return errBadHPort
	}
	switch m.HMode {
	case registry.HealthCompose, registry.HealthHTTP, registry.HealthProcess:
		return nil
	default:
		return errBadHMode
	}
}
