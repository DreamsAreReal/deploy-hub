package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/docker"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	ToneGood     = "good"
	ToneWarning  = "warning"
	ToneCritical = "critical"
	ToneIdle     = "idle"
	ToneBusy     = "busy"

	ringLength = 314.16
	noVersion  = "none"
	clockForm  = "02.01.2006 15:04"
	minute     = 60
	probeOK    = "ok"
	probeFail  = "fail"
	never      = "not yet"
	halfFine   = 0.5
)

type mood struct {
	Face string
	Text string
	Tone string
}

var (
	moodBusy    = mood{"🛠️", "Working…", ToneBusy}
	moodNew     = mood{"🥚", "Not deployed yet", ToneIdle}
	moodDown    = mood{"😴", "Down", ToneCritical}
	moodHealthy = mood{"😊", "All good", ToneGood}
	moodRunning = mood{"🙂", "Running", ToneGood}
	moodSick    = mood{"🤒", "Unwell", ToneWarning}
)

type label struct {
	Icon string
	Text string
}

var actions = map[string]label{
	journal.ActionDeploy:   {"🚀", "Deploy"},
	journal.ActionRollback: {"↩️", "Rollback"},
	journal.ActionRedeploy: {"🔁", "Redeploy"},
	journal.ActionStop:     {"⏹️", "Stop"},
	journal.ActionGate:     {"🛡️", "Quality gate"},
	journal.ActionRefuse:   {"🚫", "Refused"},
	journal.ActionPrune:    {"🧹", "Image cleanup"},
}

type outcome struct {
	Tone string
	Text string
}

var results = map[string]outcome{
	journal.ResultOK:   {ToneGood, "✓ ok"},
	journal.ResultFail: {ToneCritical, "✗ failed"},
}

var states = map[string]string{
	docker.StateMissing: "no container", "exited": "stopped",
}

var healths = map[string]string{
	probeOK: "answering", probeFail: "not answering", docker.NoHealth: "no check",
}

type dashboardView struct {
	Clock  string
	Ring   ringView
	Ops    int
	Rate   string
	Avg    string
	Last   string
	Apps   []appView
	Chart  chartView
	Events []eventView
}

type ringView struct {
	Fine   int
	Total  int
	Offset string
	Tone   string
}

type appView struct {
	mood
	Fine      bool
	Name      string
	URL       string
	Version   string
	Updated   string
	Container string
	Beads     []beadView
	Job       string
	Previous  string
	Current   string
}

type beadView struct {
	Tone  string
	Gate  bool
	Title string
}

type eventView struct {
	Icon   string
	Action string
	App    string
	Tone   string
	Result string
	Detail string
	When   string
	Stamp  string
}

func dashboard(o Overview, running map[string]string) dashboardView {
	v := dashboardView{Clock: o.Now.Format("15:04:05"), Ops: o.Totals.Ops, Rate: rate(o.Totals), Avg: "–", Last: never}
	if o.Totals.Ops > 0 {
		v.Avg = duration(o.Totals.AvgSeconds)
	}
	if o.Totals.LastDeploy != nil {
		v.Last = ago(*o.Totals.LastDeploy, o.Now)
	}
	for _, a := range o.Apps {
		v.Apps = append(v.Apps, appCard(a, running[a.Name], o.Now))
	}
	v.Ring = ring(v.Apps)
	v.Chart = chart(o.Days)
	for _, e := range o.Events {
		v.Events = append(v.Events, event(e, o.Now))
	}
	return v
}

func rate(t Totals) string {
	if t.Ops == 0 {
		return "–"
	}
	return strconv.Itoa(t.OK*100/t.Ops) + "%"
}

func moodOf(a App, busy bool) mood {
	switch {
	case busy:
		return moodBusy
	case a.Sha7 == noVersion:
		return moodNew
	case !a.Up:
		return moodDown
	case a.Healthy:
		return moodHealthy
	case a.Health == docker.NoHealth:
		return moodRunning
	}
	return moodSick
}

func appCard(a App, job string, now time.Time) appView {
	v := appView{
		mood: moodOf(a, job != ""), Fine: moodOf(a, false).Tone == ToneGood, Name: a.Name, URL: a.URL, Job: job, Current: a.Current,
		Version: a.Sha7, Updated: never, Container: or(states, a.State) + ", " + or(healths, a.Health),
	}
	if a.Previous != "" {
		v.Previous = registry.Sha7(a.Previous)
	}
	if t, err := time.Parse(journal.TimeLayout, a.Last); err == nil {
		v.Updated = ago(t, now)
	}
	for _, e := range a.Recent {
		act, res := labelOf(actions, e.Action), resultOf(e.Result)
		v.Beads = append(v.Beads, beadView{Tone: res.Tone, Gate: e.Action == journal.ActionGate,
			Title: fmt.Sprintf("%s %s — %s, %s · %s", act.Text, e.Sha7, res.Text, duration(e.Seconds), e.Time.In(now.Location()).Format(clockForm))})
	}
	return v
}

func ring(apps []appView) ringView {
	r := ringView{Total: len(apps), Offset: offset(1), Tone: ToneIdle}
	for _, a := range apps {
		if a.Fine {
			r.Fine++
		}
	}
	if r.Total == 0 {
		return r
	}
	share := float64(r.Fine) / float64(r.Total)
	r.Offset = offset(1 - share)
	switch {
	case r.Fine == r.Total:
		r.Tone = ToneGood
	case share >= halfFine:
		r.Tone = ToneWarning
	default:
		r.Tone = ToneCritical
	}
	return r
}

func offset(share float64) string {
	return strconv.FormatFloat(ringLength*share, 'f', 2, 64)
}

func event(e Event, now time.Time) eventView {
	act, res := labelOf(actions, e.Action), resultOf(e.Result)
	v := eventView{Icon: act.Icon, Action: act.Text, Tone: res.Tone, Result: res.Text, Detail: duration(e.Seconds),
		When: ago(e.Time, now), Stamp: e.Time.In(now.Location()).Format(clockForm)}
	if e.App != journal.NoValue {
		v.App = e.App
	}
	if e.Sha7 != journal.NoValue {
		v.Detail += " · " + e.Sha7
	}
	return v
}

func labelOf(m map[string]label, key string) label {
	if l, ok := m[key]; ok {
		return l
	}
	return label{"•", key}
}

func resultOf(result string) outcome {
	if o, ok := results[result]; ok {
		return o
	}
	return outcome{ToneIdle, strings.TrimSpace(result)}
}

func or(m map[string]string, key string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return key
}

func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < day:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	n := int(d / day)
	if n == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", n)
}

func duration(sec int) string {
	switch {
	case sec < minute:
		return fmt.Sprintf("%d s", sec)
	case sec%minute == 0:
		return fmt.Sprintf("%d min", sec/minute)
	}
	return fmt.Sprintf("%d min %d s", sec/minute, sec%minute)
}
