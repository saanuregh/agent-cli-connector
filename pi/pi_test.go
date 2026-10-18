package pi_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func isCommand(typ string) func(map[string]any) bool {
	return func(m map[string]any) bool { return fakeproc.Str(m, "type") == typ }
}

func respond(h *fakeproc.Harness, cmd map[string]any, data any) {
	h.Send(map[string]any{"id": cmd["id"], "type": "response", "command": cmd["type"], "success": true, "data": data})
}

// bridgeToken reads the bridge token from the spawned environment.
func bridgeToken(t *testing.T, h *fakeproc.Harness) string {
	for _, kv := range h.Cmd.Env {
		if v, ok := strings.CutPrefix(kv, "AGENT_CLI_BRIDGE="); ok {
			var cfg struct {
				Token string `json:"token"`
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			if err := json.Unmarshal([]byte(v), &cfg); err != nil {
				t.Fatalf("bridge config: %v", err)
			}
			if len(cfg.Tools) != 1 || cfg.Tools[0].Name != "add" {
				t.Errorf("bridge tools = %+v", cfg.Tools)
			}
			return cfg.Token
		}
	}
	t.Fatal("no bridge config in environment")
	return ""
}

// bridge sends a bridge request and returns the decoded answer.
func bridge(h *fakeproc.Harness, token, id string, req map[string]any) map[string]any {
	payload, _ := json.Marshal(req)
	h.Send(map[string]any{"type": "extension_ui_request", "id": id, "method": "input", "title": token, "placeholder": string(payload)})
	resp := h.RecvWhere(func(m map[string]any) bool {
		return fakeproc.Str(m, "type") == "extension_ui_response" && fakeproc.Str(m, "id") == id
	})
	var out map[string]any
	_ = json.Unmarshal([]byte(fakeproc.Str(resp, "value")), &out)
	return out
}

func startup(h *fakeproc.Harness) bool {
	st := h.RecvWhere(isCommand("get_state"))
	if st == nil {
		return false
	}
	respond(h, st, map[string]any{"sessionId": "pi-1", "thinkingLevel": "low", "sessionName": "named",
		"model": map[string]any{"id": "flash", "provider": "openrouter", "contextWindow": 9000}})
	cmds := h.RecvWhere(isCommand("get_commands"))
	respond(h, cmds, map[string]any{"commands": []any{map[string]any{"name": "fix", "description": "Fix tests", "source": "prompt"}}})
	return true
}

func update(h *fakeproc.Harness, ev map[string]any) {
	h.Send(map[string]any{"type": "message_update", "usage": map[string]any{}, "assistantMessageEvent": ev})
}

func TestTurnBridgeAndQueue(t *testing.T) {
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		token := bridgeToken(t, h)
		prompt := h.RecvWhere(isCommand("prompt"))
		if fakeproc.Str(prompt, "message") != "first" {
			t.Errorf("prompt = %v", prompt)
		}
		respond(h, prompt, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		h.Send(map[string]any{"type": "turn_start"})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "user", "content": "first", "timestamp": 1}})
		h.Send(map[string]any{"type": "message_start", "message": map[string]any{"role": "assistant", "content": []any{}}})
		update(h, map[string]any{"type": "text_start", "contentIndex": 0})
		update(h, map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "Work"})
		update(h, map[string]any{"type": "toolcall_start", "contentIndex": 1, "id": "call1", "toolName": "bash"})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "toolUse",
			"content": []any{map[string]any{"type": "text", "text": "Working"}, map[string]any{"type": "toolCall", "id": "call1", "name": "bash", "arguments": map[string]any{"command": "rm -rf x"}}},
			"usage":   map[string]any{"input": 100, "output": 10, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 110, "cost": map[string]any{"total": 0.01}}}})
		h.Send(map[string]any{"type": "tool_execution_start", "toolCallId": "call1", "toolName": "bash", "args": map[string]any{"command": "rm -rf x"}})
		gate := bridge(h, token, "g1", map[string]any{"kind": "permission", "toolName": "bash", "toolCallId": "call1", "input": map[string]any{"command": "rm -rf x"}})
		if gate["allow"] != false || gate["reason"] != "no deleting" {
			t.Errorf("gate answer = %v", gate)
		}
		h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "call1", "toolName": "bash", "isError": true, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "no deleting"}}}})
		readGate := bridge(h, token, "g2", map[string]any{"kind": "permission", "toolName": "read", "toolCallId": "call2", "input": map[string]any{"path": "a"}})
		if readGate["allow"] != true {
			t.Errorf("read gate = %v", readGate)
		}
		tool := bridge(h, token, "t1", map[string]any{"kind": "tool", "name": "add", "id": "call3", "arguments": map[string]any{"a": 2, "b": 3}})
		if fakeproc.Str(tool, "content", 0, "text") != "5" {
			t.Errorf("tool answer = %v", tool)
		}
		h.Send(map[string]any{"type": "extension_ui_request", "id": "sel", "method": "select", "title": "Pick one", "options": []any{"red", "blue"}})
		sel := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "id") == "sel" })
		if fakeproc.Str(sel, "value") != "blue" {
			t.Errorf("select answer = %v", sel)
		}
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop",
			"content": []any{map[string]any{"type": "text", "text": "All done"}},
			"usage":   map[string]any{"input": 120, "output": 5, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 125, "cost": map[string]any{"total": 0.02}}}})
		h.Send(map[string]any{"type": "agent_end", "messages": []any{}})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})

		next := h.RecvWhere(isCommand("prompt"))
		if fakeproc.Str(next, "message") != "second" {
			t.Errorf("queued prompt = %v", next)
		}
		respond(h, next, map[string]any{"disposition": "handled"})
		h.WaitClosed()
	})
	ctx := context.Background()
	cfg := agent.Config{
		PermissionMode: agent.PermissionDefault,
		Tools: []agent.Tool{{Name: "add", Handler: func(ctx context.Context, call agent.ToolCallRequest) (agent.ToolResult, error) {
			return agent.TextResult("5"), nil
		}}},
		Handlers: agent.Handlers{
			Permission: func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
				if req.Command != "rm -rf x" || req.Kind != agent.PermissionCommand {
					t.Errorf("permission request = %+v", req)
				}
				return agent.PermissionResponse{Decision: agent.DecisionDeny, Message: "no deleting"}
			},
			UserInput: func(ctx context.Context, req *agent.UserInputRequest) agent.UserInputResponse {
				return agent.UserInputResponse{Answers: map[string][]string{req.Questions[0].ID: {req.Questions[0].Options[1].Label}}}
			},
		},
	}
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	h := <-started
	if !slices.Contains(h.Cmd.Args, "/bridge.ts") || !slices.Contains(h.Cmd.Args, "rpc") {
		t.Errorf("args = %v", h.Cmd.Args)
	}
	info := sess.Info()
	if sess.ID() != "pi-1" || info.Model != "openrouter/flash" || info.Title != "named" || len(info.Commands) != 3 || info.Commands[0].Name != "fix" {
		t.Errorf("info = %+v", info)
	}
	first, err := sess.Send(ctx, agent.Text("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := sess.Send(ctx, agent.Text("second"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := first.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnSucceeded || res.Text != "All done" || res.Usage.InputTokens != 220 || res.Usage.CostUSD != 0.03 || res.Usage.ContextWindow != 9000 {
		t.Errorf("first result = %+v", res)
	}
	declined := false
	for _, it := range res.Items {
		if it.Tool != nil && it.Tool.Command == "rm -rf x" && it.Status == agent.ItemDeclined {
			declined = true
		}
	}
	if !declined {
		t.Errorf("no declined bash item in %+v", res.Items)
	}
	res, err = second.Wait(ctx)
	if err != nil || res.Status != agent.TurnSucceeded || res.StopReason != "handled" {
		t.Errorf("second result = %+v, %v", res, err)
	}
}

