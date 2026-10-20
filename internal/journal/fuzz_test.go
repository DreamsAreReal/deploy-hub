package journal

import (
	"testing"
	"time"
)

func FuzzParse(f *testing.F) {
	f.Add("[2026-10-19T12:00:00Z] web@aaaaaaa deploy ok 12s smoke=ok")
	f.Add("[bad] x")
	f.Fuzz(func(t *testing.T, line string) {
		e, ok := Parse(line)
		if !ok {
			return
		}
		back, ok := Parse(e.Format())
		if !ok || back.App != e.App || back.Action != e.Action || back.Result != e.Result || back.Seconds != e.Seconds || !back.Time.Equal(e.Time.Truncate(time.Second)) {
			t.Fatalf("round trip changed %q into %q", line, e.Format())
		}
	})
}
