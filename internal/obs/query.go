package obs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

const (
	queryPath     = "/api/v1/query"
	queryParam    = "query"
	statusSuccess = "success"
	maxQueryBody  = 1 << 20
	valueFields   = 2
)

var ErrQuery = errors.New("metrics query failed")

type HTTPQuery struct {
	URL    string
	Client *http.Client
}

func QueryURL(importURL string) string {
	u, err := url.Parse(importURL)
	if importURL == "" || err != nil {
		return ""
	}
	u.Path, u.RawQuery = queryPath, ""
	return u.String()
}

type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func (q HTTPQuery) Scalar(ctx context.Context, expr string) (float64, bool, error) {
	if q.URL == "" {
		return 0, false, fmt.Errorf("%w: no metrics URL", ErrQuery)
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.URL+"?"+url.Values{queryParam: {expr}}.Encode(), nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := q.Client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= firstBadStatus {
		return 0, false, fmt.Errorf("%w: status %d", ErrQuery, resp.StatusCode)
	}
	return decodeScalar(io.LimitReader(resp.Body, maxQueryBody))
}

func decodeScalar(r io.Reader) (float64, bool, error) {
	var out queryResponse
	if err := json.NewDecoder(r).Decode(&out); err != nil || out.Status != statusSuccess {
		return 0, false, fmt.Errorf("%w: unreadable answer (status %q)", ErrQuery, out.Status)
	}
	if len(out.Data.Result) == 0 {
		return 0, false, nil
	}
	v := out.Data.Result[0].Value
	var raw string
	if len(v) != valueFields || json.Unmarshal(v[1], &raw) != nil {
		return 0, false, fmt.Errorf("%w: unexpected sample", ErrQuery)
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %s", ErrQuery, err.Error())
	}
	return f, true, nil
}
