package node

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
)

var Mutation = tokmut.Check(mutationSuite)

func mutationSuite(ctx context.Context, ws gate.Workspace) (tokmut.Suite, error) {
	r, err := project(ws)
	if err != nil {
		return tokmut.Suite{}, err
	}
	cov, err := linecov.Load(ws)
	if err != nil {
		return tokmut.Suite{}, err
	}
	cases, err := results(ws, r, "")
	if err != nil {
		return tokmut.Suite{}, err
	}
	files, err := productionFiles(ctx, ws)
	files = slices.DeleteFunc(files, func(f string) bool { return !slices.Contains(scripts, path.Ext(f)) })
	return tokmut.Suite{
		Family:  tokmut.JavaScript,
		Files:   files,
		Seconds: junit.Seconds(cases),
		Covered: cov.Covered,
		Prepare: func(_ context.Context, _ int, dir string) error {
			return os.Symlink(filepath.Join(stage(ws), modulesDir), filepath.Join(dir, modulesDir))
		},
		Compile: func(ctx context.Context, _ int, dir, rel string) error {
			_, cerr := run(ctx, ws, dir, nil, NodeBinary, filepath.Join(tools(ws), syntaxCheckFile), filepath.Join(dir, filepath.FromSlash(rel)))
			return cerr
		},
		Test: func(ctx context.Context, _ int, dir string, _ []string) error {
			_, terr := run(ctx, ws, dir, nil, NodeBinary, adapters[r].mutant(ws)...)
			return terr
		},
	}, err
}
