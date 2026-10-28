package cli

import (
	"context"
	"os"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/arch"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	embedded "github.com/DreamsAreReal/deploy-hub/policy"
)

const (
	archPrefix      = "arch-report: "
	usageArchReport = "usage: deploy-hub arch-report [metrics-file]"
	metricsPerm     = 0o644
)

func runArchReport(_ context.Context, a *App, _ procexec.Runner, args []string) int {
	if len(args) > 1 {
		a.Err.Println(usageArchReport)
		return ExitUsage
	}
	catalog, err := arch.Parse(embedded.Architecture())
	if err != nil {
		return a.failWith(archPrefix, err)
	}
	if len(args) == 1 {
		if err := os.WriteFile(args[0], []byte(catalog.Exposition()), metricsPerm); err != nil {
			return a.failWith(archPrefix, err)
		}
	}
	a.Out.Print(catalog.Report())
	return ExitOK
}
