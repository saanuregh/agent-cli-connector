package claude_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

func isControl(subtype string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		return fakeproc.Str(m, "type") == "control_request" && fakeproc.Str(m, "request", "subtype") == subtype
	}
}

func isType(typ string) func(map[string]any) bool {
	return func(m map[string]any) bool { return fakeproc.Str(m, "type") == typ }
}

func controlResponse(requestID string, response any) map[string]any {
	return map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": requestID, "response": response}}
}

func controlError(requestID, msg string) map[string]any {
	return map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": requestID, "error": msg}}
}

// refuseNativeRewind answers the next rewind_conversation request the way a
// CLI without it does, so Rewind restarts the process.
func refuseNativeRewind(h *fakeproc.Harness) {
	if req := h.RecvWhere(isControl("rewind_conversation")); req != nil {
		h.Send(controlError(fakeproc.Str(req, "request_id"), "Unsupported control request subtype: rewind_conversation"))
	}
}

// handshake answers the initialize request.
func handshake(h *fakeproc.Harness) map[string]any {
	init := h.RecvWhere(isControl("initialize"))
	if init == nil {
		return nil
	}
	h.Send(controlResponse(fakeproc.Str(init, "request_id"), map[string]any{
		"commands":                []any{map[string]any{"name": "review", "description": "Review code", "argumentHint": "", "builtin": false}},
		"agents":                  []any{},
		"models":                  []any{map[string]any{"value": "m1", "displayName": "Model One", "description": "d"}},
		"account":                 map[string]any{"email": "a@example.com"},
		"output_style":            "default",
		"available_output_styles": []any{"default"},
		"claude_code_version":     "9.9.9",
	}))
	settings := h.RecvWhere(isControl("get_settings"))
	h.Send(controlResponse(fakeproc.Str(settings, "request_id"), map[string]any{"applied": map[string]any{"effort": "medium"}}))
	return init
}

func stream(h *fakeproc.Harness, event map[string]any) {
	h.Send(map[string]any{"type": "stream_event", "event": event, "parent_tool_use_id": nil})
}

func collect(t *testing.T, sess agent.Session) <-chan []agent.Event {
	out := make(chan []agent.Event, 1)
	go func() {
		var all []agent.Event
		for ev := range sess.Events() {
			all = append(all, ev)
			if p, ok := ev.(*agent.PermissionRequest); ok && p.Kind == agent.PermissionCommand {
				if err := p.AllowForSession(context.Background()); err != nil {
					t.Errorf("answer permission: %v", err)
				}
			}
		}
		out <- all
	}()
	return out
}

