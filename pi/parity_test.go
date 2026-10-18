package pi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// startupWith answers the startup commands with the given state and
// command list.
func startupWith(h *fakeproc.Harness, state map[string]any, commands []any) bool {
	st := h.RecvWhere(isCommand("get_state"))
	if st == nil {
		return false
	}
	respond(h, st, state)
	cmds := h.RecvWhere(isCommand("get_commands"))
	respond(h, cmds, map[string]any{"commands": commands})
	return true
}

func baseState() map[string]any {
	return map[string]any{"sessionId": "pi-1", "thinkingLevel": "low",
		"model": map[string]any{"id": "flash", "provider": "openrouter", "contextWindow": 9000, "input": []any{"text", "image"}}}
}

// tokenOf reads the bridge token from the spawned environment.
func tokenOf(t *testing.T, h *fakeproc.Harness) string {
	for _, kv := range h.Cmd.Env {
		if v, ok := strings.CutPrefix(kv, "AGENT_CLI_BRIDGE="); ok {
			var cfg struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal([]byte(v), &cfg); err != nil {
				t.Fatalf("bridge config: %v", err)
			}
			return cfg.Token
		}
	}
	t.Fatal("no bridge config in environment")
	return ""
}

func bridgeConfig(t *testing.T, h *fakeproc.Harness) map[string]any {
	for _, kv := range h.Cmd.Env {
		if v, ok := strings.CutPrefix(kv, "AGENT_CLI_BRIDGE="); ok {
			var cfg map[string]any
			if err := json.Unmarshal([]byte(v), &cfg); err != nil {
				t.Fatalf("bridge config: %v", err)
			}
			return cfg
		}
	}
	t.Fatal("no bridge config in environment")
	return nil
}

func status(h *fakeproc.Harness, key string, v any) {
	data, _ := json.Marshal(v)
	h.Send(map[string]any{"type": "extension_ui_request", "id": "st-" + key, "method": "setStatus", "statusKey": key, "statusText": string(data)})
}

// collect records a session's events until it closes.
type collector struct {
	mu     sync.Mutex
	events []agent.Event
	done   chan struct{}
}

