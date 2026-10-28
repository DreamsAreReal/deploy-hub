package arch

import (
	"fmt"
	"slices"
	"strings"
)

const (
	metricPrefix = "deploy_hub_arch_"
	reportHead   = "# Architecture report\n\nComputed from `policy/architecture.yaml` by code; no model involved.\n\n"
	metricsHead  = "| Services | Links | Secrets | Public sites | Longest call chain |\n|---|---|---|---|---|\n"
	servicesHead = "\n| Service | Calls | Called by | If it is hacked, it can call | If it is down, these stop |\n|---|---|---|---|---|\n"
	noneMark     = "-"
	listJoin     = ", "
)

type Metrics struct {
	Services     int
	Links        int
	Secrets      int
	Public       int
	LongestChain int
	In           map[string]int
	Out          map[string]int
}

func (c Catalog) names() []string {
	names := make([]string, 0, len(c.Services))
	for name := range c.Services {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (c Catalog) callees(name string) []string {
	var out []string
	for _, call := range c.Services[name].Calls {
		out = appendNew(out, call.To)
	}
	return out
}

func (c Catalog) callers(name string) []string {
	var in []string
	for _, other := range c.names() {
		if slices.Contains(c.callees(other), name) {
			in = append(in, other)
		}
	}
	return in
}

func (c Catalog) Metrics() Metrics {
	m := Metrics{Services: len(c.Services), In: map[string]int{}, Out: map[string]int{}}
	var secrets []string
	for _, name := range c.names() {
		service := c.Services[name]
		out := c.callees(name)
		m.Out[name], m.In[name] = len(out), len(c.callers(name))
		m.Links += len(out)
		secrets = appendNew(secrets, service.Secrets...)
		for _, expose := range service.Exposes {
			if expose.Site != "" {
				m.Public++
			}
		}
		m.LongestChain = max(m.LongestChain, c.longest(name, map[string]bool{}))
	}
	m.Secrets = len(secrets)
	return m
}

func (c Catalog) longest(name string, seen map[string]bool) int {
	seen[name] = true
	defer delete(seen, name)
	best := 0
	for _, next := range c.callees(name) {
		if !seen[next] {
			best = max(best, 1+c.longest(next, seen))
		}
	}
	return best
}

func (c Catalog) closure(name string, step func(string) []string) []string {
	seen := map[string]bool{name: true}
	queue := []string{name}
	var out []string
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range step(current) {
			if !seen[next] {
				seen[next] = true
				out = append(out, next)
				queue = append(queue, next)
			}
		}
	}
	slices.Sort(out)
	return out
}

func (c Catalog) Reach(name string) []string {
	return c.closure(name, c.callees)
}

func (c Catalog) Dependents(name string) []string {
	return c.closure(name, c.callers)
}

func (c Catalog) Report() string {
	m := c.Metrics()
	var out strings.Builder
	out.WriteString(reportHead + metricsHead)
	fmt.Fprintf(&out, "| %d | %d | %d | %d | %d |\n", m.Services, m.Links, m.Secrets, m.Public, m.LongestChain)
	out.WriteString(servicesHead)
	for _, name := range c.names() {
		fmt.Fprintf(&out, "| %s | %d | %d | %s | %s |\n", name, m.Out[name], m.In[name], listOrNone(c.Reach(name)), listOrNone(c.Dependents(name)))
	}
	return out.String()
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return noneMark
	}
	return strings.Join(items, listJoin)
}

func (c Catalog) Exposition() string {
	m := c.Metrics()
	var out strings.Builder
	for _, total := range []struct {
		name  string
		value int
	}{{"services", m.Services}, {"links", m.Links}, {"secrets", m.Secrets}, {"public_sites", m.Public}, {"longest_chain", m.LongestChain}} {
		fmt.Fprintf(&out, "%s%s %d\n", metricPrefix, total.name, total.value)
	}
	for _, name := range c.names() {
		fmt.Fprintf(&out, "%sreach{service=%q} %d\n", metricPrefix, name, len(c.Reach(name)))
		fmt.Fprintf(&out, "%sdependents{service=%q} %d\n", metricPrefix, name, len(c.Dependents(name)))
	}
	return out.String()
}

func (c Catalog) budgetFindings() []string {
	if c.LinkBudget <= 0 {
		return nil
	}
	if links := c.Metrics().Links; links > c.LinkBudget {
		return []string{fmt.Sprintf("the catalog has %d links, over the budget of %d: remove one before adding another", links, c.LinkBudget)}
	}
	return nil
}
