package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

var errBadVerdict = errors.New(msgBadVerdict)

func (s *Service) report(ctx context.Context, raw string, entry registry.Entry, tag string, stdin io.Reader) int {
	conf, err := s.loadConf(entry.Dir, entry.Name)
	if err != nil {
		return ExitFail
	}
	meta, err := ReadMeta(stdin)
	if err == nil {
		err = s.authorize(ctx, VerbReport, entry, conf, tag, meta.OIDC)
	}
	if err != nil {
		return s.refuse(raw, refusalMessage(err))
	}
	v, err := decodeVerdict(meta.Verdict, entry.Name, tag)
	if err != nil {
		s.Log.Warn("gate verdict rejected", "app", entry.Name, "tag", tag, "error", err.Error())
		return s.refuse(raw, msgBadVerdict)
	}
	s.recordGate(entry.Name, tag, v)
	return ExitOK
}

func decodeVerdict(encoded, app, tag string) (gate.Verdict, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return gate.Verdict{}, errors.Join(errBadVerdict, err)
	}
	v, err := gate.Decode(data)
	if err != nil {
		return gate.Verdict{}, err
	}
	if v.App != app || !strings.HasPrefix(v.Commit, strings.TrimPrefix(tag, registry.TagPrefix)) {
		return gate.Verdict{}, errBadVerdict
	}
	return v, nil
}

func (s *Service) recordGate(app, tag string, v gate.Verdict) {
	s.record(journal.Entry{App: app, Sha7: registry.Sha7(tag), Action: journal.ActionGate, Result: resultOf(v.Passed), Seconds: int(v.Seconds)})
	own := boolLabel(v.Own)
	s.samples = append(s.samples,
		obs.Sample{Name: obs.MetricGateVerdict, Labels: obs.Labels{obs.LabelApp: app, obs.LabelOwn: own}, Value: obs.Bool(v.Passed)},
		obs.Sample{Name: obs.MetricGateDuration, Labels: obs.Labels{obs.LabelApp: app}, Value: v.Seconds},
		obs.Sample{Name: obs.MetricGateFailedChecks, Labels: obs.Labels{obs.LabelApp: app}, Value: float64(len(v.Failed()))},
	)
	for _, r := range v.Results {
		s.recordCheck(app, tag, r)
	}
	s.Log.Info("gate verdict recorded", "app", app, "tag", tag, "passed", v.Passed, "own", v.Own, "failed_checks", v.Failed(), "seconds", v.Seconds)
	s.Out.Printf(msgGateRecorded, app, registry.Sha7(tag), verdictWord(v.Passed), strings.Join(v.Failed(), ","))
}

func (s *Service) recordCheck(app, tag string, r gate.Result) {
	if r.Status == gate.StatusNA {
		s.Log.Info("gate check not applicable", "app", app, "tag", tag, "check", r.Check, "reason", r.Reason)
		return
	}
	labels := obs.Labels{obs.LabelApp: app, obs.LabelCheck: r.Check}
	s.samples = append(s.samples,
		obs.Sample{Name: obs.MetricGateCheckVerdict, Labels: labels, Value: obs.Bool(r.Status == gate.StatusPass)},
		obs.Sample{Name: obs.MetricGateCheckDuration, Labels: labels, Value: r.Seconds},
	)
	for name, value := range r.Values {
		s.samples = append(s.samples, obs.Sample{Name: obs.MetricGateCheckValue, Labels: obs.Labels{obs.LabelApp: app, obs.LabelCheck: r.Check, obs.LabelName: name}, Value: value})
	}
	if r.Status == gate.StatusFail {
		s.Log.Error("gate check failed", "app", app, "tag", tag, "check", r.Check, "reason", r.Reason, "values", r.Values, "findings", r.Findings)
		return
	}
	s.Log.Info("gate check passed", "app", app, "tag", tag, "check", r.Check, "values", r.Values)
}

func boolLabel(v bool) string {
	if v {
		return labelYes
	}
	return labelNo
}

func verdictWord(passed bool) string {
	if passed {
		return wordPassed
	}
	return wordBlocked
}