func collect(sess agent.Session) *collector {
	c := &collector{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for ev := range sess.Events() {
			c.mu.Lock()
			c.events = append(c.events, ev)
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *collector) all() []agent.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

// waitFor polls the collected events until match finds one.
func (c *collector) waitFor(t *testing.T, match func(agent.Event) bool) agent.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range c.all() {
			if match(ev) {
				return ev
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("event not seen")
	return nil
}

func TestInterruptClearsPiQueueButKeepsLibraryQueue(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		prompt := h.RecvWhere(isCommand("prompt"))
		respond(h, prompt, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		next := h.Recv()
		if fakeproc.Str(next, "type") != "clear_queue" {
			t.Errorf("first command after interrupt = %v, want clear_queue", next)
		}
		h.Send(map[string]any{"id": next["id"], "type": "response", "command": "clear_queue", "success": false, "error": "Unknown command: clear_queue"})
		abort := h.RecvWhere(isCommand("abort"))
		h.Send(map[string]any{"type": "agent_settled", "aborted": true})
		respond(h, abort, nil)
		queued := h.RecvWhere(isCommand("prompt"))
		if fakeproc.Str(queued, "message") != "second" {
			t.Errorf("queued prompt = %v", queued)
		}
		respond(h, queued, map[string]any{"disposition": "handled"})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	first, err := sess.Send(ctx, agent.Text("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := sess.Send(ctx, agent.Text("second"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatalf("interrupt with old Pi: %v", err)
	}
	if res, _ := first.Wait(ctx); res.Status != agent.TurnInterrupted {
		t.Errorf("first = %+v", res)
	}
	if res, err := second.Wait(ctx); err != nil || res.Status != agent.TurnSucceeded {
		t.Errorf("queued input after interrupt = %+v, %v", res, err)
	}
}

func TestInPlaceRewindAndLiveEntryIDs(t *testing.T) {
	rewindCmd := map[string]any{"name": "agent-cli-rewind", "source": "extension"}
	var token string
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startupWith(h, baseState(), []any{rewindCmd}) {
			return
		}
		token = tokenOf(t, h)
		prompt := h.RecvWhere(isCommand("prompt"))
		respond(h, prompt, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		h.Send(map[string]any{"type": "message_start", "message": map[string]any{"role": "user"}})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "user", "content": "hello", "timestamp": 100}})
		steer := h.RecvWhere(isCommand("prompt"))
		respond(h, steer, map[string]any{"disposition": "queued"})
		status(h, token+":user", map[string]any{"entryId": "e1", "timestamp": 100})
		h.Send(map[string]any{"type": "message_start", "message": map[string]any{"role": "assistant"}})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{}}})
		h.Send(map[string]any{"type": "message_start", "message": map[string]any{"role": "user"}})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "user", "content": "also", "timestamp": 200}})
		status(h, token+":user", map[string]any{"entryId": "e2", "timestamp": 200})
		h.Send(map[string]any{"type": "message_start", "message": map[string]any{"role": "assistant"}})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": "ok"}}}})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})

		for _, want := range []string{"e1", "e2", "e3"} {
			cmd := h.RecvWhere(isCommand("prompt"))
			name, arg, _ := strings.Cut(fakeproc.Str(cmd, "message"), " ")
			if name != "/agent-cli-rewind" {
				t.Errorf("rewind prompt = %v", cmd)
			}
			if _, ok := cmd["streamingBehavior"]; ok {
				t.Errorf("rewind prompt has streamingBehavior: %v", cmd)
			}
			data, _ := base64.RawURLEncoding.DecodeString(arg)
			var req map[string]string
			_ = json.Unmarshal(data, &req)
			if req["targetId"] != want || req["token"] != token {
				t.Errorf("rewind request = %v, want target %s", req, want)
			}
			result := map[string]any{"nonce": req["nonce"], "ok": true}
			if want == "e3" {
				result = map[string]any{"nonce": req["nonce"], "ok": false, "error": "Entry e3 not found"}
			}
			status(h, token+":rewind", result)
			respond(h, cmd, map[string]any{"disposition": "handled"})
			if want != "e3" {
				st := h.RecvWhere(isCommand("get_state"))
				respond(h, st, baseState())
			}
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	events := collect(sess)
	for _, c := range sess.Info().Commands {
		if c.Name == "agent-cli-rewind" {
			t.Error("internal rewind command listed")
		}
	}
	in := agent.Text("hello")
	in.ClientID = "client-1"
	turn, err := sess.Send(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	steer := agent.Text("also")
	steer.ClientID = "client-2"
	steer.Delivery = agent.DeliverSteer
	if _, err := sess.Send(ctx, steer); err != nil {
		t.Fatal(err)
	}
	res, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var users []agent.Item
	for _, it := range res.Items {
		if it.Kind == agent.ItemUserMessage {
			users = append(users, it)
		}
	}
	if len(users) != 2 || users[0].ID != "e1" || users[0].ClientID != "client-1" || users[0].TurnID != turn.ID() || users[0].Time.UnixMilli() != 100 ||
		users[1].ID != "e2" || users[1].ClientID != "client-2" {
		t.Fatalf("user items = %+v", users)
	}
	if err := sess.Rewind(ctx, res.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := sess.Rewind(ctx, "e2"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Rewind(ctx, "e3"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("failed rewind error = %v", err)
	}
	if sess.ID() != "pi-1" {
		t.Errorf("session ID after in-place rewind = %q", sess.ID())
	}
	sess.Close()
	<-events.done
	completed := 0
	for _, ev := range events.all() {
		if _, ok := ev.(*agent.TurnCompleted); ok {
			completed++
		}
	}
	if completed != 1 {
		t.Errorf("turns completed = %d, want only the prompt's turn", completed)
	}
}

func TestImageInputNeedsImageModel(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		state := baseState()
		state["model"] = map[string]any{"id": "text-only", "provider": "openrouter", "input": []any{"text"}}
		if !startupWith(h, state, nil) {
			return
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	in := agent.Input{Parts: []agent.Part{agent.TextPart("what is this"), agent.ImageData("image/png", []byte{1, 2, 3})}}
	if _, err := sess.Send(ctx, in); !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("image to a text-only model: %v", err)
	}

	listSpawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		models := h.RecvWhere(isCommand("get_available_models"))
		respond(h, models, map[string]any{"models": []any{
			map[string]any{"id": "flash", "provider": "openrouter", "input": []any{"text", "image"}},
			map[string]any{"id": "coder", "provider": "openrouter", "input": []any{"text"}},
		}})
		h.WaitClosed()
	})
	models, err := pi.New(pi.Options{Spawner: listSpawner}).ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || !slices.Equal(models[0].InputModalities, []string{"text", "image"}) || !slices.Equal(models[1].InputModalities, []string{"text"}) {
		t.Errorf("models = %+v", models)
	}
}

func TestResumeAppliesAvailableModelAndClampedEffort(t *testing.T) {
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		st := h.RecvWhere(isCommand("get_state"))
		if st == nil {
			return
		}
		respond(h, st, baseState())
		models := h.RecvWhere(isCommand("get_available_models"))
		available := []any{map[string]any{"id": "flash", "provider": "openrouter"}}
		if slices.Contains(h.Cmd.Args, "pi-new") {
			available = append(available, map[string]any{"id": "pro", "provider": "anthropic"})
		}
		respond(h, models, map[string]any{"models": available})
		if slices.Contains(h.Cmd.Args, "pi-new") {
			set := h.RecvWhere(isCommand("set_model"))
			if fakeproc.Str(set, "provider") != "anthropic" || fakeproc.Str(set, "modelId") != "pro" {
				t.Errorf("set_model = %v", set)
			}
			respond(h, set, map[string]any{"id": "pro", "provider": "anthropic"})
			st := h.RecvWhere(isCommand("get_state"))
			state := baseState()
			state["model"] = map[string]any{"id": "pro", "provider": "anthropic"}
			respond(h, st, state)
		}
		cmds := h.RecvWhere(isCommand("get_commands"))
		respond(h, cmds, map[string]any{"commands": []any{}})
		if slices.Contains(h.Cmd.Args, "pi-new") {
			level := h.RecvWhere(isCommand("set_thinking_level"))
			respond(h, level, nil)
			st := h.RecvWhere(isCommand("get_state"))
			state := baseState()
			state["thinkingLevel"] = "medium"
			respond(h, st, state)
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	p := pi.New(pi.Options{Spawner: spawner, DisableBridge: true})
	sess, err := p.StartSession(ctx, agent.Config{Resume: "pi-new", Model: "anthropic:pro"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	h := <-started
	if slices.Contains(h.Cmd.Args, "--model") {
		t.Errorf("resume passed --model: %v", h.Cmd.Args)
	}
	if sess.Info().Model != "anthropic/pro" {
		t.Errorf("model after resume = %q", sess.Info().Model)
	}
	if err := sess.SetEffort(ctx, agent.EffortXHigh); err != nil {
		t.Fatal(err)
	}
	if e := sess.Info().Effort; e != agent.EffortMedium {
		t.Errorf("effort after clamp = %q, want Pi's medium", e)
	}

	kept, err := p.StartSession(ctx, agent.Config{Resume: "pi-old", Model: "anthropic/gone"})
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	if kept.Info().Model != "openrouter/flash" {
		t.Errorf("unavailable model replaced the session's model: %q", kept.Info().Model)
	}
}

func TestCustomMessagesTodoPlansAndTurnError(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		prompt := h.RecvWhere(isCommand("prompt"))
		respond(h, prompt, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "custom", "customType": "note", "content": "hidden by default", "timestamp": 1}})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "custom", "customType": "note", "content": "shown", "display": true, "timestamp": 2}})
		h.Send(map[string]any{"type": "tool_execution_start", "toolCallId": "t1", "toolName": "todo", "args": map[string]any{"action": "add", "text": "b"}})
		h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "t1", "toolName": "todo", "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "Added"}},
			"details": map[string]any{"action": "add", "nextId": 3, "todos": []any{
				map[string]any{"id": 1, "text": "a", "done": true}, map[string]any{"id": 2, "text": "b", "done": false}}}}})
		h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "t2", "toolName": "todo", "result": map[string]any{
			"details": map[string]any{"action": "update", "nextId": 3, "tasks": []any{
				map[string]any{"id": 1, "subject": "x", "status": "in_progress"}, map[string]any{"id": 2, "subject": "y", "status": "deleted"}}}}})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "overloaded",
			"provider": "openrouter", "model": "flash", "responseId": "resp-9", "content": []any{}}})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	events := collect(sess)
	res, err := sess.Run(ctx, agent.Text("go"))
	if res == nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnFailed || res.Err == nil || !strings.Contains(res.Err.Error(), "overloaded") ||
		!strings.Contains(res.Err.Error(), "openrouter/flash") || !strings.Contains(res.Err.Error(), "resp-9") {
		t.Errorf("turn error = %v", res.Err)
	}
	var others []string
	for _, it := range res.Items {
		if it.Kind == agent.ItemOther {
			others = append(others, it.Text)
		}
	}
	if !slices.Equal(others, []string{"shown"}) {
		t.Errorf("custom message items = %v", others)
	}
	sess.Close()
	<-events.done
	var raw int
	var plans [][]agent.PlanStep
	for _, ev := range events.all() {
		switch e := ev.(type) {
		case *agent.RawEvent:
			if e.Type == "custom_message" {
				raw++
			}
		case *agent.PlanUpdated:
			plans = append(plans, e.Steps)
		}
	}
	if raw != 1 {
		t.Errorf("hidden custom messages as raw events = %d", raw)
	}
	want := [][]agent.PlanStep{
		{{ID: "1", Text: "a", Status: agent.StepCompleted}, {ID: "2", Text: "b", Status: agent.StepPending}},
		{{ID: "1", Text: "x", Status: agent.StepInProgress}},
	}
	if len(plans) != 2 || !slices.Equal(plans[0], want[0]) || !slices.Equal(plans[1], want[1]) {
		t.Errorf("plans = %+v", plans)
	}
}

