package arch

import (
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	webName = "web"
	apiName = "api"
	dbName  = "db"
)

const graphBody = `link_budget: 3
services:
  web:
    critical: high
    exposes: [{site: web}, {port: 8080}]
    calls: [{to: api, why: data}, {to: api, why: twice}]
    secrets: [WEB_KEY]
  api:
    critical: high
    calls: [{to: db, why: rows}, {to: web, why: callback}]
    secrets: [API_KEY, WEB_KEY]
  db:
    critical: high
    exposes: [{site: admin}]
  lone:
    critical: low
`

func TestMetricsCountTheCatalog(t *testing.T) {
	m := mustParse(t, graphBody).Metrics()
	testutil.Equal(t, []int{m.Services, m.Links, m.Secrets, m.Public, m.LongestChain}, []int{4, 3, 2, 2, 2})
	testutil.Equal(t, []int{m.Out["web"], m.In["web"], m.Out["api"], m.In["db"], m.In["lone"]}, []int{1, 1, 2, 1, 0})
}

func TestReachAndDependentsFollowTheCalls(t *testing.T) {
	c := mustParse(t, graphBody)
	testutil.Equal(t, c.Reach(webName), []string{apiName, dbName})
	testutil.Equal(t, c.Dependents(dbName), []string{apiName, webName})
	testutil.Equal(t, len(c.Reach("lone"))+len(c.Dependents("lone")), 0)
}

func TestReportAndExpositionSayTheSame(t *testing.T) {
	c := mustParse(t, graphBody)
	testutil.Contains(t, c.Report(), "| 4 | 3 | 2 | 2 | 2 |", "| web | 1 | 1 | api, db | api |", "| lone | 0 | 0 | - | - |")
	testutil.Contains(t, c.Exposition(), "deploy_hub_arch_links 3\n", `deploy_hub_arch_reach{service="web"} 2`, `deploy_hub_arch_dependents{service="db"} 2`)
}

func TestLinkBudgetWarnsWithoutFailing(t *testing.T) {
	over := strings.Replace(cleanEntry, "services:\n", "link_budget: 1\nservices:\n  b:\n    critical: low\n    calls: [{to: c, why: x}, {to: app, why: y}]\n  c:\n    critical: low\n", 1)
	outcome := Check(t.Context(), workspace(t, over, repoWith(t)))
	testutil.Equal(t, outcome.Status, gate.StatusPass)
	testutil.Contains(t, strings.Join(outcome.Findings, "; "), "2 links, over the budget of 1")
	testutil.Equal(t, len(mustParse(t, cleanEntry).budgetFindings()), 0)
	testutil.Equal(t, len(mustParse(t, graphBody).budgetFindings()), 0)
}
