package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

func call(t *testing.T, s *Server, msg string) map[string]any {
	t.Helper()
	raw := s.Handle(context.Background(), json.RawMessage(msg), "sess")
	if raw == nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestServer(t *testing.T) {
	s := New("host", "1", []agent.Tool{
		{Name: "ok", ReadOnly: true, Handler: func(ctx context.Context, c agent.ToolCallRequest) (agent.ToolResult, error) {
			if c.ID != "tu1" || c.SessionID != "sess" {
				t.Errorf("call = %+v", c)
			}
			return agent.ToolResult{Content: []agent.ToolContent{{Type: "text", Text: "fine"}}, Structured: json.RawMessage(`{"a":1}`)}, nil
		}},
		{Name: "fail", Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) {
			return agent.ToolResult{}, errors.New("broken")
		}},
	})
	init := call(t, s, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	if init["result"].(map[string]any)["protocolVersion"] != "2025-11-25" {
		t.Errorf("initialize = %v", init)
	}
	if call(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`) != nil {
		t.Error("notification answered")
	}
	list := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["annotations"] == nil {
		t.Errorf("tools/list = %v", list)
	}
	ok := call(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ok","arguments":{},"_meta":{"claudecode/toolUseId":"tu1"}}}`)
	res := ok["result"].(map[string]any)
	if res["structuredContent"] == nil || res["isError"] != nil {
		t.Errorf("ok result = %v", res)
	}
	fail := call(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fail"}}`)
	if fail["result"].(map[string]any)["isError"] != true {
		t.Errorf("fail result = %v", fail)
	}
	unknown := call(t, s, `{"jsonrpc":"2.0","id":4,"method":"nope"}`)
	if unknown["error"].(map[string]any)["code"] != float64(-32601) {
		t.Errorf("unknown = %v", unknown)
	}
}

func TestEncodeContent(t *testing.T) {
	got, _ := json.Marshal(EncodeContent(agent.ToolResult{Content: []agent.ToolContent{
		{Type: agent.ToolContentText, Text: "hi"},
		{Type: agent.ToolContentImage, Data: []byte("png"), MediaType: "image/png"},
	}}))
	want := `[{"text":"hi","type":"text"},{"data":"cG5n","mimeType":"image/png","type":"image"}]`
	if string(got) != want {
		t.Errorf("EncodeContent = %s; want %s", got, want)
	}
	if got := EncodeContent(agent.ToolResult{}); got == nil || len(got) != 0 {
		t.Errorf("empty result content = %#v; want an empty list", got)
	}
}
