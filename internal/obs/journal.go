package obs

import (
	"cmp"
	"maps"
	"slices"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
)

const refusedResult = "denied"

type opKey struct {
	app, action, result string
}

func keyOf(e journal.Entry) opKey {
	k := opKey{app: e.App, action: e.Action, result: e.Result}
	if e.Action == journal.ActionRefuse {
		k.result = refusedResult
	}
	return k
}

func deployCounters(apps []string) map[opKey]int {
	counts := map[opKey]int{}
	for _, app := range apps {
		for _, result := range []string{journal.ResultOK, journal.ResultFail} {
			counts[opKey{app: app, action: journal.ActionDeploy, result: result}] = 0
		}
	}
	return counts
}

func compareKeys(a, b opKey) int {
	return cmp.Or(cmp.Compare(a.app, b.app), cmp.Compare(a.action, b.action), cmp.Compare(a.result, b.result))
}

func JournalSamples(entries []journal.Entry, apps []string) []Sample {
	counts := deployCounters(apps)
	last := map[opKey]float64{}
	incidents := map[string]float64{}
	for _, e := range entries {
		k := keyOf(e)
		counts[k]++
		last[k] = float64(e.Time.Unix())
		if journal.IsIncident(e) {
			incidents[e.App] = float64(e.Time.Unix())
		}
	}
	keys := slices.SortedFunc(maps.Keys(counts), compareKeys)
	out := make([]Sample, 0, len(keys)+len(last)+len(incidents))
	for _, k := range keys {
		l := Labels{LabelApp: k.app, LabelAction: k.action, LabelResult: k.result}
		out = append(out, Sample{Name: MetricOperationsTotal, Labels: l, Value: float64(counts[k])})
		if counts[k] > 0 {
			out = append(out, Sample{Name: MetricLastOperationTime, Labels: l, Value: last[k]})
		}
	}
	for _, app := range slices.Sorted(maps.Keys(incidents)) {
		out = append(out, Sample{Name: MetricLastIncidentTime, Labels: Labels{LabelApp: app}, Value: incidents[app]})
	}
	return out
}
