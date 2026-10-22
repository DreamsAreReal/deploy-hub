package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	ToolStatus   = "status"
	ToolHistory  = "history"
	ToolRollback = "rollback"

	argApp = "app"
	argTag = "tag"

	typeString = "string"
)

type property struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type inputSchema struct {
	Type                 string              `json:"type"`
	Properties           map[string]property `json:"properties"`
	Required             []string            `json:"required"`
	AdditionalProperties bool                `json:"additionalProperties"`
}

type annotations struct {
	ReadOnly    bool `json:"readOnlyHint"`
	Destructive bool `json:"destructiveHint"`
	Idempotent  bool `json:"idempotentHint"`
	OpenWorld   bool `json:"openWorldHint"`
}

type tool struct {
	Name        string      `json:"name"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
	Annotations annotations `json:"annotations"`
	build       func(args map[string]string) (string, error)
}

var (
	appProperty = property{Type: typeString, Description: "app name as listed by status, e.g. myapp"}
	tagProperty = property{Type: typeString, Description: "version to start, sha-<7..40 hex>; omit for the previous version"}
	readOnly    = annotations{ReadOnly: true, Idempotent: true}
)

func schema(props map[string]property, required ...string) inputSchema {
	return inputSchema{Type: "object", Properties: props, Required: append([]string{}, required...)}
}

func toolList() []tool {
	return []tool{
		{
			Name: ToolStatus, Title: "Hub status",
			Description: "Every app on the hub: app | sha | health | last deploy.",
			InputSchema: schema(map[string]property{}), Annotations: readOnly,
			build: func(map[string]string) (string, error) { return ToolStatus, nil },
		},
		{
			Name: ToolHistory, Title: "Deploy history",
			Description: "Last 20 deploy journal lines of one app: deploys, gate verdicts, rollbacks, refusals.",
			InputSchema: schema(map[string]property{argApp: appProperty}, argApp), Annotations: readOnly,
			build: buildHistory,
		},
		{
			Name: ToolRollback, Title: "Roll back an app",
			Description: "Blue-green switch of an app to a version this hub deployed successfully before. " +
				"No tag: the previous version. Restarts the app; check status afterwards.",
			InputSchema: schema(map[string]property{argApp: appProperty, argTag: tagProperty}, argApp),
			Annotations: annotations{Destructive: true},
			build:       buildRollback,
		},
	}
}

func buildHistory(args map[string]string) (string, error) {
	app := args[argApp]
	if err := validate.AppName(app); err != nil {
		return "", err
	}
	return ToolHistory + " " + app, nil
}

func buildRollback(args map[string]string) (string, error) {
	app, tag := args[argApp], args[argTag]
	if err := validate.AppName(app); err != nil {
		return "", err
	}
	if tag == "" {
		return ToolRollback + " " + app, nil
	}
	if err := validate.Tag(tag); err != nil {
		return "", err
	}
	return ToolRollback + " " + app + " " + tag, nil
}

type callParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError"`
}

func findTool(name string) (tool, bool) {
	for _, t := range toolList() {
		if t.Name == name {
			return t, true
		}
	}
	return tool{}, false
}

func (s Server) call(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p callParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	t, ok := findTool(p.Name)
	if !ok {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
	}
	req, err := t.build(p.Arguments)
	if err != nil {
		return textResult(err.Error(), true), nil
	}
	s.Log.Info("tool call", "tool", t.Name, "request", req)
	reply, err := s.Remote(ctx, req)
	text := strings.TrimSpace(reply.Stdout + reply.Stderr)
	if err != nil {
		s.Log.Warn("tool call failed", "tool", t.Name, "error", err.Error())
		text = strings.TrimSpace(text + "\n" + err.Error())
	}
	return textResult(text, err != nil), nil
}

func textResult(text string, isError bool) callResult {
	return callResult{Content: []content{{Type: "text", Text: text}}, IsError: isError}
}