func TestSteerJoinsRunOrStartsTurn(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		first := h.RecvWhere(isCommand("prompt"))
		if fakeproc.Str(first, "streamingBehavior") != "steer" {
			t.Errorf("prompt without streamingBehavior: %v", first)
		}
		respond(h, first, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})

		joined := h.RecvWhere(isCommand("prompt"))
		respond(h, joined, map[string]any{"disposition": "queued"})

		// Pi settles before it reads the second steer, so the steer starts a new run.
		late := h.RecvWhere(isCommand("prompt"))
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": "one"}}}})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		respond(h, late, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": "two"}}}})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	first, err := sess.Send(ctx, agent.Text("go"))
	if err != nil {
		t.Fatal(err)
	}
	steer := agent.Text("also")
	steer.Delivery = agent.DeliverSteer
	joined, err := sess.Send(ctx, steer)
	if err != nil {
		t.Fatal(err)
	}
	if joined.ID() != first.ID() {
		t.Errorf("queued steer turn %q, want %q", joined.ID(), first.ID())
	}
	late, err := sess.Send(ctx, steer)
	if err != nil {
		t.Fatal(err)
	}
	if late.ID() == first.ID() {
		t.Fatal("steer that started a run reused the finished turn")
	}
	res, err := late.Wait(ctx)
	if err != nil || res.Text != "two" {
		t.Errorf("late steer result %+v, %v", res, err)
	}
	if res, _ := first.Wait(ctx); res.Text != "one" {
		t.Errorf("first result %+v", res)
	}
}

