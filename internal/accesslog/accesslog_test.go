package accesslog

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/arch"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	domain     = "example.org"
	firstAgent = "a/1"
	tasksSite  = "tasks"
)

var errNoInterfaces = errors.New("no interfaces")

func addresses() ([]net.Addr, error) {
	return []net.Addr{
		&net.IPNet{IP: net.ParseIP("127.0.0.1")},
		&net.IPNet{IP: net.ParseIP("fe80::1")},
		&net.IPAddr{IP: net.ParseIP("10.0.0.1")},
		&net.IPNet{IP: net.ParseIP("2a02:c207::1")},
		&net.IPNet{IP: net.ParseIP("13.140.152.253")},
	}, nil
}

func logStore(t *testing.T, status int, body string) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != formType {
			t.Errorf("%s with %q", r.Method, r.Header.Get("Content-Type"))
		}
		queries = append(queries, r.FormValue("query"))
		w.WriteHeader(status)
		_, _ = io.Copy(w, strings.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func TestFromServerAsksForTheServerAddressesAndNamesSites(t *testing.T) {
	srv, queries := logStore(t, http.StatusOK, `{"host":"tasks.example.org","user_agent":"a/1","hits":"3"}`+"\n"+
		`{"host":"tasks.example.org:443","user_agent":"b"}`+"\n"+`{"host":"example.org","user_agent":"c"}`+"\n"+
		`{"host":"other.net","user_agent":"d"}`+"\n"+`{"host":"[2a02:c207::1]:443"}`+"\n")
	reader := Reader{URL: srv.URL, Domain: domain, Client: srv.Client(), Addresses: addresses}
	visits, err := reader.FromServer(context.Background())
	testutil.NoErr(t, err)
	testutil.Equal(t, visits, []arch.Visit{{Site: tasksSite, Agent: firstAgent}, {Site: tasksSite, Agent: "b"}, {Agent: "c"}, {Agent: "d"}, {}})
	testutil.Equal(t, *queries, []string{`_time:10m log_type:access remote_ip:in("127.0.0.1","2a02:c207::1","13.140.152.253") | stats by (host, user_agent) count() hits`})
	reader.Domain = ""
	visits, err = reader.FromServer(context.Background())
	testutil.NoErr(t, err)
	testutil.Equal(t, visits, []arch.Visit{{Agent: firstAgent}, {Agent: "b"}, {Agent: "c"}, {Agent: "d"}, {}})
}

func TestFromServerReportsEveryFailure(t *testing.T) {
	ok, _ := logStore(t, http.StatusOK, "")
	down, _ := logStore(t, http.StatusServiceUnavailable, "")
	broken, _ := logStore(t, http.StatusOK, "{\"host\":\n")
	long, _ := logStore(t, http.StatusOK, strings.Repeat("x", 70_000))
	closed, _ := logStore(t, http.StatusOK, "")
	closed.Close()
	cases := map[string]Reader{
		"interfaces": {URL: ok.URL, Client: ok.Client(), Addresses: func() ([]net.Addr, error) { return nil, errNoInterfaces }},
		"url":        {URL: "http://bad\x7f", Client: ok.Client(), Addresses: addresses},
		"refused":    {URL: closed.URL, Client: closed.Client(), Addresses: addresses},
		"status":     {URL: down.URL, Client: down.Client(), Addresses: addresses},
		"json":       {URL: broken.URL, Client: broken.Client(), Addresses: addresses},
		"too long":   {URL: long.URL, Client: long.Client(), Addresses: addresses},
	}
	for name, reader := range cases {
		visits, err := reader.FromServer(context.Background())
		if err == nil || visits != nil {
			t.Errorf("%s: %v %v", name, visits, err)
		}
	}
	_, err := cases["status"].FromServer(context.Background())
	testutil.Is(t, err, ErrStatus)
	testutil.Contains(t, err.Error(), "HTTP 503")
	_, err = cases["interfaces"].FromServer(context.Background())
	testutil.Is(t, err, errNoInterfaces)
	empty, err := Reader{URL: ok.URL, Client: ok.Client(), Addresses: addresses}.FromServer(context.Background())
	testutil.NoErr(t, err)
	testutil.Equal(t, empty, []arch.Visit(nil))
}
