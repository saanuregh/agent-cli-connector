// Package mcpserver answers MCP JSON-RPC messages for a set of host tools.
// It is transport-free: the caller passes each message in and writes each
// answer out, which suits harnesses that tunnel MCP through their own
// protocol.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// DefaultProtocolVersion is offered when the client names none.
const DefaultProtocolVersion = "2025-06-18"

// Server serves tools over MCP.
type Server struct {
	name     string
	version  string
	byName   map[string]agent.Tool
	toolList map[string]any
}

// New returns a server named name serving tools.
func New(name, version string, tools []agent.Tool) *Server {
	s := &Server{name: name, version: version, byName: make(map[string]agent.Tool, len(tools))}
	for _, t := range tools {
		s.byName[t.Name] = t
	}
	s.toolList = listTools(tools)
	return s
}

func listTools(tools []agent.Tool) map[string]any {
	list := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		entry := map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": providerutil.ObjectSchema(t.InputSchema),
		}
		annotations := map[string]any{}
		if t.ReadOnly {
			annotations["readOnlyHint"] = true
		}
		if t.Destructive {
			annotations["destructiveHint"] = true
		}
		if t.OpenWorld {
			annotations["openWorldHint"] = true
		}
		if t.Title != "" {
			annotations["title"] = t.Title
			entry["title"] = t.Title
		}
		if len(annotations) > 0 {
			entry["annotations"] = annotations
		}
		meta := map[string]any{}
		if t.AlwaysLoad {
			meta["anthropic/alwaysLoad"] = true
		}
		if t.SearchHint != "" {
			meta["anthropic/searchHint"] = t.SearchHint
		}
		if len(meta) > 0 {
			entry["_meta"] = meta
		}
		list = append(list, entry)
	}
	return map[string]any{"tools": list}
}

// Name returns the server name.
func (s *Server) Name() string { return s.name }

type message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
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

// Handle answers one message. It returns nil for notifications. sessionID
// is passed to tool handlers.
func (s *Server) Handle(ctx context.Context, raw json.RawMessage, sessionID string) json.RawMessage {
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
	}
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		return nil
	}
	result, rerr := s.dispatch(ctx, msg, sessionID)
	return encode(response{JSONRPC: "2.0", ID: msg.ID, Result: result, Error: rerr})
}

func (s *Server) dispatch(ctx context.Context, msg message, sessionID string) (any, *rpcError) {
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		version := p.ProtocolVersion
		if version == "" {
			version = DefaultProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.toolList, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      map[string]any  `json:"_meta"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		tool, ok := s.byName[p.Name]
		if !ok {
			return nil, &rpcError{Code: -32602, Message: "unknown tool " + p.Name}
		}
		callID, _ := p.Meta["claudecode/toolUseId"].(string)
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage("{}")
		}
		res := providerkit.CallTool(ctx, tool, agent.ToolCallRequest{ID: callID, Name: p.Name, Arguments: p.Arguments, SessionID: sessionID})
		return encodeResult(res), nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + msg.Method}
	}
}

// EncodeContent converts a tool result's content to MCP content blocks:
// images as base64 image blocks, everything else as text blocks.
func EncodeContent(res agent.ToolResult) []map[string]any {
	content := make([]map[string]any, 0, len(res.Content))
	for _, c := range res.Content {
		switch c.Type {
		case agent.ToolContentImage:
			content = append(content, map[string]any{
				"type":     "image",
				"data":     base64.StdEncoding.EncodeToString(c.Data),
				"mimeType": c.MediaType,
			})
		default:
			content = append(content, map[string]any{"type": "text", "text": c.Text})
		}
	}
	return content
}

// encodeResult converts a tool result to an MCP CallToolResult.
func encodeResult(res agent.ToolResult) map[string]any {
	out := map[string]any{"content": EncodeContent(res)}
	if res.IsError {
		out["isError"] = true
	}
	// MCP requires structuredContent to be an object.
	if trimmed := bytes.TrimSpace(res.Structured); len(trimmed) > 0 && trimmed[0] == '{' {
		out["structuredContent"] = json.RawMessage(trimmed)
	}
	return out
}

func encode(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`)
	}
	return data
}
