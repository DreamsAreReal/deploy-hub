package obs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func queryServer(t *testing.T, status int, body string) (HTTPQuery, *string) {
	t.Helper()
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path + " " + r.URL.Query().Get("query")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return HTTPQuery{URL: srv.URL + "/api/v1/query", Client: srv.Client()}, &asked
}

func TestQueryReadsTheFirstSample(t *testing.T) {
	q, asked := queryServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1793449719,"2"]}]}}`)
	v, ok, err := q.Scalar(context.Background(), `max(tasks_runner_info{version=~"abc.*"})`)
	testutil.NoErr(t, err)
	testutil.True(t, ok && v == 2, "value 2 present")
	testutil.Equal(t, *asked, `/api/v1/query max(tasks_runner_info{version=~"abc.*"})`)
}

func TestEmptyQueryResultIsAbsentNotZero(t *testing.T) {
	q, _ := queryServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	v, ok, err := q.Scalar(context.Background(), "x")
	testutil.NoErr(t, err)
	testutil.True(t, !ok && v == 0, "absent")
}

func TestBadQueryAnswersAreErrors(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"status":        {http.StatusBadRequest, `{"status":"fail"}`},
		"not json":      {http.StatusOK, `<html>`},
		"error status":  {http.StatusOK, `{"status":"error"}`},
		"short sample":  {http.StatusOK, `{"status":"success","data":{"result":[{"value":[1]}]}}`},
		"number sample": {http.StatusOK, `{"status":"success","data":{"result":[{"value":[1,2]}]}}`},
		"not a number":  {http.StatusOK, `{"status":"success","data":{"result":[{"value":[1,"x"]}]}}`},
	} {
		t.Run(name, func(t *testing.T) {
			q, _ := queryServer(t, c.status, c.body)
			_, _, err := q.Scalar(context.Background(), "x")
			testutil.Is(t, err, ErrQuery)
		})
	}
}

func TestQueryWithoutAURLOrServerFails(t *testing.T) {
	_, _, err := HTTPQuery{}.Scalar(context.Background(), "x")
	testutil.Is(t, err, ErrQuery)
	_, _, err = HTTPQuery{URL: "http://127.0.0.1:1/api/v1/query", Client: &http.Client{}}.Scalar(context.Background(), "x")
	testutil.Fails(t, err)
	_, _, err = HTTPQuery{URL: "http://bad\x7f/", Client: &http.Client{}}.Scalar(context.Background(), "x")
	testutil.Fails(t, err)
}

func TestQueryURLFollowsTheImportURL(t *testing.T) {
	testutil.Equal(t, QueryURL("http://127.0.0.1:8428/api/v1/import/prometheus"), "http://127.0.0.1:8428/api/v1/query")
	testutil.Equal(t, QueryURL(""), "")
	testutil.Equal(t, QueryURL("http://[::1"), "")
}
