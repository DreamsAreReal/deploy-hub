package netprobe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestHTTPURL(t *testing.T) {
	if got := HTTPURL("9001", "/health"); got != "http://127.0.0.1:9001/health" {
		t.Fatal(got)
	}
}

func TestHTTPOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/broken", http.StatusFound) })
	mux.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) { time.Sleep(200 * time.Millisecond) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := NewProber()
	ctx := context.Background()
	mux.HandleFunc("/bad", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) })
	mux.HandleFunc("/moved", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotModified) })
	cases := map[string]bool{"/ok": true, "/redirect": true, "/broken": false, "/missing": false, "/bad": false, "/moved": true}
	for path, want := range cases {
		if got := p.HTTPOK(ctx, srv.URL+path, time.Second); got != want {
			t.Errorf("%s: got %v want %v", path, got, want)
		}
	}
	if p.HTTPOK(ctx, srv.URL+"/slow", 20*time.Millisecond) {
		t.Error("timeout must fail")
	}
	if p.HTTPOK(ctx, "http://bad host/", time.Second) {
		t.Error("invalid url must fail")
	}
}

func TestListening(t *testing.T) {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("not a TCP listener")
	}
	port := addr.Port
	if !Listening(port) {
		t.Fatal("listener not detected")
	}
	_ = l.Close()
	if Listening(port) {
		t.Fatal("closed port reported as listening")
	}
}

func TestListeningPorts(t *testing.T) {
	dir := t.TempDir()
	tcp := "  sl  local_address rem_address   st\n" +
		"   0: 0100007F:2329 00000000:0000 0A 00000000\n" +
		"   1: 00000000:0016 00000000:0000 0A 00000000\n" +
		"   2: 0100007F:1F90 0100007F:9C40 01 00000000\n" +
		"   3: broken\n" +
		"   4: 0100007F:ZZZZ 00000000:0000 0A 00000000\n" +
		"   5: nocolon 00000000:0000 0A 00000000\n" +
		"   6: :0050 x 0A\n" +
		"   7: 0100007F:0051 x\n"
	tcp6 := "  sl  local_address\n   0: 00000000000000000000000000000000:2329 00000000000000000000000000000000:0000 0A\n"
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	got := ListeningPorts([]string{write("tcp", tcp), write("tcp6", tcp6), filepath.Join(dir, "missing")})
	if !reflect.DeepEqual(got, []int{22, 80, 9001}) {
		t.Fatalf("got %v", got)
	}
	if strconv.Itoa(got[2]) != "9001" {
		t.Fatal("hex decode")
	}
}
