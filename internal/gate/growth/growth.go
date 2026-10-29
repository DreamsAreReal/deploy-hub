package growth

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	Window       = 30 * 24 * time.Hour
	ValueAdded   = "added"
	ValueRemoved = "removed"
	ValueNet     = "net"
	head         = "HEAD"
	numstatParts = 3
	reasonGrowth = "the code grew by more lines in 30 days than max_net_growth_lines allows: remove what the new code replaced"
)

func Check(ctx context.Context, ws gate.Workspace) gate.Outcome {
	base, err := windowStart(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	numstat, err := git(ctx, ws, "diff", "--numstat", "--no-renames", "-z", base, head)
	if err != nil {
		return gate.Fail(err.Error())
	}
	added, removed := count(numstat)
	values := map[string]float64{ValueAdded: float64(added), ValueRemoved: float64(removed), ValueNet: float64(added - removed)}
	limit := ws.Policy.Thresholds.MaxNetGrowth
	return gate.Judge(limit == 0 || added-removed <= limit, reasonGrowth, values, nil)
}

func windowStart(ctx context.Context, ws gate.Workspace) (string, error) {
	stamp, err := git(ctx, ws, "show", "-s", "--format=%ct", head)
	if err != nil {
		return "", err
	}
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return "", fmt.Errorf("commit time %q: %w", stamp, err)
	}
	before := time.Unix(seconds, 0).Add(-Window).UTC().Format(time.RFC3339)
	base, err := git(ctx, ws, "rev-list", "--first-parent", "-1", "--before="+before, head)
	if err != nil || base != "" {
		return base, err
	}
	return git(ctx, ws, "rev-list", "--first-parent", "--max-parents=0", head)
}

func git(ctx context.Context, ws gate.Workspace, args ...string) (string, error) {
	res, err := ws.Run(ctx, source.GitBinary, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, procexec.Output(err))
	}
	return strings.TrimSpace(res.Stdout), nil
}

func count(numstat string) (added, removed int) {
	for _, record := range strings.Split(numstat, "\x00") {
		fields := strings.SplitN(record, "\t", numstatParts)
		if len(fields) < numstatParts || !source.Code(fields[2]) {
			continue
		}
		plus, plusErr := strconv.Atoi(fields[0])
		minus, minusErr := strconv.Atoi(fields[1])
		if plusErr == nil && minusErr == nil {
			added, removed = added+plus, removed+minus
		}
	}
	return added, removed
}
