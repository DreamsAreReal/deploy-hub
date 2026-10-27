package obs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	alertRulesFile = "../../server/vmalert/deploy-hub.yml"
	namesFile      = "names.go"
	metricPrefix   = "Metric"
)

var metricRef = regexp.MustCompile(`deploy_hub_[a-z_]+`)

type alertRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Annotations map[string]string `yaml:"annotations"`
}

type alertFile struct {
	Groups []struct {
		Name     string      `yaml:"name"`
		Interval string      `yaml:"interval"`
		Rules    []alertRule `yaml:"rules"`
	} `yaml:"groups"`
}

func alertRules(t *testing.T) []alertRule {
	t.Helper()
	data, err := os.ReadFile(alertRulesFile)
	testutil.NoErr(t, err)
	var f alertFile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	testutil.NoErr(t, dec.Decode(&f))
	var rules []alertRule
	for _, g := range f.Groups {
		rules = append(rules, g.Rules...)
	}
	return rules
}

func declaredMetrics(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), namesFile, nil, 0)
	testutil.NoErr(t, err)
	metrics := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || !strings.HasPrefix(spec.Names[0].Name, metricPrefix) {
			return true
		}
		lit, isLiteral := spec.Values[0].(*ast.BasicLit)
		testutil.True(t, isLiteral, "metric names are literals")
		value, err := strconv.Unquote(lit.Value)
		testutil.NoErr(t, err)
		metrics[value] = true
		return true
	})
	return metrics
}

func TestAlertRulesAreCompleteAndReferenceDeclaredMetrics(t *testing.T) {
	declared := declaredMetrics(t)
	rules := alertRules(t)
	testutil.True(t, len(rules) > 0, "no alert rules")
	seen := map[string]bool{}
	for _, r := range rules {
		testutil.True(t, r.Alert != "" && r.Expr != "", "rule without name or expression")
		testutil.True(t, !seen[r.Alert], "duplicate alert "+r.Alert)
		seen[r.Alert] = true
		testutil.True(t, r.Annotations["summary"] != "" && r.Annotations["description"] != "", r.Alert+" lacks summary or description")
		refs := metricRef.FindAllString(r.Expr, -1)
		testutil.True(t, len(refs) > 0, r.Alert+" watches no deploy-hub metric")
		for _, m := range refs {
			testutil.True(t, declared[m], r.Alert+" uses undeclared metric "+m)
		}
	}
}

func TestEveryProblemSignalHasAnAlert(t *testing.T) {
	watched := map[string]bool{}
	for _, r := range alertRules(t) {
		for _, m := range metricRef.FindAllString(r.Expr, -1) {
			watched[m] = true
		}
	}
	for _, m := range []string{
		MetricCommandExitCode, MetricCommandPanicked, MetricCommandLogRecords,
		MetricOperationsTotal, MetricGateCheckVerdict, MetricGateCheckValue,
		MetricRequestRefused, MetricAppHealthy, MetricCaddySyncSuccess,
		MetricLockWait, MetricDeliverySuccess, MetricJournalReadable,
		MetricCIRunnerUp, MetricCIDiskUsedRatio, MetricCIIsolationOK, MetricHostSyncSuccess, MetricSelfUpdateSuccess,
		MetricCIQuotaOn, MetricCIQuotaUsedRatio,
		MetricPreemptSuccess, MetricPreemptRerunsMax,
		MetricMutationCacheIntact,
		MetricFullGateSuccess, MetricFullGateVerdict, MetricFullGateAge,
	} {
		testutil.True(t, watched[m], "no alert watches "+m)
	}
}