func TestTurnStreamingAndPermission(t *testing.T) {
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		user := h.RecvWhere(isType("user"))
		uuid := fakeproc.Str(user, "uuid")
		h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "started"})
		h.Send(map[string]any{"type": "system", "subtype": "init", "model": "m1", "permissionMode": "default", "cwd": "/w",
			"tools": []any{"Bash"}, "mcp_servers": []any{}, "claude_code_version": "9.9.9", "user_message_uuids": []any{uuid}})
		stream(h, map[string]any{"type": "message_start", "message": map[string]any{"id": "msg1"}})
		stream(h, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		stream(h, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Hel"}})
		stream(h, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "lo"}})
		h.Send(map[string]any{"type": "assistant", "message": map[string]any{"id": "msg1", "content": []any{map[string]any{"type": "text", "text": "Hello"}}}, "parent_tool_use_id": nil})
		stream(h, map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash"}})
		h.Send(map[string]any{"type": "assistant", "message": map[string]any{"id": "msg1", "content": []any{map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "ls"}}}}, "parent_tool_use_id": nil})
		h.Send(map[string]any{"type": "control_request", "request_id": "cli_1", "request": map[string]any{
			"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ls"}, "tool_use_id": "tu1",
			"permission_suggestions": []any{map[string]any{"type": "addRules", "rules": []any{map[string]any{"toolName": "Bash", "ruleContent": "ls"}}, "behavior": "allow", "destination": "localSettings"}},
		}})
		resp := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "cli_1" })
		if fakeproc.Str(resp, "response", "response", "behavior") != "allow" ||
			fakeproc.Str(resp, "response", "response", "updatedPermissions", 0, "destination") != "session" ||
			fakeproc.Str(resp, "response", "response", "updatedInput", "command") != "ls" {
			t.Errorf("permission answer = %v", resp)
		}
		h.Send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "a.txt", "is_error": false}}},
			"parent_tool_use_id": nil, "tool_use_result": map[string]any{"stdout": "a.txt"}})
		h.Send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Hello", "num_turns": 2, "duration_ms": 1500,
			"total_cost_usd": 0.5, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}, "user_message_uuids": []any{uuid},
			"modelUsage": map[string]any{"m1": map[string]any{"contextWindow": 1000}}})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{Cwd: "/w", Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	h := <-started
	if !slices.Contains(h.Cmd.Args, "--permission-prompt-tool=stdio") || !slices.Contains(h.Cmd.Args, "--include-partial-messages") || !slices.Contains(h.Cmd.Args, "--model=m1") {
		t.Errorf("args = %v", h.Cmd.Args)
	}
	if !slices.Contains(h.Cmd.Env, "CLAUDE_CODE_ENTRYPOINT=sdk-ts") {
		t.Errorf("env = %v", h.Cmd.Env)
	}
	if !providerkitIsUUID(sess.ID()) || !slices.Contains(h.Cmd.Args, "--session-id="+sess.ID()) {
		t.Errorf("session id %q not passed: %v", sess.ID(), h.Cmd.Args)
	}
	info := sess.Info()
	if info.Version != "9.9.9" || len(info.Commands) != 1 || info.Commands[0].Name != "review" {
		t.Errorf("info = %+v", info)
	}
	events := collect(t, sess)
	res, err := sess.Run(ctx, agent.Text("list files"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnSucceeded || res.Text != "Hello" || res.Usage.CostUSD != 0.5 || res.Usage.ContextWindow != 1000 || res.Steps != 2 {
		t.Errorf("result = %+v", res)
	}
	sess.Close()
	all := <-events

	if _, ok := all[0].(*agent.SessionStarted); !ok {
		t.Errorf("first event %T, want SessionStarted", all[0])
	}
	var deltas []string
	var sawPermission, sawTool, sawTurnStarted bool
	for _, ev := range all {
		switch ev := ev.(type) {
		case *agent.TextDelta:
			deltas = append(deltas, ev.Delta)
		case *agent.PermissionRequest:
			sawPermission = ev.Command == "ls" && ev.Kind == agent.PermissionCommand && slices.Contains(ev.Decisions, agent.DecisionAllowSession)
		case *agent.ItemCompleted:
			if ev.Item.Tool != nil && ev.Item.Tool.Output == "a.txt" && ev.Item.Tool.Command == "ls" && ev.Item.Status == agent.ItemDone {
				sawTool = true
			}
		case *agent.TurnStarted:
			sawTurnStarted = ev.TurnID == res.TurnID
		}
	}
	if strings.Join(deltas, "") != "Hello" || !sawPermission || !sawTool || !sawTurnStarted {
		t.Errorf("deltas %q permission %v tool %v turnStarted %v", deltas, sawPermission, sawTool, sawTurnStarted)
	}
	if len(res.Items) != 3 {
		t.Errorf("items = %d, want user message, text, tool", len(res.Items))
	}
}

func providerkitIsUUID(s string) bool { return len(s) == 36 && strings.Count(s, "-") == 4 }

func TestInterruptControlErrorAndExit(t *testing.T) {
	exit := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		setModel := h.RecvWhere(isControl("set_model"))
		h.Send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": fakeproc.Str(setModel, "request_id"), "error": "unknown model"}})
		user := h.RecvWhere(isType("user"))
		uuid := fakeproc.Str(user, "uuid")
		h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "started"})
		interrupt := h.RecvWhere(isControl("interrupt"))
		h.Send(controlResponse(fakeproc.Str(interrupt, "request_id"), map[string]any{"still_queued": []any{}}))
		h.Send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "terminal_reason": "aborted_streaming",
			"errors": []any{}, "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		<-exit
		h.Stderr("fatal: boom")
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var perr *agent.ProtocolError
	if err := sess.SetModel(ctx, "nope"); !errors.As(err, &perr) || perr.Message != "unknown model" {
		t.Errorf("SetModel error = %v", err)
	}
	turn, err := sess.Send(ctx, agent.Text("work"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := turn.Wait(ctx)
	if !errors.Is(err, agent.ErrInterrupted) || res.Status != agent.TurnInterrupted {
		t.Fatalf("interrupted turn = %+v, %v", res, err)
	}
	close(exit)
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end when the process exited")
	}
	var exitErr *agent.ExitError
	if !errors.As(sess.Err(), &exitErr) || !strings.Contains(exitErr.Stderr, "boom") {
		t.Errorf("Err = %v", sess.Err())
	}
	if _, err := sess.Send(ctx, agent.Text("again")); !errors.Is(err, agent.ErrClosed) {
		t.Errorf("Send after exit = %v", err)
	}
}

func TestHostToolsHooksQuestionsAndElicitation(t *testing.T) {
	var hookInput claude.HookInput
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := handshake(h)
		if init == nil {
			return
		}
		if servers := fakeproc.Get(init, "request", "sdkMcpServers"); servers == nil {
			t.Errorf("initialize without sdkMcpServers: %v", init)
		}
		hookID := fakeproc.Str(init, "request", "hooks", "PreToolUse", 0, "hookCallbackIds", 0)
		call := func(id string, msg map[string]any) map[string]any {
			h.Send(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": "mcp_message", "server_name": "host", "message": msg}})
			return h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == id })
		}
		list := call("m1", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
		if fakeproc.Str(list, "response", "response", "mcp_response", "result", "tools", 0, "name") != "echo" {
			t.Errorf("tools/list = %v", list)
		}
		res := call("m2", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}}})
		if fakeproc.Str(res, "response", "response", "mcp_response", "result", "content", 0, "text") != "echo: hi" {
			t.Errorf("tools/call = %v", res)
		}
		h.Send(map[string]any{"type": "control_request", "request_id": "h1", "request": map[string]any{"subtype": "hook_callback", "callback_id": hookID,
			"input": map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf /"}, "session_id": "s"}}})
		hook := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "h1" })
		if fakeproc.Str(hook, "response", "response", "hookSpecificOutput", "permissionDecision") != "deny" ||
			fakeproc.Str(hook, "response", "response", "hookSpecificOutput", "hookEventName") != "PreToolUse" {
			t.Errorf("hook answer = %v", hook)
		}
		h.Send(map[string]any{"type": "control_request", "request_id": "q1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "tool_use_id": "tq",
			"input": map[string]any{"questions": []any{map[string]any{"question": "Which?", "header": "Pick", "multiSelect": false,
				"options": []any{map[string]any{"label": "A", "description": "a"}, map[string]any{"label": "B", "description": "b"}}}}}}})
		ans := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "q1" })
		if fakeproc.Str(ans, "response", "response", "updatedInput", "answers", "Which?") != "B" {
			t.Errorf("question answer = %v", ans)
		}
		h.Send(map[string]any{"type": "control_request", "request_id": "e1", "request": map[string]any{"subtype": "elicitation", "mcp_server_name": "srv", "message": "Token?",
			"requested_schema": map[string]any{"type": "object"}}})
		el := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "e1" })
		if fakeproc.Str(el, "response", "response", "action") != "accept" || fakeproc.Str(el, "response", "response", "content", "token") != "x" {
			t.Errorf("elicitation answer = %v", el)
		}
		h.WaitClosed()
	})
	echo := agent.Tool{
		Name:        "echo",
		Description: "Echo text.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Handler: func(ctx context.Context, call agent.ToolCallRequest) (agent.ToolResult, error) {
			var in struct{ Text string }
			_ = json.Unmarshal(call.Arguments, &in)
			return agent.TextResult("echo: " + in.Text), nil
		},
	}
	cfg := agent.Config{
		Tools: []agent.Tool{echo},
		Handlers: agent.Handlers{
			UserInput: func(ctx context.Context, req *agent.UserInputRequest) agent.UserInputResponse {
				return agent.UserInputResponse{Answers: map[string][]string{req.Questions[0].ID: {req.Questions[0].Options[1].Label}}}
			},
			Elicitation: func(ctx context.Context, req *agent.ElicitationRequest) agent.ElicitationResponse {
				return agent.ElicitationResponse{Action: agent.ElicitationAccept, Content: json.RawMessage(`{"token":"x"}`)}
			},
		},
		ProviderOptions: &claude.SessionOptions{Hooks: map[claude.HookEvent][]claude.HookMatcher{
			claude.HookPreToolUse: {{Matcher: "Bash", Hooks: []claude.HookFunc{func(ctx context.Context, in claude.HookInput) (claude.HookOutput, error) {
				hookInput = in
				return claude.HookOutput{Specific: claude.PreToolUseOutput{PermissionDecision: "deny", PermissionDecisionReason: "no"}}, nil
			}}}},
		}},
	}
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	deadline := time.After(10 * time.Second)
	for resolved < 2 {
		select {
		case ev := <-sess.Events():
			if _, ok := ev.(*agent.RequestResolved); ok {
				resolved++
			}
		case <-deadline:
			t.Fatal("requests were not resolved")
		}
	}
	sess.Close()
	if hookInput.ToolName != "Bash" || !strings.Contains(string(hookInput.ToolInput), "rm -rf") {
		t.Errorf("hook input = %+v", hookInput)
	}
}