func TestRewindAndForkAtTurn(t *testing.T) {
	entries := []any{
		map[string]any{"type": "message", "id": "u1", "message": map[string]any{"role": "user", "timestamp": 100}},
		map[string]any{"type": "message", "id": "a1", "message": map[string]any{"role": "assistant", "timestamp": 101}},
		map[string]any{"type": "message", "id": "u2", "message": map[string]any{"role": "user", "timestamp": 200}},
	}
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		for _, stamp := range []int{100, 200} {
			prompt := h.RecvWhere(isCommand("prompt"))
			respond(h, prompt, map[string]any{"disposition": "started"})
			h.Send(map[string]any{"type": "agent_start"})
			h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "user", "content": "hi", "timestamp": stamp}})
			h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		}
		get := h.RecvWhere(isCommand("get_entries"))
		respond(h, get, map[string]any{"entries": entries, "leafId": "u2"})
		fork := h.RecvWhere(isCommand("fork"))
		if fakeproc.Str(fork, "entryId") != "u2" {
			t.Errorf("rewind forked at %v", fork)
		}
		respond(h, fork, map[string]any{"text": "hi", "cancelled": false})
		st := h.RecvWhere(isCommand("get_state"))
		respond(h, st, map[string]any{"sessionId": "pi-2"})
		if cmds := h.RecvWhere(isCommand("get_commands")); cmds != nil {
			respond(h, cmds, map[string]any{"commands": []any{}})
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Run(ctx, agent.Text("one")); err != nil {
		t.Fatal(err)
	}
	second, err := sess.Run(ctx, agent.Text("two"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Rewind(ctx, second.TurnID); err != nil {
		t.Fatal(err)
	}
	if sess.ID() != "pi-2" {
		t.Errorf("session ID after rewind = %q", sess.ID())
	}

	forkSpawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		first := h.RecvWhere(isCommand("get_state"))
		if first == nil {
			return
		}
		respond(h, first, map[string]any{"sessionId": "pi-copy"})
		cmds := h.RecvWhere(isCommand("get_commands"))
		respond(h, cmds, map[string]any{"commands": []any{}})
		msgs := h.RecvWhere(isCommand("get_fork_messages"))
		respond(h, msgs, map[string]any{"messages": []any{map[string]any{"entryId": "u1"}, map[string]any{"entryId": "u2"}}})
		fork := h.RecvWhere(isCommand("fork"))
		if fakeproc.Str(fork, "entryId") != "u2" {
			t.Errorf("ForkAtTurn forked at %v", fork)
		}
		respond(h, fork, map[string]any{"cancelled": false})
		st := h.RecvWhere(isCommand("get_state"))
		respond(h, st, map[string]any{"sessionId": "pi-3"})
		if cmds := h.RecvWhere(isCommand("get_commands")); cmds != nil {
			respond(h, cmds, map[string]any{"commands": []any{}})
		}
		h.WaitClosed()
	})
	forked, err := pi.New(pi.Options{Spawner: forkSpawner, DisableBridge: true}).StartSession(ctx, agent.Config{Resume: "pi-1", Fork: true, ForkAtTurn: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	defer forked.Close()
	if forked.ID() != "pi-3" {
		t.Errorf("forked session ID = %q", forked.ID())
	}
}

func TestTurnOptionsRefused(t *testing.T) {
	release := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		<-release
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	defer close(release)
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("x")}, ProviderOptions: "options"})
	if err == nil || !strings.Contains(err.Error(), "pi has no turn options") {
		t.Errorf("Send = %v", err)
	}
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("x")}, Delivery: "Steer"})
	if err == nil || !strings.Contains(err.Error(), `unknown Input.Delivery "Steer"`) {
		t.Errorf("Send with bad delivery = %v", err)
	}
}

func TestSessionCapabilitiesWithoutBridge(t *testing.T) {
	release := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		<-release
	})
	ctx := context.Background()
	p := pi.New(pi.Options{Spawner: spawner})
	sess, err := p.StartSession(ctx, agent.Config{ProviderOptions: &pi.SessionOptions{DisableBridge: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	defer close(release)
	if !p.Capabilities().HostTools {
		t.Fatal("provider without DisableBridge reports no host tools")
	}
	if c := sess.Capabilities(); c.HostTools || c.PermissionPrompt || c.MCPServers || c.OutputSchema || c.PermissionModes != nil {
		t.Errorf("session without bridge = %+v", c)
	}
}
