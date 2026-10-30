package arch

import (
	"fmt"
	"slices"
	"strings"
)

const (
	agentSeparator = "/"
	unnamedCaller  = "site %s gets requests from the server itself with User-Agent %q, which names no service of the catalog"
	undeclaredCall = "%s calls site %s of %s from the server, and the catalog has no call between them"
)

type Visit struct {
	Site  string
	Agent string
}

func (c Catalog) visitDrift(visits []Visit) []string {
	var drift []string
	for _, visit := range visits {
		if target, ok := c.serving(visit.Site); ok {
			drift = appendNew(drift, c.callerDrift(visit, target)...)
		}
	}
	return drift
}

func (c Catalog) serving(site string) (string, bool) {
	for _, name := range c.names() {
		if slices.Contains(c.Services[name].siteNames(), site) {
			return name, true
		}
	}
	return "", false
}

func (c Catalog) callerDrift(visit Visit, target string) []string {
	caller, _, _ := strings.Cut(visit.Agent, agentSeparator)
	if _, known := c.Services[caller]; !known {
		return []string{fmt.Sprintf(unnamedCaller, visit.Site, visit.Agent)}
	}
	if caller == target || c.Linked(caller, target) {
		return nil
	}
	return []string{fmt.Sprintf(undeclaredCall, caller, visit.Site, target)}
}
