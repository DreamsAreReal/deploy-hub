package web

import (
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	justNow  = "just now"
	fullRing = "314.16"
	tickX    = "22.0"
)

func TestAgo(t *testing.T) {
	cases := map[time.Duration]string{
		0: justNow, 59 * time.Second: justNow, time.Minute: "1 min ago", 59 * time.Minute: "59 min ago",
		time.Hour: "1 h ago", 23 * time.Hour: "23 h ago", day: "1 day ago", 2 * day: "2 days ago", 5 * day: "5 days ago",
		11 * day: "11 days ago", 21 * day: "21 days ago", 112 * day: "112 days ago",
	}
	for d, want := range cases {
		testutil.Equal(t, ago(clock.Add(-d), clock), want)
	}
}

func TestDuration(t *testing.T) {
	cases := map[int]string{0: "0 s", 59: "59 s", 60: "1 min", 61: "1 min 1 s", 120: "2 min", 3599: "59 min 59 s"}
	for sec, want := range cases {
		testutil.Equal(t, duration(sec), want)
	}
}

func TestMood(t *testing.T) {
	up := App{Up: true}
	up.Sha7 = sha
	cases := []struct {
		app  App
		busy bool
		want mood
	}{
		{up, true, moodBusy},
		{App{}, false, moodNew},
		{withHealth(App{}, "-"), false, moodDown},
		{withHealth(App{Up: true, Healthy: true}, "ok"), false, moodHealthy},
		{withHealth(App{Up: true}, "-"), false, moodRunning},
		{withHealth(App{Up: true}, "fail"), false, moodSick},
	}
	cases[1].app.Sha7 = noVersion
	for _, tc := range cases {
		testutil.Equal(t, moodOf(tc.app, tc.busy), tc.want)
	}
}

func withHealth(a App, h string) App {
	a.Sha7, a.Health = sha, h
	return a
}

func TestRing(t *testing.T) {
	good, bad := appView{Fine: true}, appView{mood: moodHealthy}
	testutil.Equal(t, ring(nil), ringView{Offset: fullRing, Tone: ToneIdle})
	testutil.Equal(t, ring([]appView{good, good}), ringView{Fine: 2, Total: 2, Offset: "0.00", Tone: ToneGood})
	testutil.Equal(t, ring([]appView{good, bad}), ringView{Fine: 1, Total: 2, Offset: "157.08", Tone: ToneWarning})
	testutil.Equal(t, ring([]appView{good, bad, bad}), ringView{Fine: 1, Total: 3, Offset: "209.44", Tone: ToneCritical})
	testutil.Equal(t, ring([]appView{bad}), ringView{Total: 1, Offset: fullRing, Tone: ToneCritical})
}

func TestScale(t *testing.T) {
	cases := map[int]int{1: 4, 4: 4, 5: 6, 7: 8, 10: 10, 11: 20, 23: 30, 99: 100, 101: 200, 150: 200}
	for peak, want := range cases {
		testutil.Equal(t, scaleFor(peak), want)
	}
}

func TestChart(t *testing.T) {
	days := make([]Day, historyDays)
	for i := range days {
		days[i].Date = clock.AddDate(0, 0, i-historyDays+1).Format(dayLayout)
	}
	days[0].OK, days[1].Fail, days[29].OK, days[29].Fail = 3, 1, 2, 2
	c := chart(days)
	testutil.True(t, !c.Empty, "chart has data")
	testutil.Equal(t, c.ViewBox, "0 0 640.0 240.0")
	testutil.Equal(t, c.Base, "218.0")
	testutil.Equal(t, c.Ticks, []tickView{{Y: "218.0", X: tickX, Text: "0"}, {Y: "113.0", X: tickX, Text: "2"}, {Y: "8.0", X: tickX, Text: "4"}})
	testutil.Equal(t, len(c.Bars), historyDays)
	testutil.Equal(t, c.Rows, []rowView{{Date: "23 Sep", OK: 3}, {Date: "24 Sep", Fail: 1}, {Date: "22 Oct", OK: 2, Fail: 2}})

	first := c.Bars[0]
	testutil.Equal(t, first.HitX, "28.0")
	testutil.Equal(t, first.HitW, "20.4")
	testutil.Equal(t, first.HitH, "220.0")
	testutil.Equal(t, first.OK, "M31.0,218.0V64.5Q31.0,60.5 35.0,60.5H41.4Q45.4,60.5 45.4,64.5V218.0Z")
	testutil.Equal(t, first.Fail, "")
	testutil.Equal(t, first.Title, "23 Sep: ✓ 3, ✗ 0")
	testutil.Equal(t, first.Label, "")
	testutil.Equal(t, c.Bars[1].LabelX, "58.6")
	testutil.Equal(t, c.Bars[1].LabelY, "236.0")

	testutil.Equal(t, c.Bars[1].OK, "")
	testutil.Equal(t, c.Bars[1].Fail, "M51.4,218.0V169.5Q51.4,165.5 55.4,165.5H61.8Q65.8,165.5 65.8,169.5V218.0Z")
	testutil.Equal(t, c.Bars[1].Label, "24 Sep")
	testutil.Equal(t, c.Bars[2].Label, "")
	testutil.Equal(t, c.Bars[22].Label, "15 Oct")
	testutil.Equal(t, c.Bars[28].Label, "")

	last := c.Bars[29]
	testutil.Equal(t, last.OK, "M622.6,218.0V113.0Q622.6,113.0 622.6,113.0H637.0Q637.0,113.0 637.0,113.0V218.0Z")
	testutil.Equal(t, last.Fail, "M622.6,111.0V12.0Q622.6,8.0 626.6,8.0H633.0Q637.0,8.0 637.0,12.0V111.0Z")
	testutil.Equal(t, last.Label, "22 Oct")
	testutil.Equal(t, last.LabelX, "629.8")

	testutil.True(t, chart(make([]Day, historyDays)).Empty, "no operations, no chart")
}

func TestBarPathClampsTheRadius(t *testing.T) {
	testutil.Equal(t, barPath(0, 10, 4, 1, true), "M0.0,11.0V11.0Q0.0,10.0 1.0,10.0H3.0Q4.0,10.0 4.0,11.0V11.0Z")
	testutil.Equal(t, barPath(0, 10, 4, 10, true), "M0.0,20.0V12.0Q0.0,10.0 2.0,10.0H2.0Q4.0,10.0 4.0,12.0V20.0Z")
}

func TestLabels(t *testing.T) {
	testutil.Equal(t, dayName("not-a-date"), "not-a-date")
	testutil.Equal(t, dayName("2026-01-01"), "1 Jan")
	testutil.Equal(t, labelOf(actions, "teleport"), label{"•", "teleport"})
	testutil.Equal(t, resultOf("denied(x) "), outcome{ToneIdle, "denied(x)"})
	testutil.Equal(t, or(states, "zombie"), "zombie")
	testutil.Equal(t, rate(Totals{Ops: 3, OK: 2}), "66%")
}
