package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Tool is a host-implemented tool the model can call. The library serves
// it through each harness's native extension point: an in-process MCP
// server for Claude, dynamic tools for Codex, a bridge extension for Pi,
// and an in-process HTTP MCP server for OpenCode. The handler runs in this
// process.
type Tool struct {
	// Name is the tool name the model sees. Claude prefixes it as
	// "mcp__<server>__<name>"; see the claude package.
	Name        string
	Description string
	// InputSchema is a JSON Schema object for the arguments. Nil means an
	// object with any properties.
	InputSchema json.RawMessage
	// Handler runs the tool. A returned error becomes an error result the
	// model sees.
	Handler func(ctx context.Context, call ToolCallRequest) (ToolResult, error)
	// ReadOnly hints that the tool changes nothing.
	ReadOnly bool
	// NeedsApproval routes calls through the permission flow. By default
	// host tools run without asking, because the host defined them.
	NeedsApproval bool
	// Title is a display name; Destructive and OpenWorld hint that the
	// tool deletes data or reaches outside the host. Harnesses that
	// support MCP tool annotations pass them on.
	Title       string
	Destructive bool
	OpenWorld   bool
	// AlwaysLoad keeps the tool in context where the harness would defer
	// it behind tool search; SearchHint helps tool search find it.
	AlwaysLoad bool
	SearchHint string
}

// NewTool returns a tool whose handler receives the call's arguments
// decoded into T. Arguments that do not decode return an error, which the
// model sees as an error result. schema is the JSON Schema of the
// arguments; nil means an object with any properties. Set the other Tool
// fields on the returned value.
//
//	type weatherArgs struct {
//		City string `json:"city"`
//	}
//	tool := agent.NewTool("weather", "Current weather for a city.",
//		json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
//		func(ctx context.Context, args weatherArgs) (agent.ToolResult, error) {
//			return agent.TextResult("Sunny in " + args.City), nil
//		})
func NewTool[T any](name, description string, schema json.RawMessage, handler func(ctx context.Context, args T) (ToolResult, error)) Tool {
	return Tool{
		Name:        name,
		Description: description,
		InputSchema: schema,
		Handler: func(ctx context.Context, call ToolCallRequest) (ToolResult, error) {
			var args T
			if len(call.Arguments) > 0 {
				if err := json.Unmarshal(call.Arguments, &args); err != nil {
					return ToolResult{}, fmt.Errorf("invalid arguments for %s: %w", name, err)
				}
			}
			return handler(ctx, args)
		},
	}
}

// ToolCallRequest is one call of a host tool.
type ToolCallRequest struct {
	// ID is the harness's tool call ID.
	ID   string
	Name string
	// Arguments is the JSON arguments object.
	Arguments json.RawMessage
	// SessionID is the session making the call.
	SessionID string
}

// ToolResult is the outcome of a host tool call.
type ToolResult struct {
	Content []ToolContent
	// IsError tells the model the call failed.
	IsError bool
	// Structured is an optional machine-readable result.
	Structured json.RawMessage
}

// ToolContent is one piece of a tool result: Text for a text piece, Data
// and MediaType for an image.
type ToolContent struct {
	Type      ToolContentType
	Text      string
	Data      []byte
	MediaType string
}

// ToolContentType names a kind of tool result content.
type ToolContentType string

// Tool result content types.
const (
	ToolContentText  ToolContentType = "text"
	ToolContentImage ToolContentType = "image"
)

// TextResult returns a successful text result.
func TextResult(text string) ToolResult {
	return ToolResult{Content: []ToolContent{{Type: ToolContentText, Text: text}}}
}

// ErrorResult returns a failed text result.
func ErrorResult(text string) ToolResult {
	return ToolResult{Content: []ToolContent{{Type: ToolContentText, Text: text}}, IsError: true}
}

// ImageResult returns a successful result holding one image.
func ImageResult(mediaType string, data []byte) ToolResult {
	return ToolResult{Content: []ToolContent{{Type: ToolContentImage, Data: data, MediaType: mediaType}}}
}

// JSONResult returns a successful result holding v as JSON text and as
// structured content.
func JSONResult(v any) (ToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: []ToolContent{{Type: ToolContentText, Text: string(data)}}, Structured: data}, nil
}

// Text joins the text content of the result.
func (r ToolResult) Text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type != ToolContentText {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(c.Text)
	}
	return b.String()
}

// MCPTransport names how an MCP server is reached.
type MCPTransport string

// MCP transports.
const (
	MCPStdio MCPTransport = "stdio"
	MCPHTTP  MCPTransport = "http"
	MCPSSE   MCPTransport = "sse"
)

// MCPServer configures an MCP server for a session.
type MCPServer struct {
	// Transport defaults to stdio when Command is set and to http when URL
	// is set.
	Transport MCPTransport
	// Command, Args, Env, and Cwd start a stdio server.
	Command string
	Args    []string
	Env     map[string]string
	Cwd     string
	// URL and Headers reach an http or sse server.
	URL     string
	Headers map[string]string
	// Enabled is nil for the harness default.
	Enabled *bool
	// ToolTimeout bounds one tool call and StartupTimeout bounds the
	// server start. Zero means the harness default.
	ToolTimeout    time.Duration
	StartupTimeout time.Duration
	// AlwaysLoad keeps the server's tools in context instead of deferring
	// them behind tool search, where the harness defers tools.
	AlwaysLoad bool
	// EnabledTools limits the server's tools to these names; empty means
	// all. ApprovedTools run without asking.
	EnabledTools  []string
	ApprovedTools []string
}

// TransportOrDefault returns Transport, inferring it when empty.
func (s MCPServer) TransportOrDefault() MCPTransport {
	if s.Transport != "" {
		return s.Transport
	}
	if s.URL != "" {
		return MCPHTTP
	}
	return MCPStdio
}

// StdioServer returns a stdio MCP server configuration.
func StdioServer(command string, args ...string) MCPServer {
	return MCPServer{Transport: MCPStdio, Command: command, Args: args}
}

// HTTPServer returns a streamable HTTP MCP server configuration.
func HTTPServer(url string) MCPServer {
	return MCPServer{Transport: MCPHTTP, URL: url}
}
