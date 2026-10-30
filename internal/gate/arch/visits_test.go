package arch

import (
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	botSite     = "bot"
	speechSite  = "speech"
	plainClient = "Go-http-client/1.1"
	manualCurl  = "curl/8.5.0"
)

func TestVisitDriftNamesUnnamedAndUndeclaredCallers(t *testing.T) {
	catalog := Catalog{Services: map[string]Service{
		TasksService:         {Exposes: []Expose{{Site: TasksService}, {Port: 8787}}},
		botSite:              {Gives: gives[:1], Exposes: []Expose{{Site: botSite}}},
		speechSite:           {Exposes: []Expose{{Site: speechSite}}},
		ObservabilityService: {Calls: []Call{{To: TasksService}}},
	}}
	drift := catalog.HostDrift(Facts{Visits: []Visit{
		{Site: TasksService, Agent: ObservabilityService + "/obs-host-metrics"},
		{Site: botSite, Agent: ObservabilityService},
		{Site: TasksService, Agent: TasksService + "/1.0"},
		{Site: TasksService, Agent: plainClient},
		{Site: TasksService, Agent: plainClient},
		{Site: speechSite, Agent: botSite + "/2"},
		{Site: botSite, Agent: ""},
		{Site: "", Agent: manualCurl},
		{Site: "p1.preview", Agent: manualCurl},
	}})
	testutil.Equal(t, drift, []string{
		`site tasks gets requests from the server itself with User-Agent "Go-http-client/1.1", which names no service of the catalog`,
		"bot calls site speech of speech from the server, and the catalog has no call between them",
		`site bot gets requests from the server itself with User-Agent "", which names no service of the catalog`,
	})
}
