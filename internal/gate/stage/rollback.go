package stage

import (
	"context"
	"fmt"
	"net/http"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	purposeRollback  = "rollback"
	reasonNoPrevious = "no deployed release to roll back to"
	reasonNoData     = "the app keeps no data, so a rollback cannot meet data it does not know"
	reasonNoStart    = "this release does not start on seeded data in the stage"
	reasonBackward   = "the deployed release does not work on the data this release migrated: a rollback would fail"
	pathFailed       = "%s answered %d"
	firstServerError = 500
)

func Rollback(ctx context.Context, ws gate.Workspace) gate.Outcome {
	switch {
	case ws.Previous == "":
		return gate.Outcome{Status: gate.StatusNA, Reason: reasonNoPrevious}
	case len(ws.App.Data) == 0:
		return gate.Outcome{Status: gate.StatusNA, Reason: reasonNoData}
	}
	var seed []string
	if ws.App.UI != nil {
		seed = ws.App.UI.Seed
	}
	st, err := StartSeeded(ctx, ws, purposeRollback, seed)
	if err != nil {
		return gate.Fail(reasonNoStart, err.Error())
	}
	defer st.Stop(ctx)
	if err := st.Swap(ctx, ws.Previous); err != nil {
		return gate.Fail(reasonBackward, ws.Previous+": "+err.Error())
	}
	return gate.Findings(reasonBackward, st.serverErrors(ctx))
}

func (s *Stage) serverErrors(ctx context.Context) []string {
	client := &http.Client{Timeout: probeTimeout}
	var findings []string
	for _, path := range s.ws.App.Load.Paths {
		code := status(ctx, client, s.URL(path))
		if code == 0 || code >= firstServerError {
			findings = append(findings, fmt.Sprintf(pathFailed, path, code))
		}
	}
	return findings
}

func status(ctx context.Context, client *http.Client, url string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
