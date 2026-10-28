package cli

import (
	"context"
	"os"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/scaffold"
)

const scaffoldPrefix = "scaffold: "

func runScaffold(_ context.Context, a *App, _ procexec.Runner, args []string) int {
	opts, err := scaffold.ParseOptions(args, a.Config.RepoOwner)
	if err != nil {
		a.Err.Println(err.Error())
		return ExitUsage
	}
	files, err := scaffold.Render(os.DirFS(opts.Template), opts, a.Config.HubRepo)
	if err == nil {
		err = scaffold.Write(opts.Dir, files)
	}
	if err != nil {
		return a.failWith(scaffoldPrefix, err)
	}
	a.Out.Print(scaffold.NextSteps(opts))
	return ExitOK
}
