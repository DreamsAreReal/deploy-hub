package shell

import (
	"context"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
)

var Mutation = tokmut.Check(mutationSuite)

func mutationSuite(ctx context.Context, ws gate.Workspace) (tokmut.Suite, error) {
	cov, err := linecov.Load(ws)
	if err != nil {
		return tokmut.Suite{}, err
	}
	cases, err := junit.Load(ws.Report(JUnitReport))
	if err != nil {
		return tokmut.Suite{}, err
	}
	index, err := loadTestIndex(ws)
	if err != nil {
		return tokmut.Suite{}, err
	}
	tests, err := batsFiles(ctx, ws)
	if err != nil {
		return tokmut.Suite{}, err
	}
	utils, err := utilities(ws)
	if err != nil {
		return tokmut.Suite{}, err
	}
	files, err := scripts(ctx, ws)
	inside := func(dir string) gate.Workspace {
		sandboxed := ws
		sandboxed.Src = dir
		return sandboxed
	}
	return tokmut.Suite{
		Family:  tokmut.Shell,
		Files:   files,
		Seconds: junit.Seconds(cases),
		Covered: func(rel string, line int) (bool, []string) {
			hit, _ := cov.Covered(rel, line)
			return hit, index[rel][line]
		},
		Compile: func(ctx context.Context, _ int, dir, rel string) error {
			_, cerr := inside(dir).Run(ctx, BashBinary, "-n", rel)
			return cerr
		},
		Test: func(ctx context.Context, _ int, dir string, covering []string) error {
			run := tests
			if len(covering) > 0 {
				run = covering
			}
			_, terr := inside(dir).RunEnv(ctx, utils, BashBinary, batsArgs(append([]string{"--abort"}, run...)...)...)
			return terr
		},
	}, err
}
