package web

import (
	"fmt"
	"slices"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/runner"
)

const (
	historyDays = 30
	eventsLimit = 60
	recentLimit = 14
	day         = 24 * time.Hour
	dayLayout   = "2006-01-02"
	appURL      = "https://%s.%s.sslip.io"
)

type App struct {
	runner.AppStatus
	URL     string  `json:"url"`
	Up      bool    `json:"up"`
	Healthy bool    `json:"healthy"`
	Recent  []Event `json:"recent"`
}

type Event struct {
	Time    time.Time `json:"time"`
	App     string    `json:"app"`
	Sha7    string    `json:"sha7"`
	Action  string    `json:"action"`
	Result  string    `json:"result"`
	Seconds int       `json:"seconds"`
}

type Day struct {
	Date string `json:"date"`
	OK   int    `json:"ok"`
	Fail int    `json:"fail"`
}

type Totals struct {
	Ops        int        `json:"ops"`
	OK         int        `json:"ok"`
	Fail       int        `json:"fail"`
	AvgSeconds int        `json:"avg_seconds"`
	LastDeploy *time.Time `json:"last_deploy"`
}

type Overview struct {
	Now    time.Time `json:"now"`
	Apps   []App     `json:"apps"`
	Days   []Day     `json:"days"`
	Totals Totals    `json:"totals"`
	Events []Event   `json:"events"`
	Jobs   []Job     `json:"jobs"`
}

func buildOverview(now time.Time, slug string, statuses []runner.AppStatus, entries []journal.Entry) Overview {
	o := Overview{Now: now, Apps: make([]App, 0, len(statuses)), Events: events(journal.Tail(entries, eventsLimit))}
	slices.Reverse(o.Events)
	for _, st := range statuses {
		o.Apps = append(o.Apps, App{AppStatus: st, URL: urlOf(st.Name, slug), Up: st.Up(), Healthy: st.Healthy(), Recent: recent(journal.ForApp(entries, st.Name))})
	}
	o.Days, o.Totals = window(now, entries)
	return o
}

func urlOf(app, slug string) string {
	if slug == "" {
		return ""
	}
	return fmt.Sprintf(appURL, app, slug)
}

func events(entries []journal.Entry) []Event {
	out := make([]Event, 0, len(entries))
	for _, e := range entries {
		out = append(out, Event{Time: e.Time, App: e.App, Sha7: e.Sha7, Action: e.Action, Result: e.Result, Seconds: e.Seconds})
	}
	return out
}

func recent(entries []journal.Entry) []Event {
	var ops []journal.Entry
	for _, e := range entries {
		if journal.IsContainerOp(e.Action) || e.Action == journal.ActionGate {
			ops = append(ops, e)
		}
	}
	return events(journal.Tail(ops, recentLimit))
}

func window(now time.Time, entries []journal.Entry) ([]Day, Totals) {
	first := now.UTC().Truncate(day).Add(-(historyDays - 1) * day)
	days := make([]Day, historyDays)
	for i := range days {
		days[i].Date = first.Add(time.Duration(i) * day).Format(dayLayout)
	}
	var t Totals
	seconds := 0
	for _, e := range entries {
		i := int(e.Time.Sub(first) / day)
		if !journal.IsContainerOp(e.Action) || e.Time.Before(first) || i >= len(days) {
			continue
		}
		seconds += e.Seconds
		days[i].add(e.Result == journal.ResultOK)
	}
	for _, d := range days {
		t.OK += d.OK
		t.Fail += d.Fail
	}
	t.Ops = t.OK + t.Fail
	if t.Ops > 0 {
		t.AvgSeconds = seconds / t.Ops
	}
	t.LastDeploy = lastDeploy(entries)
	return days, t
}

func (d *Day) add(ok bool) {
	if ok {
		d.OK++
		return
	}
	d.Fail++
}

func lastDeploy(entries []journal.Entry) *time.Time {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Action == journal.ActionDeploy && entries[i].Result == journal.ResultOK {
			return &entries[i].Time
		}
	}
	return nil
}
