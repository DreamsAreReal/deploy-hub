package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var errSim = errors.New("simulated")

const (
	argsWeb     = `{"app":"web"}`
	rollbackWeb = "rollback web"
	wantAppName = "app name"
	jsonNull    = "null"
	emptyObject = `{}`
)

type fakeRemote struct {
	requests []string
	reply    Reply
	err      error
}

func (f *fakeRemote) send(_ context.Context, request string) (Reply, error) {
	f.requests = append(f.requests, request)
	return f.reply, f.err
}

type decoded struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func serve(t *testing.T, remote *fakeRemote, lines ...string) []decoded {
	t.Helper()
	var out bytes.Buffer
	s := Server{Remote: remote.send, Version: "v1", Log: slog.New(slog.DiscardHandler)}
	testutil.NoErr(t, s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out))
	var got []decoded
	dec := json.NewDecoder(&out)
	for {
		var d decoded
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			return got
		} else if err != nil {
			t.Fatal(err)
		}
		got = append(got, d)
	}
}

func one(t *testing.T, remote *fakeRemote, line string) decoded {
	t.Helper()
	got := serve(t, remote, line)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, got[0].JSONRPC, jsonRPCVersion)
	return got[0]
}

func call(t *testing.T, remote *fakeRemote, name, args string) callResult {
	t.Helper()
	d := one(t, remote, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	if d.Error != nil {
		t.Fatalf("unexpected error %+v", d.Error)
	}
	var r callResult
	testutil.NoErr(t, json.Unmarshal(d.Result, &r))
	testutil.Equal(t, len(r.Content), 1)
	testutil.Equal(t, r.Content[0].Type, "text")
	return r
}

func TestInitialize(t *testing.T) {
	d := one(t, &fakeRemote{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`)
	testutil.Equal(t, string(d.ID), "1")
	testutil.True(t, d.Error == nil, "initialize must not fail")
	var r struct {
		ProtocolVersion string            `json:"protocolVersion"`
		Capabilities    map[string]any    `json:"capabilities"`
		ServerInfo      map[string]string `json:"serverInfo"`
		Instructions    string            `json:"instructions"`
	}
	testutil.NoErr(t, json.Unmarshal(d.Result, &r))
	testutil.Equal(t, r.ProtocolVersion, ProtocolVersion)
	testutil.Equal(t, r.ServerInfo["name"], ServerName)
	testutil.Equal(t, r.ServerInfo["version"], "v1")
	_, hasTools := r.Capabilities["tools"]
	testutil.True(t, hasTools, "tools capability advertised")
	testutil.Contains(t, r.Instructions, "status", "history", "rollback")
}

func TestPingAndStringID(t *testing.T) {
	d := one(t, &fakeRemote{}, `{"jsonrpc":"2.0","id":"abc","method":"ping"}`)
	testutil.Equal(t, string(d.ID), `"abc"`)
	testutil.Equal(t, string(d.Result), "{}")
}

func TestNotificationsAndBlankLinesGetNoReply(t *testing.T) {
	got := serve(t, &fakeRemote{}, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "", `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, string(got[0].ID), "2")
}

func TestProtocolErrors(t *testing.T) {
	cases := []struct {
		name, line, id string
		code           int
	}{
		{"parse", `{not json`, jsonNull, codeParse},
		{"version", `{"jsonrpc":"1.0","id":3,"method":"ping"}`, "3", codeInvalidRequest},
		{"no method", `{"jsonrpc":"2.0","id":3}`, "3", codeInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`, "3", codeMethodNotFound},
		{"bad params", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"history","arguments":{"app":1}}}`, "3", codeInvalidParams},
		{"unknown tool", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"deploy","arguments":{}}}`, "3", codeInvalidParams},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remote := &fakeRemote{}
			d := one(t, remote, c.line)
			testutil.Equal(t, string(d.ID), c.id)
			if d.Error == nil {
				t.Fatal("want error")
			}
			testutil.Equal(t, d.Error.Code, c.code)
			testutil.Equal(t, len(d.Result), 0)
			testutil.Equal(t, len(remote.requests), 0)
		})
	}
}

func TestToolsList(t *testing.T) {
	d := one(t, &fakeRemote{}, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	var r struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	testutil.NoErr(t, json.Unmarshal(d.Result, &r))
	names := make([]string, 0, len(r.Tools))
	for _, tl := range r.Tools {
		names = append(names, tl.Name)
		testutil.Equal(t, tl.InputSchema["type"], "object")
		testutil.True(t, tl.Description != "", tl.Name+" has a description")
		destructive := tl.Annotations["destructiveHint"] == true
		testutil.Equal(t, destructive, tl.Name == ToolRollback)
		testutil.Equal(t, tl.Annotations["readOnlyHint"], tl.Name != ToolRollback)
	}
	testutil.Equal(t, strings.Join(names, ","), "status,history,rollback")
	testutil.Excludes(t, string(d.Result), "deploy <", `"report"`)
}

func TestToolRequests(t *testing.T) {
	cases := []struct {
		tool, args, want string
	}{
		{ToolStatus, emptyObject, ToolStatus},
		{ToolStatus, jsonNull, ToolStatus},
		{ToolHistory, argsWeb, "history web"},
		{ToolRollback, argsWeb, rollbackWeb},
		{ToolRollback, `{"app":"web","tag":""}`, rollbackWeb},
		{ToolRollback, `{"app":"web","tag":"sha-abc1234"}`, "rollback web sha-abc1234"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			remote := &fakeRemote{reply: Reply{Stdout: "out\n", Stderr: "note\n"}}
			r := call(t, remote, c.tool, c.args)
			testutil.Equal(t, strings.Join(remote.requests, "|"), c.want)
			testutil.Equal(t, r.IsError, false)
			testutil.Equal(t, r.Content[0].Text, "out\nnote")
		})
	}
}

func TestToolRejectsBadArguments(t *testing.T) {
	cases := []struct{ tool, args, want string }{
		{ToolHistory, emptyObject, wantAppName},
		{ToolHistory, `{"app":"web; reboot"}`, wantAppName},
		{ToolRollback, `{"app":"../etc"}`, wantAppName},
		{ToolRollback, `{"app":"web","tag":"latest"}`, argTag},
		{ToolRollback, `{"app":"web","tag":"sha-abc1234 extra"}`, argTag},
	}
	for _, c := range cases {
		t.Run(c.tool+c.args, func(t *testing.T) {
			remote := &fakeRemote{}
			r := call(t, remote, c.tool, c.args)
			testutil.Equal(t, r.IsError, true)
			testutil.Contains(t, r.Content[0].Text, c.want)
			testutil.Equal(t, len(remote.requests), 0)
		})
	}
}

func TestToolReportsRemoteFailure(t *testing.T) {
	remote := &fakeRemote{reply: Reply{Stderr: "refused: unknown app\n"}, err: errSim}
	r := call(t, remote, ToolHistory, argsWeb)
	testutil.Equal(t, r.IsError, true)
	testutil.Equal(t, r.Content[0].Text, "refused: unknown app\nsimulated")
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errSim }

func TestServeStopsOnWriteError(t *testing.T) {
	s := Server{Remote: (&fakeRemote{}).send, Log: slog.New(slog.DiscardHandler)}
	err := s.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), brokenWriter{})
	testutil.Is(t, err, errSim)
}

func TestServeStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	s := Server{Remote: (&fakeRemote{}).send, Log: slog.New(slog.DiscardHandler)}
	err := s.Serve(ctx, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), &out)
	testutil.Is(t, err, context.Canceled)
	testutil.Equal(t, out.Len(), 0)
}

func TestServeRejectsOversizedLine(t *testing.T) {
	s := Server{Remote: (&fakeRemote{}).send, Log: slog.New(slog.DiscardHandler)}
	err := s.Serve(context.Background(), strings.NewReader(strings.Repeat("x", maxLine+1)), io.Discard)
	testutil.Fails(t, err)
}
