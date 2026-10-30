package accesslog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/arch"
)

const (
	DefaultURL   = "http://127.0.0.1:9428/select/logsql/query"
	queryFormat  = "_time:%s log_type:access remote_ip:in(%s) | stats by (host, user_agent) count() hits"
	window       = "10m"
	readTimeout  = 5 * time.Second
	formType     = "application/x-www-form-urlencoded"
	queryField   = "query"
	addressJoin  = ","
	domainPrefix = "."
)

var ErrStatus = errors.New("access log query failed")

type Reader struct {
	URL       string
	Domain    string
	Client    *http.Client
	Addresses func() ([]net.Addr, error)
}

type row struct {
	Host  string `json:"host"`
	Agent string `json:"user_agent"`
}

func (r Reader) FromServer(ctx context.Context) ([]arch.Visit, error) {
	addrs, err := r.Addresses()
	if err != nil {
		return nil, err
	}
	var quoted []string
	for _, addr := range addrs {
		if ip, ok := addr.(*net.IPNet); ok && !ip.IP.IsLinkLocalUnicast() {
			quoted = append(quoted, strconv.Quote(ip.IP.String()))
		}
	}
	return r.visits(ctx, fmt.Sprintf(queryFormat, window, strings.Join(quoted, addressJoin)))
}

func (r Reader) visits(ctx context.Context, query string) ([]arch.Visit, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	form := url.Values{queryField: {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", formType)
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrStatus, resp.StatusCode)
	}
	var visits []arch.Visit
	lines := bufio.NewScanner(resp.Body)
	for lines.Scan() {
		var line row
		if err := json.Unmarshal(lines.Bytes(), &line); err != nil {
			return nil, err
		}
		visits = append(visits, arch.Visit{Site: r.site(line.Host), Agent: line.Agent})
	}
	return visits, lines.Err()
}

func (r Reader) site(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	site, _ := strings.CutSuffix(host, domainPrefix+r.Domain)
	if site == host || r.Domain == "" {
		return ""
	}
	return site
}
