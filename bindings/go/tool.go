package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Tool is a host-implemented tool the model can call. Pass tools to
// Client.Start with WithTools; the client sends their definitions in
// Config.Tools and runs Handler for every call.
type Tool struct {
	ToolDef
	// Handler runs one call on its own goroutine. Its context ends when the
	// client closes. An error becomes a result with IsError set.
	Handler func(ctx context.Context, call ToolCallRequest) (ToolResult, error)
}

// NewTool makes a tool whose handler receives the call's arguments decoded
// into T. schema is the JSON Schema of the arguments; nil accepts any
// object.
func NewTool[T any](name, description string, schema json.RawMessage, handler func(ctx context.Context, args T) (ToolResult, error)) Tool {
	return Tool{
		ToolDef: ToolDef{Name: name, Description: description, InputSchema: schema},
		Handler: func(ctx context.Context, call ToolCallRequest) (ToolResult, error) {
			var args T
			if len(call.Arguments) > 0 && string(call.Arguments) != "null" {
				if err := json.Unmarshal(call.Arguments, &args); err != nil {
					return ToolResult{}, fmt.Errorf("invalid arguments for %s: %w", name, err)
				}
			}
			return handler(ctx, args)
		},
	}
}

// TextResult is a successful tool result with text content.
func TextResult(text string) ToolResult {
	return ToolResult{Content: []ToolContent{{Type: ToolContentTypeText, Text: text}}}
}

// ErrorResult is a failed tool result with text content.
func ErrorResult(text string) ToolResult {
	return ToolResult{Content: []ToolContent{{Type: ToolContentTypeText, Text: text}}, IsError: true}
}

// ImageResult is a tool result with one image.
func ImageResult(mediaType string, data []byte) ToolResult {
	return ToolResult{Content: []ToolContent{{Type: ToolContentTypeImage, MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data)}}}
}

// JSONResult is a tool result with v as text and as structured content.
func JSONResult(v any) (ToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{Content: []ToolContent{{Type: ToolContentTypeText, Text: string(data)}}, Structured: data}, nil
}

// runTool runs a tool call and turns failures and panics into error results.
func runTool(ctx context.Context, tool *Tool, call ToolCallRequest) (res ToolResult) {
	if tool == nil || tool.Handler == nil {
		return ErrorResult(fmt.Sprintf("unknown tool %q", call.Name))
	}
	defer func() {
		if p := recover(); p != nil {
			res = ErrorResult(fmt.Sprintf("tool %s panicked: %v", call.Name, p))
		}
	}()
	res, err := tool.Handler(ctx, call)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return res
}