func TestSubagentExtensionResults(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		prompt := h.RecvWhere(isCommand("prompt"))
		respond(h, prompt, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		h.Send(map[string]any{"type": "tool_execution_start", "toolCallId": "ps", "toolName": "subagent", "args": map[string]any{"agent": "scout", "task": "map it"}})
		h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "ps", "toolName": "subagent", "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "mapped"}},
			"details": map[string]any{"mode": "single", "results": []any{
				map[string]any{"index": 0, "agent": "scout", "exitCode": 0, "success": true, "sessionFile": "/tmp/child-a.jsonl"}}}}})
		h.Send(map[string]any{"type": "tool_execution_start", "toolCallId": "gg", "toolName": "subagent", "args": map[string]any{"subagent_type": "general-purpose", "prompt": "list files"}})
		h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "gg", "toolName": "subagent", "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "empty"}},
			"details": map[string]any{"agentId": "a1", "status": "completed", "description": "List files", "subagentType": "general-purpose",
				"transcriptPath": "/tmp/child-b.jsonl", "toolUses": 1, "durationMs": 4723}}})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	events := collect(sess)
	if _, err := sess.Run(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	if f := sess.SubagentSessionFile("ps:subagent:0"); f != "/tmp/child-a.jsonl" {
		t.Errorf("pi-subagents session file = %q", f)
	}
	if f := sess.SubagentSessionFile("gg:subagent:0"); f != "/tmp/child-b.jsonl" {
		t.Errorf("gotgenes transcript = %q", f)
	}
	sess.Close()
	<-events.done
	tasks := map[string]agent.Task{}
	for _, ev := range events.all() {
		if tu, ok := ev.(*agent.TaskUpdated); ok {
			tasks[tu.Task.ID] = tu.Task
		}
	}
	ps, gg := tasks["ps:subagent:0"], tasks["gg:subagent:0"]
	if ps.Status != agent.TaskCompleted || ps.Description != "map it" || ps.Output != "mapped" || ps.Subagent.Type != "scout" {
		t.Errorf("pi-subagents task = %+v", ps)
	}
	if gg.Status != agent.TaskCompleted || gg.Description != "List files" || gg.Output != "empty" || gg.Subagent.Type != "general-purpose" ||
		gg.Duration != 4723*time.Millisecond {
		t.Errorf("gotgenes task = %+v", gg)
	}
}