func TestTurnOptionsOfAnotherTypeRefused(t *testing.T) {
	release := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		<-release
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	defer close(release)
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("x")}, ProviderOptions: "options"})
	if err == nil || !strings.Contains(err.Error(), "Input.ProviderOptions holds string; want *claude.TurnOptions") {
		t.Errorf("Send = %v", err)
	}
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("x")}, Delivery: "Steer"})
	if err == nil || !strings.Contains(err.Error(), `unknown Input.Delivery "Steer"`) {
		t.Errorf("Send with bad delivery = %v", err)
	}
}

func TestConfigOutputSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	release := make(chan struct{})
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := handshake(h)
		if init == nil {
			return
		}
		if fakeproc.Str(init, "request", "jsonSchema", "type") != "object" {
			t.Errorf("initialize without jsonSchema: %v", init)
		}
		<-release
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{OutputSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	defer close(release)
	if h := <-started; !slices.Contains(h.Cmd.Args, "--json-schema="+string(schema)) {
		t.Errorf("args = %v", h.Cmd.Args)
	}
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("x")}, OutputSchema: schema})
	if !errors.Is(err, agent.ErrUnsupported) || !strings.Contains(err.Error(), "agent.Config.OutputSchema") {
		t.Errorf("Send with a per-turn schema = %v", err)
	}
}

