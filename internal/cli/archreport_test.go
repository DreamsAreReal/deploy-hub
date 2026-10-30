package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestArchReportPrintsTheCatalogAndWritesMetrics(t *testing.T) {
	h := newHarness(t)
	metrics := filepath.Join(t.TempDir(), "arch.prom")
	testutil.Equal(t, h.run(CmdArchReport, metrics), ExitOK)
	testutil.Contains(t, h.out.String(), "# Architecture report", "| tasks |")
	testutil.Contains(t, testutil.Read(t, metrics), "deploy_hub_arch_services ", `deploy_hub_arch_reach{service="tasks"}`)
	quiet := newHarness(t)
	testutil.Equal(t, quiet.run(CmdArchReport), ExitOK)
	testutil.Equal(t, quiet.run(CmdArchReport, "a", "b"), ExitUsage)
	testutil.True(t, strings.Contains(quiet.err.String(), usageArchReport), quiet.err.String())
	testutil.Equal(t, quiet.run(CmdArchReport, filepath.Join(t.TempDir(), "missing", "arch.prom")), ExitFail)
}
