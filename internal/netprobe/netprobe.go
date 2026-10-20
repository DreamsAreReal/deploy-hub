package netprobe

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Loopback       = "127.0.0.1"
	dialTimeout    = 2 * time.Second
	firstBadStatus = 400
	MaxPort        = 65535
	listenState    = "0A"
	hexBase        = 16
	portBits       = 16
	localAddrField = 1
	stateField     = 3
)

type Prober struct {
	Client *http.Client
}

func NewProber() Prober {
	return Prober{Client: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func HTTPURL(port, path string) string {
	return "http://" + net.JoinHostPort(Loopback, port) + path
}

func (p Prober) HTTPOK(ctx context.Context, url string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < firstBadStatus
}

func Listening(port int) bool {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort(Loopback, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func FreePort(start int, listening func(int) bool) int {
	p := start
	for p < MaxPort && listening(p) {
		p++
	}
	return p
}

func ListeningPorts(files []string) []int {
	set := map[int]bool{}
	for _, f := range files {
		for _, p := range listeningIn(f) {
			set[p] = true
		}
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func listeningIn(path string) []int {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, parseProcLine(sc.Text())...)
	}
	return out
}

func parseProcLine(line string) []int {
	fields := strings.Fields(line)
	if len(fields) <= stateField || fields[stateField] != listenState {
		return nil
	}
	_, hexPort, found := strings.Cut(fields[localAddrField], ":")
	port, err := strconv.ParseUint(hexPort, hexBase, portBits)
	if !found || err != nil {
		return nil
	}
	return []int{int(port)}
}