func writeLines(t *testing.T, path string, lines ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		data, _ := json.Marshal(l)
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreDiscoveryAndSubagents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cwd := filepath.Join(dir, "project")
	child := filepath.Join(dir, "children", "child.jsonl")
	writeLines(t, child,
		map[string]any{"type": "session", "id": "child-1", "cwd": cwd},
		map[string]any{"type": "message", "id": "c1", "parentId": nil, "message": map[string]any{"role": "user", "content": "map it"}},
		map[string]any{"type": "message", "id": "c2", "parentId": "c1", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "mapped"}}}},
	)
	writeLines(t, filepath.Join(dir, "sessions", "2026_parent-1.jsonl"),
		map[string]any{"type": "session", "id": "parent-1", "cwd": cwd, "timestamp": "2026-10-17T10:00:00Z"},
		map[string]any{"type": "model_change", "id": "m0", "parentId": nil, "provider": "openrouter", "modelId": "flash"},
		map[string]any{"type": "thinking_level_change", "id": "m1", "parentId": "m0", "thinkingLevel": "off"},
		map[string]any{"type": "message", "id": "u1", "parentId": "m1", "timestamp": "2026-10-17T10:00:01Z", "message": map[string]any{"role": "user", "content": "first prompt"}},
		map[string]any{"type": "message", "id": "a1", "parentId": "u1", "message": map[string]any{"role": "assistant", "provider": "anthropic", "model": "pro",
			"content": []any{map[string]any{"type": "toolCall", "id": "call1", "name": "subagent", "arguments": map[string]any{"agent": "scout", "task": "map it"}}}}},
		map[string]any{"type": "message", "id": "r1", "parentId": "a1", "message": map[string]any{"role": "toolResult", "toolCallId": "call1", "toolName": "subagent",
			"content": []any{map[string]any{"type": "text", "text": "mapped"}},
			"details": map[string]any{"results": []any{map[string]any{"index": 0, "agent": "scout", "exitCode": 0, "success": true, "sessionFile": child}}}}},
		map[string]any{"type": "custom_message", "id": "n1", "parentId": "r1", "customType": "note", "content": "visible note", "display": true},
		map[string]any{"type": "custom_message", "id": "n2", "parentId": "n1", "customType": "note", "content": "hidden note"},
		map[string]any{"type": "message", "id": "u2", "parentId": "n2", "message": map[string]any{"role": "user", "content": "last prompt"}},
	)
	writeLines(t, filepath.Join(dir, "sessions", "2026_other-1.jsonl"),
		map[string]any{"type": "session", "id": "other-1", "cwd": filepath.Join(dir, "elsewhere")})

	// The project's settings name the session directory relative to it.
	writeLines(t, filepath.Join(cwd, ".pi", "settings.json"), map[string]any{"sessionDir": "../sessions"})
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(dir, "agent"))
	for name, p := range map[string]*pi.Provider{
		"settings":    pi.New(pi.Options{}),
		"option":      pi.New(pi.Options{SessionDir: filepath.Join(dir, "sessions")}),
		"home option": pi.New(pi.Options{SessionDir: "~/sessions"}),
	} {
		if name == "home option" {
			t.Setenv("HOME", dir)
		}
		sums, err := p.ListSessions(ctx, agent.ListSessionsOptions{Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}
		if len(sums) != 1 || sums[0].ID != "parent-1" || sums[0].Model != "anthropic/pro" || sums[0].Effort != agent.EffortNone ||
			sums[0].FirstPrompt != "first prompt" || sums[0].LastPrompt != "last prompt" {
			t.Errorf("%s: summaries = %+v", name, sums)
		}
	}

	p := pi.New(pi.Options{SessionDir: filepath.Join(dir, "sessions")})
	items, err := p.ReadHistory(ctx, "parent-1", agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range items {
		if it.Kind == agent.ItemOther {
			texts = append(texts, it.Text)
		}
	}
	if !slices.Equal(texts, []string{"visible note"}) || items[0].TurnID != "u1" || items[len(items)-1].TurnID != "u2" {
		t.Errorf("history = %+v", items)
	}
	tasks, err := p.ListSubagents(ctx, "parent-1", agent.ListSubagentsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "call1:subagent:0" || tasks[0].Status != agent.TaskCompleted {
		t.Fatalf("subagents = %+v", tasks)
	}
	childItems, err := p.ReadHistory(ctx, "parent-1", agent.ReadHistoryOptions{SubagentID: tasks[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(childItems) != 2 || childItems[1].Text != "mapped" {
		t.Errorf("subagent history = %+v", childItems)
	}
	if _, err := p.ReadHistory(ctx, "missing", agent.ReadHistoryOptions{}); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Errorf("missing session error = %v", err)
	}
	page, err := p.ReadHistory(ctx, "parent-1", agent.ReadHistoryOptions{Limit: 2, Offset: 1})
	if err != nil || len(page) != 2 || page[0].ID != items[len(items)-3].ID || page[1].ID != items[len(items)-2].ID {
		t.Errorf("history page = %+v, %v", page, err)
	}
	all, _ := p.ListSessions(ctx, agent.ListSessionsOptions{})
	rest, err := p.ListSessions(ctx, agent.ListSessionsOptions{Offset: 1})
	if err != nil || len(all) != 2 || len(rest) != 1 || rest[0].ID != all[1].ID {
		t.Errorf("sessions %+v, after offset 1: %+v, %v", all, rest, err)
	}

	if err := p.DeleteSession(ctx, "*", ""); err == nil {
		t.Error("glob session ID accepted")
	}
	if err := p.DeleteSession(ctx, filepath.Join(cwd, ".pi", "settings.json"), ""); err == nil {
		t.Error("deleted a file that is not a session")
	}
	if err := p.DeleteSession(ctx, "parent-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadHistory(ctx, "parent-1", agent.ReadHistoryOptions{}); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Errorf("history after delete = %v", err)
	}
	if _, err := os.Stat(child); err != nil {
		t.Errorf("subagent session file removed: %v", err)
	}
	if err := p.DeleteSession(ctx, "parent-1", ""); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Errorf("second delete = %v", err)
	}
}

func TestMCPServerConfigAndApprovedTools(t *testing.T) {
	ctx := context.Background()
	p := pi.New(pi.Options{Spawner: agent.SpawnFunc(func(context.Context, agent.Command) (agent.Process, error) {
		t.Fatal("started despite an invalid MCP server name")
		return nil, nil
	})})
	if _, err := p.StartSession(ctx, agent.Config{MCPServers: map[string]agent.MCPServer{"bad name": agent.StdioServer("x")}}); err == nil {
		t.Error("invalid MCP server name accepted")
	}
	if _, err := p.StartSession(ctx, agent.Config{MCPServers: map[string]agent.MCPServer{"a-b": agent.StdioServer("x"), "a_b": agent.StdioServer("y")}}); err == nil {
		t.Error("colliding MCP server names accepted")
	}

	asked := make(chan string, 4)
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		token := tokenOf(t, h)
		answer := bridge(h, token, "g1", map[string]any{"kind": "permission", "toolName": "mcp__docs__search", "toolCallId": "c1", "input": map[string]any{}})
		if answer["allow"] != true {
			t.Errorf("approved MCP tool answer = %v", answer)
		}
		h.WaitClosed()
	})
	srv := agent.StdioServer("docs-server")
	srv.EnabledTools = []string{"search", "fetch"}
	srv.ApprovedTools = []string{"search"}
	srv.AlwaysLoad = true
	cfg := agent.Config{
		PermissionMode: agent.PermissionDefault,
		MCPServers:     map[string]agent.MCPServer{"docs": srv},
		UnsetEnv:       []string{"CLAUDECODE"},
		Handlers: agent.Handlers{Permission: func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			asked <- req.ToolName
			return agent.PermissionResponse{Decision: agent.DecisionDeny}
		}},
	}
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	h := <-started
	if !slices.Contains(h.Cmd.UnsetEnv, "CLAUDECODE") {
		t.Errorf("UnsetEnv not passed: %v", h.Cmd.UnsetEnv)
	}
	docs := fakeproc.Get(bridgeConfig(t, h), "mcpServers", "docs")
	if fakeproc.Str(docs, "exposure") != "hidden" || fakeproc.Str(docs, "toolExposure", "search") != "direct" || fakeproc.Str(docs, "toolExposure", "fetch") != "direct" {
		t.Errorf("MCP server config = %v", docs)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case name := <-asked:
		t.Errorf("asked about approved tool %s", name)
	default:
	}
}

