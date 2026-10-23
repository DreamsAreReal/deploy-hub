package mutation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

var EngineVersion string

const (
	enginePackage  = "./internal/gate/mutation"
	engineListing  = "{{if not .Standard}}{{.Dir}}{{range .GoFiles}} {{.}}{{end}}{{end}}"
	engineFailed   = "engine version: %w: %s"
	logUnversioned = "this deploy-hub was built without an engine version (use make); no mutation verdict is cached or reused"
)

var engineExcluded = []string{"internal/gate/policy", "internal/validate"}

func SourceVersion(ctx context.Context, ws gate.Workspace) (string, error) {
	mod, err := ws.Run(ctx, golang.GoBinary, "list", "-m", "-f", "{{.Dir}}")
	var listing procexec.Result
	if err == nil {
		listing, err = ws.Run(ctx, golang.GoBinary, "list", "-deps", "-f", engineListing, enginePackage)
	}
	if err != nil {
		return "", fmt.Errorf(engineFailed, err, gate.Tail(procexec.Output(err)))
	}
	root := strings.TrimSpace(mod.Stdout)
	var parts []string
	for _, line := range strings.Split(strings.TrimSpace(listing.Stdout), "\n") {
		files, ferr := engineFiles(root, strings.Fields(line))
		if ferr != nil {
			return "", ferr
		}
		parts = append(parts, files...)
	}
	return hashOf(parts...), nil
}

func engineFiles(root string, fields []string) ([]string, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	rel, err := filepath.Rel(root, fields[0])
	if err != nil || rel == parentDirElem || strings.HasPrefix(rel, parentDirElem+string(filepath.Separator)) || slices.Contains(engineExcluded, filepath.ToSlash(rel)) {
		return nil, nil
	}
	dir, err := os.OpenRoot(fields[0])
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	parts := make([]string, 0, len(fields)+len(fields))
	for _, f := range fields[1:] {
		data, rerr := dir.ReadFile(f)
		if rerr != nil {
			return nil, rerr
		}
		parts = append(parts, filepath.ToSlash(filepath.Join(rel, f)), string(data))
	}
	return parts, nil
}
