package dotnet

import (
	"context"
	"path"
	"slices"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
)

var (
	plainBuild = append(slices.Clone(quietAnalyzers), "-p:GenerateDocumentationFile=false", flagVerbosity, verbosityQuiet)
	Mutation   = tokmut.Check(mutationSuite)
)

func mutationSuite(ctx context.Context, ws gate.Workspace) (tokmut.Suite, error) {
	cov, err := linecov.Load(ws)
	if err != nil {
		return tokmut.Suite{}, err
	}
	cases, err := loadTRX(ws.Report(TRXReport))
	if err != nil {
		return tokmut.Suite{}, err
	}
	seconds := 0.0
	for _, c := range cases {
		seconds += c.Time
	}
	files, err := source.Select(ctx, ws, func(f string) bool { return path.Ext(f) == csharpSource && !isTest(f) })
	build := func(ctx context.Context, dir string, flags ...string) error {
		_, berr := run(ctx, ws, dir, nil, slices.Concat([]string{cmdBuild}, flags, plainBuild)...)
		return berr
	}
	return tokmut.Suite{
		Family:  tokmut.CSharp,
		Files:   files,
		Seconds: seconds,
		Covered: cov.Covered,
		Prepare: func(ctx context.Context, _ int, dir string) error { return build(ctx, dir) },
		Compile: func(ctx context.Context, _ int, dir, _ string) error { return build(ctx, dir, flagNoRestore) },
		Test: func(ctx context.Context, _ int, dir string, _ []string) error {
			_, terr := run(ctx, ws, dir, nil, cmdTest, "--no-build", flagNoRestore, flagVerbosity, verbosityQuiet)
			return terr
		},
	}, err
}
