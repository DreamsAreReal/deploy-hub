package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
)

const (
	ProtocolVersion = "2025-06-18"
	ServerName      = "deploy-hub"
	jsonRPCVersion  = "2.0"
	maxLine         = 1 << 20

	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602

	methodInitialize = "initialize"
	methodPing       = "ping"
	methodToolsList  = "tools/list"
	methodToolsCall  = "tools/call"

	instructions = "deploy-hub runs gated blue-green deploys on one VPS. Use status to see every app, " +
		"history to read an app's deploy journal, rollback to return an app to a version that deployed " +
		"successfully before. New code reaches the server only through a push to the app repo, never through this server."
)

type Reply struct {
	Stdout string
	Stderr string
}

type Remote func(ctx context.Context, request string) (Reply, error)

type Server struct {
	Remote  Remote
	Version string
	Log     *slog.Logger
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var nullID = json.RawMessage("null")

func (s Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxLine)
	enc := json.NewEncoder(out)
	for ctx.Err() == nil && sc.Scan() {
		resp, ok := s.handle(ctx, sc.Bytes())
		if !ok {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return errors.Join(sc.Err(), ctx.Err())
}

func (s Server) handle(ctx context.Context, line []byte) (response, bool) {
	if len(line) == 0 {
		return response{}, false
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return failure(nullID, codeParse, "parse error"), true
	}
	if len(req.ID) == 0 {
		return response{}, false
	}
	if req.JSONRPC != jsonRPCVersion || req.Method == "" {
		return failure(req.ID, codeInvalidRequest, "invalid request"), true
	}
	result, rerr := s.dispatch(ctx, req)
	if rerr != nil {
		return failure(req.ID, rerr.Code, rerr.Message), true
	}
	return response{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result}, true
}

func failure(id json.RawMessage, code int, msg string) response {
	return response{JSONRPC: jsonRPCVersion, ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (s Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case methodInitialize:
		return s.initialize(), nil
	case methodPing:
		return struct{}{}, nil
	case methodToolsList:
		return struct {
			Tools []tool `json:"tools"`
		}{toolList()}, nil
	case methodToolsCall:
		return s.call(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

func (s Server) initialize() map[string]any {
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]string{"name": ServerName, "version": s.Version},
		"instructions":    instructions,
	}
}