func TestInterruptTurnCancelsQueuedThenInterruptsRunning(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		first := fakeproc.Str(h.RecvWhere(isType("user")), "uuid")
		h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": first, "state": "started"})
		queued := h.RecvWhere(isType("user"))
		if fakeproc.Str(queued, "priority") != "later" {
			t.Errorf("second message priority = %v", queued["priority"])
		}
		cancel := h.RecvWhere(isControl("cancel_async_message"))
		if fakeproc.Str(cancel, "request", "message_uuid") != fakeproc.Str(queued, "uuid") {
			t.Errorf("cancel_async_message = %v", cancel)
		}
		h.Send(controlResponse(fakeproc.Str(cancel, "request_id"), map[string]any{"cancelled": true}))
		cancel = h.RecvWhere(isControl("cancel_async_message"))
		h.Send(controlResponse(fakeproc.Str(cancel, "request_id"), map[string]any{"cancelled": false}))
		interrupt := h.RecvWhere(isControl("interrupt"))
		if fakeproc.Get(interrupt, "request", "cancel_queued") != nil {
			t.Errorf("interrupt cancels the queue: %v", interrupt)
		}
		h.Send(controlResponse(fakeproc.Str(interrupt, "request_id"), map[string]any{"still_queued": []any{}}))
		h.Send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "terminal_reason": "aborted_streaming",
			"errors": []any{}, "user_message_uuids": []any{first}, "usage": map[string]any{}})
		h.RecvWhere(func(map[string]any) bool { return false })
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var _ agent.TurnInterrupter = sess
	first, err := sess.Send(ctx, agent.Text("work"))
	if err != nil {
		t.Fatal(err)
	}
	for ev := range first.Events() {
		if _, ok := ev.(*agent.TurnStarted); ok {
			break
		}
	}
	second, err := sess.Send(ctx, agent.Text("later"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.InterruptTurn(ctx, second.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Wait(ctx); !errors.Is(err, agent.ErrCancelled) {
		t.Errorf("queued turn = %v, want ErrCancelled", err)
	}
	select {
	case <-first.Done():
		t.Fatal("cancelling the queued turn ended the running one")
	default:
	}
	if err := sess.InterruptTurn(ctx, first.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Wait(ctx); !errors.Is(err, agent.ErrInterrupted) {
		t.Errorf("running turn = %v, want ErrInterrupted", err)
	}
	if err := sess.InterruptTurn(ctx, first.ID()); err != nil {
		t.Errorf("InterruptTurn on an ended turn = %v", err)
	}
}

func TestSessionCapabilitiesFollowOptions(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		h.WaitClosed()
	})
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(context.Background(), agent.Config{
		ProviderOptions: &claude.SessionOptions{NoPartialMessages: true, NoPermissionPrompts: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if c := sess.Capabilities(); c.Streaming || c.ReasoningStream || c.PermissionPrompt {
		t.Errorf("capabilities = %+v", c)
	}
}

func TestQueuedInputRefusesModelSwitch(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		first := fakeproc.Str(h.RecvWhere(isType("user")), "uuid")
		h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": first, "state": "started"})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Send(ctx, agent.Text("work")); err != nil {
		t.Fatal(err)
	}
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("later")}, Model: "opus"})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("queued input with a model = %v, want ErrUnsupported", err)
	}
}