func TestBuiltinCompactCommandsAndContextUsage(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		compact := h.RecvWhere(isCommand("compact"))
		if fakeproc.Str(compact, "customInstructions") != "keep the tests" {
			t.Errorf("compact = %v", compact)
		}
		h.Send(map[string]any{"type": "compaction_start", "reason": "manual"})
		h.Send(map[string]any{"type": "compaction_end", "reason": "manual", "result": map[string]any{"summary": "S", "tokensBefore": 5000}})
		respond(h, compact, map[string]any{"summary": "S"})
		stats := h.RecvWhere(isCommand("get_session_stats"))
		respond(h, stats, map[string]any{"contextUsage": map[string]any{"tokens": 1234, "contextWindow": 9000}})

		st := h.RecvWhere(isCommand("get_state"))
		respond(h, st, map[string]any{"sessionId": "pi-1", "autoCompactionEnabled": true})
		set := h.RecvWhere(isCommand("set_auto_compaction"))
		if fakeproc.Get(set, "enabled") != false {
			t.Errorf("set_auto_compaction = %v", set)
		}
		respond(h, set, nil)
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	events := collect(sess)
	res, err := sess.Run(ctx, agent.Text("/compact keep the tests"))
	if err != nil || res.Status != agent.TurnSucceeded || res.Text != "S" || res.StopReason != "handled" {
		t.Errorf("/compact = %+v, %v", res, err)
	}
	ev := events.waitFor(t, func(ev agent.Event) bool {
		u, ok := ev.(*agent.UsageUpdated)
		return ok && u.Usage.ContextTokens == 1234
	})
	if u := ev.(*agent.UsageUpdated); u.Usage.ContextWindow != 9000 {
		t.Errorf("usage after compaction = %+v", u.Usage)
	}
	res, err = sess.Run(ctx, agent.Text("/autocompact"))
	if err != nil || res.Status != agent.TurnSucceeded || res.Text != "Auto-compaction disabled" {
		t.Errorf("/autocompact = %+v, %v", res, err)
	}
}
