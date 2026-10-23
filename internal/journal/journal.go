package journal

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	TimeLayout = "2006-01-02T15:04:05Z"
	filePerm   = 0o600

	ActionDeploy   = "deploy"
	ActionRollback = "rollback"
	ActionRedeploy = "redeploy"
	ActionStop     = "stop"
	ActionRefuse   = "refuse"
	ActionPrune    = "prune"
	ActionGate     = "gate"
	ActionHostSync = "host-sync"
	ActionHostBack = "host-restore"

	ResultOK   = "ok"
	ResultFail = "fail"
	ResultSkip = "skip"

	NoValue = "-"
)

var linePattern = regexp.MustCompile(`^\[([^\]]+)\] (\S+)@(\S+) (\S+) (\S+) (\d+)s(?: (.*))?$`)

type Entry struct {
	Time    time.Time
	App     string
	Sha7    string
	Action  string
	Result  string
	Seconds int
	Extra   string
	Raw     string
}

func (e Entry) Format() string {
	line := fmt.Sprintf("[%s] %s@%s %s %s %ds", e.Time.UTC().Format(TimeLayout), e.App, e.Sha7, e.Action, e.Result, e.Seconds)
	if e.Extra != "" {
		line += " " + e.Extra
	}
	return line
}

func (e Entry) Stamp() string {
	return e.Time.UTC().Format(TimeLayout)
}

func Parse(line string) (Entry, bool) {
	m := linePattern.FindStringSubmatch(line)
	if m == nil {
		return Entry{}, false
	}
	ts, err := time.Parse(TimeLayout, m[1])
	if err != nil {
		return Entry{}, false
	}
	secs, err := strconv.Atoi(m[6])
	if err != nil {
		return Entry{}, false
	}
	return Entry{Time: ts, App: m[2], Sha7: m[3], Action: m[4], Result: m[5], Seconds: secs, Extra: m[7], Raw: line}, true
}

func IsContainerOp(action string) bool {
	switch action {
	case ActionDeploy, ActionRollback, ActionRedeploy, ActionStop:
		return true
	default:
		return false
	}
}

type Journal struct {
	Path string
	Now  func() time.Time
}

func (j Journal) Append(e Entry) error {
	e.Time = j.Now()
	f, err := os.OpenFile(j.Path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, filePerm)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(e.Format() + "\n")
	return errors.Join(werr, f.Close())
}

func (j Journal) Entries() ([]Entry, error) {
	f, err := os.Open(j.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if e, ok := Parse(sc.Text()); ok {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func ForApp(entries []Entry, app string) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.App == app {
			out = append(out, e)
		}
	}
	return out
}

func ForApps(entries []Entry, apps []string) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.App == NoValue || slices.Contains(apps, e.App) {
			out = append(out, e)
		}
	}
	return out
}

func Tail(entries []Entry, n int) []Entry {
	return entries[max(0, len(entries)-n):]
}

func LastContainerOp(entries []Entry) (Entry, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		if IsContainerOp(entries[i].Action) {
			return entries[i], true
		}
	}
	return Entry{}, false
}

func SuccessfulShas(entries []Entry) []string {
	seen := map[string]bool{}
	var out []string
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Result != ResultOK || !movesVersion(e.Action) || seen[e.Sha7] {
			continue
		}
		seen[e.Sha7] = true
		out = append(out, e.Sha7)
	}
	return out
}

func movesVersion(action string) bool {
	return action == ActionDeploy || action == ActionRollback || action == ActionRedeploy
}

func IsIncident(e Entry) bool {
	return (e.Action == ActionDeploy && e.Result == ResultFail) || (e.Action == ActionRollback && e.Result == ResultOK)
}

func Sanitize(v string, limit int) string {
	var b strings.Builder
	for _, r := range v {
		if r < ' ' || r == '\u007f' {
			continue
		}
		b.WriteRune(r)
	}
	out := []rune(b.String())
	if len(out) > limit {
		out = out[:limit]
	}
	return string(out)
}
