package dotnet

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	Binary         = "dotnet"
	Stack          = "dotnet"
	lockFile       = "packages.lock.json"
	stageDir       = "src"
	configDir      = "dotnet-config"
	readyStamp     = "dotnet.ready"
	policySub      = "dotnet"
	beforeProps    = "before.props"
	afterTargets   = "after.targets"
	editorConfig   = "editorconfig"
	sonarLint      = "SonarLint.xml"
	stampPerm      = 0o600
	dirPerm        = 0o700
	sonarTemplate  = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<AnalysisInput>\n  <Rules>\n    <Rule>\n      <Key>S3776</Key>\n      <Parameters>\n        <Parameter><Key>threshold</Key><Value>%d</Value></Parameter>\n        <Parameter><Key>propertyThreshold</Key><Value>%d</Value></Parameter>\n      </Parameters>\n    </Rule>\n  </Rules>\n</AnalysisInput>\n"
	propBefore     = "-p:CustomBeforeMicrosoftCommonProps="
	propAfter      = "-p:CustomAfterMicrosoftCommonTargets="
	propSonarLint  = "-p:DeployHubSonarLint="
	flagNoRestore  = "--no-restore"
	flagVerbosity  = "-v"
	verbosityQuiet = "q"
	csharpSource   = ".cs"
	cmdBuild       = "build"
	cmdTest        = "test"
	flagLogger     = "--logger"
)

var (
	Manifests       = []string{"*.sln", "*.slnx", "*.csproj", "*.fsproj", "*.vbproj"}
	Sources         = []string{".cs", ".fs", ".vb"}
	projects        = []string{".csproj", ".fsproj", ".vbproj"}
	analyzerConfigs = []string{".editorconfig", ".globalconfig"}
	baseEnv         = []string{"DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1"}
)

func stage(ws gate.Workspace) string {
	return ws.Report(stageDir)
}

func config(ws gate.Workspace, name string) string {
	return filepath.Join(ws.Report(configDir), name)
}

func analysisProps(ws gate.Workspace) []string {
	return []string{propBefore + config(ws, beforeProps), propAfter + config(ws, afterTargets), propSonarLint + config(ws, sonarLint)}
}

func run(ctx context.Context, ws gate.Workspace, dir string, env []string, args ...string) (procexec.Result, error) {
	ws.Src = dir
	res, err := ws.RunEnv(ctx, append(slices.Clone(baseEnv), env...), Binary, args...)
	if err != nil {
		return res, fmt.Errorf("dotnet %s: %w: %s", args[0], err, gate.Tail(procexec.Output(err)))
	}
	return res, nil
}

var (
	prepared = pack.Prepared(prepare)
	Tests    = prepared(runTests)
	Fuzz     = prepared(runFuzz)
)

func prepare(ctx context.Context, ws gate.Workspace) error {
	return pack.Once(ctx, ws, readyStamp, stageProject, lockedRestore, writeConfig, analysisRestore)
}

func stageProject(ctx context.Context, ws gate.Workspace) error {
	return source.CopyTracked(ctx, ws, stage(ws))
}

func lockedRestore(ctx context.Context, ws gate.Workspace) error {
	_, err := run(ctx, ws, stage(ws), nil, "restore", "--locked-mode", flagVerbosity, verbosityQuiet)
	return err
}

func writeConfig(_ context.Context, ws gate.Workspace) error {
	if err := os.MkdirAll(ws.Report(configDir), dirPerm); err != nil {
		return err
	}
	for _, name := range []string{beforeProps, afterTargets} {
		if err := copyFile(filepath.Join(ws.PolicyDir, policySub, name), config(ws, name)); err != nil {
			return err
		}
	}
	limit := ws.Policy.Thresholds.MaxCognitive
	if err := os.WriteFile(config(ws, sonarLint), fmt.Appendf(nil, sonarTemplate, limit, limit), stampPerm); err != nil {
		return err
	}
	if err := neutralize(stage(ws)); err != nil {
		return err
	}
	return copyFile(filepath.Join(ws.PolicyDir, policySub, editorConfig), filepath.Join(stage(ws), analyzerConfigs[0]))
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(filepath.Clean(from))
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.WriteFile(filepath.Base(to), data, stampPerm)
}

func neutralize(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !d.IsDir() && (slices.Contains(analyzerConfigs, d.Name()) || d.Name() == lockFile) {
			return root.Remove(p)
		}
		return nil
	})
}

func analysisRestore(ctx context.Context, ws gate.Workspace) error {
	_, err := run(ctx, ws, stage(ws), nil, append([]string{"restore", flagVerbosity, verbosityQuiet}, analysisProps(ws)...)...)
	return err
}

func rel(ws gate.Workspace, p string) string {
	return gate.RelTo(stage(ws), p)
}

func isTest(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		lower := strings.ToLower(part)
		if lower == "test" || lower == "tests" || strings.HasSuffix(lower, ".tests") || strings.HasSuffix(lower, ".test") {
			return true
		}
	}
	return false
}

func isProject(f string) bool {
	return slices.Contains(projects, path.Ext(f))
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
