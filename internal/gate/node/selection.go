package node

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
)

const (
	indexDir      = "test-index"
	indexExt      = ".info"
	indexDirPerm  = 0o700
	testDirectory = "test"
)

var nodeTestName = regexp.MustCompile(`^(.+[.\-_]test|test-.+|test)\.[cm]?js$`)

type testIndex map[string]map[int][]string

func (ix testIndex) tests(rel string, line int) []string {
	return ix[rel][line]
}

func nodeTestFile(rel string) bool {
	if source.Fixture(rel) || !slices.Contains([]string{extJS, extMJS, extCJS}, path.Ext(rel)) {
		return false
	}
	return nodeTestName.MatchString(path.Base(rel)) || slices.Contains(strings.Split(path.Dir(rel), "/"), testDirectory)
}

func buildIndex(ctx context.Context, ws gate.Workspace) (testIndex, error) {
	files, err := source.Select(ctx, ws, nodeTestFile)
	if err != nil {
		return nil, err
	}
	dir := ws.Report(indexDir)
	if err := os.MkdirAll(dir, indexDirPerm); err != nil {
		return nil, err
	}
	index := testIndex{}
	for i, file := range files {
		report := filepath.Join(dir, strconv.Itoa(i)+indexExt)
		_, _ = node(ctx, ws, nil, flagTest, "--experimental-test-coverage", "--test-reporter=lcov", "--test-reporter-destination="+report, file)
		cov := linecov.Report{}
		if err := readLcovFile(ws, report, cov); err != nil {
			return nil, err
		}
		index.add(cov, file)
	}
	return index, nil
}

func (ix testIndex) add(cov linecov.Report, test string) {
	for rel, f := range cov {
		for line, hits := range f.Lines {
			if hits == 0 {
				continue
			}
			if ix[rel] == nil {
				ix[rel] = map[int][]string{}
			}
			ix[rel][line] = append(ix[rel][line], test)
		}
	}
}
