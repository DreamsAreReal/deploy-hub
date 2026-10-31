package remotesync

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	DeliverVerb    = "remote-deliver"
	EngineBundled  = "bundled"
	EngineOnTarget = "installed"
	viaLine        = "via "
	resultLine     = "result "
	sudoBinary     = "sudo"
	sudoUserFlag   = "-u"
	sudoNonInter   = "-n"
)

var (
	ErrShipment = errors.New("invalid shipment")
	targetName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	sha7Pattern = regexp.MustCompile(`^[0-9a-f]{7}$`)
)

type Shipment struct {
	Target  string
	Sha7    string
	Engine  string
	Archive []byte
}

type Outcome struct {
	Host   string
	Result string
}

type Deliverer interface {
	Deliver(ctx context.Context, sh Shipment) (Outcome, error)
}

type Courier struct {
	Exec        procexec.Runner
	Dir         string
	Backups     string
	HealthWait  time.Duration
	VerdictWait time.Duration
	Poll        time.Duration
	Now         func() time.Time
	Sleep       func(time.Duration)
	Log         *slog.Logger
}

type parcel struct {
	target  string
	sha7    string
	conf    targetConf
	archive []byte
	size    int
	dir     string
	engine  string
}

func (sh Shipment) Validate() error {
	if !targetName.MatchString(sh.Target) || !sha7Pattern.MatchString(sh.Sha7) || (sh.Engine != EngineBundled && sh.Engine != EngineOnTarget) {
		return fmt.Errorf("%w: target %q, commit %q, engine %q", ErrShipment, sh.Target, sh.Sha7, sh.Engine)
	}
	return nil
}

func (s Courier) Deliver(ctx context.Context, sh Shipment) (Outcome, error) {
	if err := sh.Validate(); err != nil {
		return Outcome{Host: noHost}, err
	}
	conf, err := loadTarget(s.Dir, sh.Target)
	if err != nil {
		return Outcome{Host: noHost}, err
	}
	p := parcel{target: sh.Target, sha7: sh.Sha7, conf: conf, archive: sh.Archive, size: len(sh.Archive), dir: ReleaseRoot + sh.Sha7, engine: InstalledEngine}
	if sh.Engine == EngineBundled {
		p.engine = "sh " + p.dir + "/" + rootDir + InstalledEngine
	}
	return s.deliver(ctx, p)
}

type SudoCourier struct {
	Exec   procexec.Runner
	Binary string
	User   string
}

func (c SudoCourier) Deliver(ctx context.Context, sh Shipment) (Outcome, error) {
	args := []string{sudoNonInter, sudoUserFlag, c.User, c.Binary, DeliverVerb, sh.Target, sh.Sha7, sh.Engine}
	res, err := c.Exec.Run(ctx, procexec.Command{Name: sudoBinary, Args: args, Stdin: string(sh.Archive)})
	text := res.Stdout
	if err != nil {
		text = procexec.Output(err)
	}
	out := ParseOutcome(text)
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", DeliverVerb, err, lastLine(text))
	}
	return out, nil
}

func (o Outcome) Format() string {
	return viaLine + o.Host + "\n" + resultLine + oneLine(o.Result) + "\n"
}

func ParseOutcome(text string) Outcome {
	out := Outcome{Host: noHost}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, viaLine); ok {
			out.Host = v
		} else if v, ok := strings.CutPrefix(line, resultLine); ok {
			out.Result = v
		}
	}
	return out
}
