package pi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func TestStructuredOutputEmulation(t *testing.T) {
	type run struct {
		schema  string
		details any // arguments of the structured_output call; nil for none
		want    string
	}
	runs := []run{
		{`{"type":"object","properties":{"answer":{"type":"string"}}}`, map[string]any{"answer": "42"}, `{"answer":"42"}`},
		{`{"type":"array","items":{"type":"integer"}}`, map[string]any{"value": []any{1, 2}}, `[1,2]`},
		{`{"type":"object"}`, nil, ""},
	}
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		var token string
		for _, kv := range h.Cmd.Env {
			if v, ok := strings.CutPrefix(kv, "AGENT_CLI_BRIDGE="); ok {
				var cfg struct{ Token string }
				_ = json.Unmarshal([]byte(v), &cfg)
				token = cfg.Token
			}
		}
		for i, r := range runs {
			prompt := h.RecvWhere(isCommand("prompt"))
			if prompt == nil {
				return
			}
			text, marker, ok := strings.Cut(fakeproc.Str(prompt, "message"), "\n"+token+":output:")
			var payload struct{ Schema json.RawMessage }
			raw, _ := base64.RawURLEncoding.DecodeString(marker)
			if !ok || text != "answer" || json.Unmarshal(raw, &payload) != nil {
				t.Errorf("run %d prompt = %q", i, fakeproc.Str(prompt, "message"))
			}
			if wrapped := !strings.HasPrefix(r.schema, `{"type":"object"`); wrapped != strings.Contains(string(payload.Schema), `"value"`) {
				t.Errorf("run %d tool parameters = %s", i, payload.Schema)
			}
			respond(h, prompt, map[string]any{"disposition": "started"})
			h.Send(map[string]any{"type": "agent_start"})
			if r.details != nil {
				h.Send(map[string]any{"type": "tool_execution_start", "toolCallId": "out", "toolName": "structured_output", "args": r.details})
				h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "out", "toolName": "structured_output",
					"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Structured output recorded."}}, "details": r.details}})
			}
			h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{}}})
			h.Send(map[string]any{"type": "agent_end", "messages": []any{}})
			h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	<-started
	if !sess.Capabilities().OutputSchema {
		t.Error("OutputSchema capability is off with the bridge")
	}
	if _, err := sess.Send(ctx, agent.Input{Parts: []agent.Part{{Type: agent.PartText, Text: "answer"}}, OutputSchema: json.RawMessage(`[1]`)}); err == nil {
		t.Error("a schema that is not an object was accepted")
	}
	for i, r := range runs {
		res, err := sess.Run(ctx, agent.Input{Parts: []agent.Part{{Type: agent.PartText, Text: "answer"}}, OutputSchema: json.RawMessage(r.schema)})
		if res == nil {
			t.Fatal(err)
		}
		if r.want == "" {
			var te *agent.TurnError
			if res.Status != agent.TurnFailed || !errors.As(res.Err, &te) || te.Code != "no_structured_output" {
				t.Errorf("run %d without output = %+v", i, res)
			}
			continue
		}
		if res.Status != agent.TurnSucceeded || string(res.StructuredOutput) != r.want {
			t.Errorf("run %d = %s %s, want %s", i, res.Status, res.StructuredOutput, r.want)
		}
	}
}

func TestStructuredOutputNeedsBridge(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startup(h) {
			h.WaitClosed()
		}
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner}).StartSession(ctx, agent.Config{ProviderOptions: &pi.SessionOptions{DisableBridge: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{{Type: agent.PartText, Text: "x"}}, OutputSchema: json.RawMessage(`{"type":"object"}`)})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("send without the bridge = %v", err)
	}
}

func TestConfigOutputSchema(t *testing.T) {
	type run struct {
		input   string // Input.OutputSchema; empty uses Config.OutputSchema
		details any
		want    string
	}
	runs := []run{
		{"", map[string]any{"answer": "42"}, `{"answer":"42"}`},
		{`{"type":"array","items":{"type":"integer"}}`, map[string]any{"value": []any{1, 2}}, `[1,2]`},
	}
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		var token string
		for _, kv := range h.Cmd.Env {
			if v, ok := strings.CutPrefix(kv, "AGENT_CLI_BRIDGE="); ok {
				var cfg struct{ Token string }
				_ = json.Unmarshal([]byte(v), &cfg)
				token = cfg.Token
			}
		}
		for i, r := range runs {
			prompt := h.RecvWhere(isCommand("prompt"))
			if prompt == nil {
				return
			}
			_, marker, ok := strings.Cut(fakeproc.Str(prompt, "message"), "\n"+token+":output:")
			var payload struct{ Schema json.RawMessage }
			raw, _ := base64.RawURLEncoding.DecodeString(marker)
			if !ok || json.Unmarshal(raw, &payload) != nil {
				t.Errorf("run %d prompt = %q", i, fakeproc.Str(prompt, "message"))
			}
			if wrapped := r.input != ""; wrapped != strings.Contains(string(payload.Schema), `"value"`) {
				t.Errorf("run %d tool parameters = %s", i, payload.Schema)
			}
			respond(h, prompt, map[string]any{"disposition": "started"})
			h.Send(map[string]any{"type": "agent_start"})
			h.Send(map[string]any{"type": "tool_execution_start", "toolCallId": "out", "toolName": "structured_output", "args": r.details})
			h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "out", "toolName": "structured_output",
				"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Structured output recorded."}}, "details": r.details}})
			h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{}}})
			h.Send(map[string]any{"type": "agent_end", "messages": []any{}})
			h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, agent.Config{OutputSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	<-started
	for i, r := range runs {
		res, err := sess.Run(ctx, agent.Input{Parts: []agent.Part{{Type: agent.PartText, Text: "answer"}}, OutputSchema: json.RawMessage(r.input)})
		if res == nil {
			t.Fatal(err)
		}
		if res.Status != agent.TurnSucceeded || string(res.StructuredOutput) != r.want {
			t.Errorf("run %d = %s %s, want %s", i, res.Status, res.StructuredOutput, r.want)
		}
	}
}

func TestConfigOutputSchemaIgnored(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	for name, opts := range map[string]*pi.SessionOptions{
		"no bridge": {DisableBridge: true},
		"no tools":  {NoTools: true},
	} {
		t.Run(name, func(t *testing.T) {
			spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
				if startup(h) {
					h.WaitClosed()
				}
			})
			p := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"})
			strict, err := p.StartSession(context.Background(), agent.Config{OutputSchema: schema, Strict: true, ProviderOptions: opts})
			if err == nil {
				strict.Close()
			}
			if !errors.Is(err, agent.ErrUnsupported) || !strings.Contains(err.Error(), "OutputSchema") {
				t.Errorf("strict start = %v", err)
			}
			sess, err := p.StartSession(context.Background(), agent.Config{OutputSchema: schema, ProviderOptions: opts})
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			first, second := <-sess.Events(), <-sess.Events()
			n, ok := second.(*agent.Notice)
			if _, started := first.(*agent.SessionStarted); !started || !ok || n.Code != "config_ignored" || !strings.Contains(n.Message, "OutputSchema") {
				t.Errorf("first events %T, %+v", first, second)
			}
		})
	}
}

func TestConfigOutputSchemaInvalid(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startup(h) {
			h.WaitClosed()
		}
	})
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(context.Background(), agent.Config{OutputSchema: json.RawMessage(`[1]`)})
	if err == nil {
		sess.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "output schema") {
		t.Errorf("start with an invalid schema = %v", err)
	}
}
