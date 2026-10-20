package prune

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	KeepPrevious = 2
	latestTag    = "latest"
	noStateExtra = "no-deploy-state"
	resultExtra  = "removed=%d kept=%d"
)

type Images interface {
	ImageTags(ctx context.Context, image string) ([]string, error)
	RemoveImage(ctx context.Context, ref string) error
	PruneDangling(ctx context.Context)
}

type Pruner struct {
	Apps    registry.Apps
	Journal journal.Journal
	Images  Images
	Now     func() time.Time
	Log     *slog.Logger
	Metrics obs.Pusher
}

func (p Pruner) Run(ctx context.Context) error {
	apps, err := p.Apps.All()
	if err != nil {
		return err
	}
	entries, err := p.Journal.Entries()
	if err != nil {
		return err
	}
	var samples []obs.Sample
	for _, a := range apps {
		samples = append(samples, p.app(ctx, a, journal.ForApp(entries, a.Name))...)
	}
	p.Images.PruneDangling(ctx)
	p.Metrics.Push(ctx, samples)
	return nil
}

func (p Pruner) app(ctx context.Context, a registry.Entry, entries []journal.Entry) []obs.Sample {
	conf, err := registry.LoadAppConf(a.Dir)
	if err != nil || conf.Image == "" {
		return nil
	}
	started := p.Now()
	state := registry.LoadState(a.Dir)
	if state.Current == "" {
		p.journal(journal.Entry{App: a.Name, Sha7: journal.NoValue, Action: journal.ActionPrune, Result: journal.ResultSkip, Extra: noStateExtra})
		return nil
	}
	tags, err := p.Images.ImageTags(ctx, conf.Image)
	if err != nil {
		p.Log.Warn("image list failed", "app", a.Name, "error", err.Error())
		return nil
	}
	keep := KeepSet(state, journal.SuccessfulShas(entries), tags)
	removed := p.remove(ctx, conf.Image, tags, keep)
	secs := int(p.Now().Sub(started).Seconds())
	p.journal(journal.Entry{App: a.Name, Sha7: registry.Sha7(state.Current), Action: journal.ActionPrune, Result: journal.ResultOK, Seconds: secs, Extra: fmt.Sprintf(resultExtra, removed, len(keep))})
	labels := obs.Labels{obs.LabelApp: a.Name}
	return []obs.Sample{
		{Name: obs.MetricPruneRemoved, Labels: labels, Value: float64(removed)},
		{Name: obs.MetricPruneKept, Labels: labels, Value: float64(len(keep))},
	}
}

func (p Pruner) remove(ctx context.Context, image string, tags, keep []string) int {
	removed := 0
	for _, tag := range tags {
		if tag == latestTag || slices.Contains(keep, tag) {
			continue
		}
		if err := p.Images.RemoveImage(ctx, image+":"+tag); err != nil {
			p.Log.Warn("image not removed", "image", image, "tag", tag, "error", err.Error())
			continue
		}
		removed++
	}
	return removed
}

func (p Pruner) journal(e journal.Entry) {
	if err := p.Journal.Append(e); err != nil {
		p.Log.Error("journal append failed", "error", err.Error())
	}
	p.Log.Info("prune result", "app", e.App, "result", e.Result, "extra", e.Extra)
}

func KeepSet(state registry.DeployState, shas, tags []string) []string {
	keep := []string{state.Current}
	if state.Previous != "" && state.Previous != state.Current {
		keep = append(keep, state.Previous)
	}
	for _, sha := range shas {
		if len(keep) >= 1+KeepPrevious {
			break
		}
		if mentions(keep, sha) {
			continue
		}
		if full := tagFor(tags, sha); full != "" {
			keep = append(keep, full)
		}
	}
	return keep
}

func tagFor(tags []string, sha7 string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, registry.TagPrefix+sha7) {
			return t
		}
	}
	return ""
}

func mentions(keep []string, sha7 string) bool {
	return slices.ContainsFunc(keep, func(k string) bool { return strings.Contains(k, sha7) })
}
