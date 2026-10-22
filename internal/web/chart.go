package web

import (
	"fmt"
	"strconv"
	"time"
)

const (
	chartW      = 640.0
	chartH      = 240.0
	chartLeft   = 28.0
	chartBottom = 22.0
	chartTop    = 8.0
	barGap      = 6.0
	minBar      = 4.0
	stackGap    = 2.0
	barRadius   = 4.0
	labelEvery  = 7
	minScale    = 4
	tickPad     = 6.0
	labelLift   = 4.0
	half        = 2.0
	dayFormat   = "2 Jan"
)

type chartView struct {
	Empty   bool
	ViewBox string
	Left    string
	Right   string
	Base    string
	Ticks   []tickView
	Bars    []barView
	Rows    []rowView
}

type tickView struct {
	Y    string
	X    string
	Text string
}

type barView struct {
	HitX   string
	HitW   string
	HitH   string
	OK     string
	Fail   string
	Title  string
	Label  string
	LabelX string
	LabelY string
}

type rowView struct {
	Date string
	OK   int
	Fail int
}

func chart(days []Day) chartView {
	v := chartView{Empty: true, ViewBox: fmt.Sprintf("0 0 %s %s", num(chartW), num(chartH)), Left: num(chartLeft), Right: num(chartW)}
	peak := 0
	for _, d := range days {
		peak = max(peak, d.OK+d.Fail)
		if d.OK+d.Fail > 0 {
			v.Rows = append(v.Rows, rowView{Date: dayName(d.Date), OK: d.OK, Fail: d.Fail})
		}
	}
	if peak == 0 {
		return v
	}
	v.Empty = false
	scale := scaleFor(peak)
	y := func(n int) float64 { return chartTop + (chartH-chartTop-chartBottom)*(1-float64(n)/float64(scale)) }
	v.Base = num(y(0))
	for _, n := range []int{0, scale / 2, scale} {
		v.Ticks = append(v.Ticks, tickView{Y: num(y(n)), X: num(chartLeft - tickPad), Text: strconv.Itoa(n)})
	}
	slot := (chartW - chartLeft) / float64(len(days))
	width := max(minBar, slot-barGap)
	for i, d := range days {
		v.Bars = append(v.Bars, bar(d, i, len(days), slot, width, y))
	}
	return v
}

func scaleFor(peak int) int {
	if peak <= minScale {
		return minScale
	}
	step := 1
	for step*10 < peak {
		step *= 10
	}
	scale := (peak + step - 1) / step * step
	return scale + scale%2
}

func bar(d Day, i, count int, slot, width float64, y func(int) float64) barView {
	left := chartLeft + float64(i)*slot
	x := left + (slot-width)/half
	b := barView{HitX: num(left), HitW: num(slot), HitH: num(chartH - chartBottom + stackGap),
		Title: fmt.Sprintf("%s: ✓ %d, ✗ %d", dayName(d.Date), d.OK, d.Fail)}
	okTop := y(d.OK)
	if d.OK > 0 {
		b.OK = barPath(x, okTop, width, y(0)-okTop, d.Fail == 0)
	}
	if d.Fail > 0 {
		gap := 0.0
		if d.OK > 0 {
			gap = stackGap
		}
		top := y(d.OK + d.Fail)
		b.Fail = barPath(x, top, width, okTop-top-gap, true)
	}
	if (count-1-i)%labelEvery == 0 {
		b.Label, b.LabelX, b.LabelY = dayName(d.Date), num(x+width/half), num(chartH-labelLift)
	}
	return b
}

func barPath(x, y, w, h float64, round bool) string {
	r := 0.0
	if round {
		r = min(barRadius, w/half, h)
	}
	return fmt.Sprintf("M%s,%sV%sQ%s,%s %s,%sH%sQ%s,%s %s,%sV%sZ",
		num(x), num(y+h), num(y+r), num(x), num(y), num(x+r), num(y), num(x+w-r), num(x+w), num(y), num(x+w), num(y+r), num(y+h))
}

func dayName(date string) string {
	t, err := time.Parse(dayLayout, date)
	if err != nil {
		return date
	}
	return t.Format(dayFormat)
}

func num(f float64) string {
	return strconv.FormatFloat(f, 'f', 1, 64)
}
