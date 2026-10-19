package codex_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

func rpcError(h *fakeproc.Harness, req map[string]any, code int, message string, data any) {
	h.Send(map[string]any{"id": req["id"], "error": map[string]any{"code": code, "message": message, "data": data}})
}

func waitDone(t *testing.T, req agent.Request, want bool) {
	t.Helper()
	select {
	case <-req.Done():
		if !want {
			t.Errorf("request %s was withdrawn", req.RequestID())
		}
	case <-time.After(200 * time.Millisecond):
		if want {
			t.Errorf("request %s is still pending", req.RequestID())
		}
	}
}

func TestSandboxReuseAndInterruptDuringStart(t *testing.T) {
	gotStart := make(chan struct{})
	release := make(chan struct{})
	scripted := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := h.RecvWhere(fakeproc.Method("initialize"))
		if fakeproc.Get(init, "params", "capabilities", "extensions", "openai/form") == nil {
			t.Errorf("initialize = %v", init)
		}
		h.Reply(init, map[string]any{"userAgent": "fake"})
		start := h.RecvWhere(fakeproc.Method("thread/start"))
		h.Reply(start, map[string]any{"thread": map[string]any{"id": thread}, "model": "gpt-test", "cwd": "/w",
			"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{"/r"}, "networkAccess": true, "excludeSlashTmp": true, "excludeTmpdirEnvVar": false}})

		ts := h.RecvWhere(fakeproc.Method("turn/start"))
		if fakeproc.Str(ts, "params", "sandboxPolicy", "writableRoots", 0) != "/r" || fakeproc.Get(ts, "params", "sandboxPolicy", "excludeSlashTmp") != true {
			t.Errorf("turn/start sandbox = %v", ts)
		}
		close(gotStart)
		<-release
		h.Reply(ts, map[string]any{"turn": map[string]any{"id": "t1", "status": "inProgress", "items": []any{}}})
		turnStarted(h, "t1")
		stop := h.RecvWhere(fakeproc.Method("turn/interrupt"))
		if fakeproc.Str(stop, "params", "turnId") != "t1" {
			t.Errorf("pending interrupt = %v", stop)
		}
		h.Reply(stop, map[string]any{})
		turnCompleted(h, "t1", "interrupted", nil)

		startTurn(h, "t2")
		stop = h.RecvWhere(fakeproc.Method("turn/interrupt"))
		rpcError(h, stop, -32600, "no active turn to interrupt", nil)
		turnCompleted(h, "t2", "completed", nil)
		close(scripted)
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sent := make(chan *agent.Turn, 1)
	go func() {
		turn, err := sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("go")}, ProviderOptions: codex.TurnOptions{SandboxPolicy: "workspace-write"}})
		if err != nil {
			t.Error(err)
		}
		sent <- turn
	}()
	<-gotStart
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	res, err := (<-sent).Wait(ctx)
	if !errors.Is(err, agent.ErrInterrupted) || res.Status != agent.TurnInterrupted {
		t.Fatalf("first turn = %+v, %v", res, err)
	}
	turn, err := sess.Send(ctx, agent.Text("again"))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, sess, func(ev agent.Event) bool {
		s, ok := ev.(*agent.TurnStarted)
		return ok && s.TurnID == "t2"
	})
	if err := sess.Interrupt(ctx); err != nil {
		t.Errorf("Interrupt of an idle turn = %v", err)
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, scripted)
}

func TestForkAndRewindKeepSettings(t *testing.T) {
	scripted := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		fork := h.RecvWhere(fakeproc.Method("thread/fork"))
		p := fork["params"]
		if fakeproc.Str(p, "model") != "gpt-test" || fakeproc.Str(p, "baseInstructions") != "base" || fakeproc.Str(p, "developerInstructions") != "dev" ||
			fakeproc.Str(p, "sandbox") != "read-only" || fakeproc.Str(p, "approvalsReviewer") != "user" ||
			fakeproc.Str(p, "config", "model_reasoning_effort") != "high" ||
			fakeproc.Str(p, "config", "mcp_servers.m", "enabled_tools", 0) != "a" ||
			fakeproc.Str(p, "config", "mcp_servers.m", "tools", "a", "approval_mode") != "approve" {
			t.Errorf("fork = %v", fork)
		}
		h.Reply(fork, map[string]any{"thread": map[string]any{"id": "th-2"}, "model": "gpt-test"})

		// t1 and t2 each ask an async question.
		for _, id := range []string{"t1", "t2"} {
			startTurn(h, id)
			item(h, "item/completed", id, map[string]any{"type": "agentMessage", "id": "a-" + id, "text": "?", "delivery": "async",
				"questions": []any{map[string]any{"title": "Pick", "options": []any{"x"}}}})
			turnCompleted(h, id, "completed", nil)
		}

		read := h.RecvWhere(fakeproc.Method("thread/read"))
		h.Reply(read, map[string]any{"thread": map[string]any{"id": thread, "historyMode": "legacy"}})
		fork = h.RecvWhere(fakeproc.Method("thread/fork"))
		p = fork["params"]
		if fakeproc.Str(p, "threadId") != thread || fakeproc.Str(p, "beforeTurnId") != "t2" || fakeproc.Get(p, "excludeTurns") != true ||
			fakeproc.Get(p, "lastTurnId") != nil || fakeproc.Str(p, "developerInstructions") != "dev" || fakeproc.Str(p, "config", "mcp_servers.m", "enabled_tools", 0) != "a" {
			t.Errorf("rewind fork = %v", fork)
		}
		h.Reply(fork, map[string]any{"thread": map[string]any{"id": "th-9"}, "model": "gpt-test"})
		unsub := h.RecvWhere(fakeproc.Method("thread/unsubscribe"))
		if fakeproc.Str(unsub, "params", "threadId") != thread {
			t.Errorf("unsubscribe = %v", unsub)
		}
		h.Reply(unsub, map[string]any{})

		// A paginated thread whose revert fails forks too.
		read = h.RecvWhere(fakeproc.Method("thread/read"))
		if fakeproc.Str(read, "params", "threadId") != "th-9" {
			t.Errorf("read = %v", read)
		}
		h.Reply(read, map[string]any{"thread": map[string]any{"id": "th-9", "historyMode": "paginated"}})
		revert := h.RecvWhere(fakeproc.Method("thread/revert"))
		rpcError(h, revert, -32600, "cannot revert", nil)
		fork = h.RecvWhere(fakeproc.Method("thread/fork"))
		if fakeproc.Str(fork, "params", "threadId") != "th-9" || fakeproc.Str(fork, "params", "beforeTurnId") != "t1" {
			t.Errorf("second rewind fork = %v", fork)
		}
		h.Reply(fork, map[string]any{"thread": map[string]any{"id": "th-10"}})
		h.Reply(h.RecvWhere(fakeproc.Method("thread/unsubscribe")), map[string]any{})

		ts := h.RecvWhere(fakeproc.Method("turn/start"))
		if fakeproc.Str(ts, "params", "threadId") != "th-10" {
			t.Errorf("turn/start after rewind = %v", ts)
		}
		close(scripted)
		// Answer the unsubscribe and interrupt requests of Close.
		for m := h.Recv(); m != nil; m = h.Recv() {
			if m["id"] != nil {
				h.Reply(m, map[string]any{})
			}
		}
	})
	ctx := context.Background()
	cfg := agent.Config{
		SystemPrompt: "base", AppendSystemPrompt: "dev", Effort: agent.EffortHigh, PermissionMode: agent.PermissionReadOnly,
		MCPServers: map[string]agent.MCPServer{"m": {Command: "srv", EnabledTools: []string{"a"}, ApprovedTools: []string{"a"}, AlwaysLoad: true}},
	}
	p := codex.New(codex.Options{Spawner: spawner, SharedServer: true})
	defer p.Close()
	sess, err := p.StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	forked, err := sess.ForkSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer forked.Close()

	var asks []*agent.UserInputRequest
	for _, text := range []string{"one", "two"} {
		if _, err := sess.Send(ctx, agent.Text(text)); err != nil {
			t.Fatal(err)
		}
		collect(t, sess, func(ev agent.Event) bool {
			if r, ok := ev.(*agent.UserInputRequest); ok {
				asks = append(asks, r)
			}
			return isTurnCompleted(ev)
		})
	}
	if len(asks) != 2 {
		t.Fatalf("async questions = %d", len(asks))
	}
	if err := sess.Rewind(ctx, "t2"); err != nil {
		t.Fatal(err)
	}
	if sess.ID() != "th-9" || sess.Info().ID != "th-9" {
		t.Errorf("id after rewind = %s / %s", sess.ID(), sess.Info().ID)
	}
	waitDone(t, asks[1], true)
	waitDone(t, asks[0], false)
	collect(t, sess, func(ev agent.Event) bool {
		u, ok := ev.(*agent.SessionUpdated)
		return ok && u.Info.ID == "th-9"
	})
	if err := sess.Rewind(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if sess.ID() != "th-10" {
		t.Errorf("id after second rewind = %s", sess.ID())
	}
	go sess.Send(ctx, agent.Text("after"))
	waitFor(t, scripted)
}

func TestSteerRejectionAsyncAnswersAndStrictSchema(t *testing.T) {
	scripted := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		ts := startTurn(h, "t1")
		schema := fakeproc.Get(ts, "params", "outputSchema")
		if fakeproc.Get(schema, "additionalProperties") != false || fakeproc.Str(schema, "required", 0) != "b" || fakeproc.Str(schema, "required", 1) != "a" {
			t.Errorf("output schema = %v", schema)
		}
		item(h, "item/completed", "t1", map[string]any{"type": "agentMessage", "id": "a1", "text": "Which?", "delivery": "async",
			"questions": []any{map[string]any{"title": "Pick a color", "options": []any{"red", "blue"}}}})
		steer := h.RecvWhere(fakeproc.Method("turn/steer"))
		if fakeproc.Str(steer, "params", "input", 0, "text") != "blue" || fakeproc.Str(steer, "params", "expectedTurnId") != "t1" {
			t.Errorf("answer steer = %v", steer)
		}
		rpcError(h, steer, -32600, "turn is not steerable", map[string]any{"codexErrorInfo": map[string]any{"activeTurnNotSteerable": map[string]any{"turnKind": "review"}}})
		turnCompleted(h, "t1", "completed", nil)

		answer := startTurn(h, "t2")
		if fakeproc.Str(answer, "params", "input", 0, "text") != "blue" {
			t.Errorf("queued answer = %v", answer)
		}
		steer = h.RecvWhere(fakeproc.Method("turn/steer"))
		rpcError(h, steer, -32600, "expected active turn id `t2` but found `t3`", nil)
		steer = h.RecvWhere(fakeproc.Method("turn/steer"))
		rpcError(h, steer, -32600, "no active turn to steer", nil)
		item(h, "item/completed", "t2", map[string]any{"type": "agentMessage", "id": "a2", "text": "Which?", "delivery": "async",
			"questions": []any{map[string]any{"title": "Pick", "options": []any{"x"}}}})
		turnCompleted(h, "t2", "interrupted", nil)

		late := startTurn(h, "t3")
		if fakeproc.Str(late, "params", "input", 0, "text") != "late" {
			t.Errorf("late turn = %v", late)
		}
		turnCompleted(h, "t3", "completed", nil)
		close(scripted)
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	in := agent.Text("go")
	in.OutputSchema = json.RawMessage(`{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"number"}},"required":["b"]}`)
	if _, err := sess.Send(ctx, in); err != nil {
		t.Fatal(err)
	}
	var ask *agent.UserInputRequest
	collect(t, sess, func(ev agent.Event) bool {
		ask, _ = ev.(*agent.UserInputRequest)
		return ask != nil
	})
	if err := ask.Answer(ctx, map[string][]string{ask.Questions[0].ID: {"blue"}}); err != nil {
		t.Fatal(err)
	}
	collect(t, sess, func(ev agent.Event) bool {
		s, ok := ev.(*agent.TurnStarted)
		return ok && s.TurnID == "t2"
	})
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("x")}, Delivery: agent.DeliverSteer})
	var perr *agent.ProtocolError
	if !errors.Is(err, agent.ErrSteerRejected) || !errors.As(err, &perr) {
		t.Errorf("rejected steer = %v", err)
	}
	if _, err := sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("late")}, Delivery: agent.DeliverSteer}); err != nil {
		t.Errorf("steer after the turn ended = %v", err)
	}
	var second *agent.UserInputRequest
	collect(t, sess, func(ev agent.Event) bool {
		if r, ok := ev.(*agent.UserInputRequest); ok {
			second = r
		}
		return isTurnCompleted(ev)
	})
	if second == nil {
		t.Fatal("second async question missing")
	}
	waitDone(t, second, true)
	waitFor(t, scripted)
}

func TestStrictOutputSchema(t *testing.T) {
	in := `{"type":"object","properties":{"z":{"type":"object","properties":{"k":{"type":"string"}}},"list":{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}}}},"pick":{"anyOf":[{"$ref":"#/$defs/item"},{"type":"null"}]}},"$defs":{"item":{"properties":{"id":{"type":"string"}}}}}`
	out, err := codex.StrictOutputSchema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"object","properties":{"z":{"type":"object","properties":{"k":{"type":"string"}},"additionalProperties":false,"required":["k"]},"list":{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false,"required":["n"]}},"pick":{"anyOf":[{"$ref":"#/$defs/item"},{"type":"null"}]}},"$defs":{"item":{"properties":{"id":{"type":"string"}},"additionalProperties":false,"required":["id"]}},"additionalProperties":false,"required":["z","list","pick"]}`
	if string(out) != want {
		t.Errorf("strict schema =\n%s\nwant\n%s", out, want)
	}
	if _, err := codex.StrictOutputSchema(json.RawMessage(`{"type":"object","additionalProperties":true}`)); err == nil || !strings.Contains(err.Error(), "$ must set additionalProperties to false") {
		t.Errorf("open object error = %v", err)
	}
	if _, err := codex.StrictOutputSchema(json.RawMessage(`{"type":"string"}`)); err == nil {
		t.Error("string root accepted")
	}
}

func TestListingHistoryAndSubagents(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		h.Reply(h.RecvWhere(fakeproc.Method("initialize")), map[string]any{"userAgent": "fake"})
		row := func(id string) map[string]any {
			return map[string]any{"id": id, "preview": "hello " + id, "cwd": "/w", "createdAt": 1, "updatedAt": 2, "model": "gpt-x", "reasoningEffort": "low"}
		}
		childTurn := map[string]any{"id": "ct1", "status": "completed", "startedAt": 1700000000, "completedAt": 1700000060, "items": []any{
			map[string]any{"type": "userMessage", "id": "u1", "clientId": "client-7", "content": []any{map[string]any{"type": "text", "text": "dig"}}},
			map[string]any{"type": "collabAgentToolCall", "id": "spawn1", "tool": "spawnAgent", "status": "completed", "senderThreadId": child,
				"receiverThreadIds": []any{"gc-1"}, "prompt": "deeper", "agentsStates": map[string]any{"gc-1": map[string]any{"status": "running"}}},
			map[string]any{"type": "collabAgentToolCall", "id": "msg1", "tool": "sendMessage", "status": "completed", "senderThreadId": child,
				"receiverThreadIds": []any{thread}, "agentsStates": map[string]any{}},
		}}
		lists := 0
		for {
			m := h.Recv()
			if m == nil {
				return
			}
			params := m["params"]
			switch fakeproc.Str(m, "method") {
			case "thread/list":
				lists++
				if fakeproc.Str(params, "modelProviders", 0) != "custom" {
					t.Errorf("list = %v", m)
				}
				switch fakeproc.Str(params, "cursor") {
				case "":
					h.Reply(m, map[string]any{"data": []any{row("a"), row("b")}, "nextCursor": "c1"})
				case "c1":
					h.Reply(m, map[string]any{"data": []any{row("b"), row("c")}, "nextCursor": "c2"})
				default:
					h.Reply(m, map[string]any{"data": []any{row("c")}, "nextCursor": "c3"})
				}
			case "thread/turns/list":
				if fakeproc.Str(params, "threadId") != child || fakeproc.Str(params, "sortDirection") != "desc" || fakeproc.Str(params, "itemsView") != "full" {
					t.Errorf("turns list = %v", m)
				}
				h.Reply(m, map[string]any{"data": []any{childTurn}, "nextCursor": nil})
			case "thread/delete":
				if fakeproc.Str(params, "threadId") == "gone" {
					rpcError(h, m, -32600, "no rollout found for thread id gone", nil)
				} else {
					h.Reply(m, map[string]any{})
				}
			case "thread/read":
				var turns []any
				switch fakeproc.Str(params, "threadId") {
				case thread:
					turns = []any{map[string]any{"id": "t1", "status": "completed", "items": []any{
						map[string]any{"type": "subAgentActivity", "id": "act1", "kind": "started", "agentThreadId": child, "agentPath": "/root/worker"},
					}}}
				case child:
					turns = []any{childTurn}
				default:
					turns = []any{}
				}
				h.Reply(m, map[string]any{"thread": map[string]any{"id": fakeproc.Str(params, "threadId"), "turns": turns}})
			default:
				if m["id"] != nil {
					h.Reply(m, map[string]any{})
				}
			}
		}
	})
	ctx := context.Background()
	p := codex.New(codex.Options{Spawner: spawner, SharedServer: true, Config: map[string]string{"model_provider": `"custom"`}})
	defer p.Close()
	sums, err := p.ListSessions(ctx, agent.ListSessionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 3 || sums[2].ID != "c" || sums[0].Model != "gpt-x" || sums[0].Effort != agent.EffortLow || sums[0].FirstPrompt != "hello a" {
		t.Errorf("sessions = %+v", sums)
	}
	page, err := p.ListSessions(ctx, agent.ListSessionsOptions{Offset: 1, Limit: 1})
	if err != nil || len(page) != 1 || page[0].ID != "b" {
		t.Errorf("sessions page = %+v, %v", page, err)
	}
	if err := p.DeleteSession(ctx, "a", ""); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := p.DeleteSession(ctx, "gone", ""); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Errorf("delete missing = %v", err)
	}
	last, err := p.ReadHistory(ctx, thread, agent.ReadHistoryOptions{SubagentID: child, Limit: 1, Offset: 1})
	if err != nil || len(last) != 1 || last[0].ID != "spawn1" {
		t.Errorf("history page = %+v, %v", last, err)
	}
	items, err := p.ReadHistory(ctx, thread, agent.ReadHistoryOptions{SubagentID: child})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].ClientID != "client-7" || items[0].TurnID != "ct1" || items[0].SubagentID != child || items[0].ParentID != "act1" ||
		items[0].Time.Unix() != 1700000000 || items[1].Time.Unix() != 1700000060 {
		t.Errorf("history = %+v", items)
	}
	tasks, err := p.ListSubagents(ctx, thread, agent.ListSubagentsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].ID != child || tasks[0].Subagent.Depth != 1 || tasks[0].ParentID != "" || tasks[0].Status != agent.TaskCompleted ||
		tasks[0].Subagent.Type != "worker" || tasks[1].ID != "gc-1" || tasks[1].Subagent.Depth != 2 || tasks[1].ParentID != child ||
		tasks[1].Subagent.Prompt != "deeper" || tasks[1].ToolCallID != "spawn1" {
		t.Errorf("subagents = %+v", tasks)
	}
}

func TestNestedSubagentsImagesAndTerminalInput(t *testing.T) {
	scripted := make(chan struct{})
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		startTurn(h, "t1")
		item(h, "item/completed", "t1", map[string]any{"type": "subAgentActivity", "id": "act1", "kind": "started", "agentThreadId": child, "agentPath": "/root/w"})
		childNotify(h, "item/completed", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "collabAgentToolCall", "id": "spawn1", "tool": "spawnAgent",
			"status": "completed", "senderThreadId": child, "receiverThreadIds": []any{"gc-1"}, "prompt": "deeper", "model": "gpt-mini",
			"agentsStates": map[string]any{"gc-1": map[string]any{"status": "running"}}}})
		h.Notify("item/completed", map[string]any{"threadId": "gc-1", "turnId": "gt1", "item": map[string]any{"type": "agentMessage", "id": "gm1", "text": "found it"}})
		item(h, "item/completed", "t1", map[string]any{"type": "mcpToolCall", "id": "mcp1", "server": "s", "tool": "shot", "status": "completed",
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}, map[string]any{"type": "image", "data": "aGk=", "mimeType": "image/jpeg"}}}})
		item(h, "item/completed", "t1", map[string]any{"type": "imageGeneration", "id": "img1", "status": "completed", "result": "", "savedPath": "/w/cat.png", "revisedPrompt": "a cat"})
		item(h, "item/completed", "t1", map[string]any{"type": "imageGeneration", "id": "img2", "status": "failed", "result": "",
			"failure": map[string]any{"type": "usageLimitExceeded", "limitId": "images"}})
		item(h, "item/completed", "t1", map[string]any{"type": "userMessage", "id": "u1", "clientId": "c-1", "content": []any{}})
		h.Notify("item/commandExecution/terminalInteraction", map[string]any{"threadId": thread, "turnId": "t1", "itemId": "c1", "processId": "pty-1", "stdin": "y\n"})
		turnCompleted(h, "t1", "completed", nil)
		close(scripted)
		h.WaitClosed()
	})
	ctx := context.Background()
	p := codex.New(codex.Options{Spawner: spawner, ClientName: "codex_app_server_daemon", UnsetEnv: []string{"SHARED_X"}})
	sess, err := p.StartSession(ctx, agent.Config{UnsetEnv: []string{"CLAUDECODE"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	h := <-started
	if strings.Join(h.Cmd.UnsetEnv, ",") != "SHARED_X,CLAUDECODE" {
		t.Errorf("unset env = %v", h.Cmd.UnsetEnv)
	}
	for _, kv := range h.Cmd.Env {
		if strings.HasPrefix(kv, "CODEX_INTERNAL_ORIGINATOR_OVERRIDE=") {
			t.Errorf("originator set for the daemon client: %s", kv)
		}
	}
	if _, err := sess.Send(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]agent.Task{}
	items := map[string]agent.Item{}
	var terminal *codex.TerminalInteraction
	collect(t, sess, func(ev agent.Event) bool {
		switch ev := ev.(type) {
		case *agent.TaskUpdated:
			tasks[ev.Task.ID] = ev.Task
		case *agent.ItemCompleted:
			items[ev.Item.ID] = ev.Item
		case *codex.TerminalInteraction:
			terminal = ev
		}
		return isTurnCompleted(ev)
	})
	gc := tasks["gc-1"]
	if gc.ParentID != child || gc.Subagent == nil || gc.Subagent.Depth != 2 || gc.Subagent.Prompt != "deeper" || gc.Subagent.Model != "gpt-mini" || gc.ToolCallID != "spawn1" {
		t.Errorf("grandchild task = %+v", gc)
	}
	if msg := items["gm1"]; msg.SubagentID != "gc-1" || msg.ParentID != "spawn1" {
		t.Errorf("grandchild item = %+v", msg)
	}
	if mcp := items["mcp1"].Tool; mcp == nil || mcp.Output != "ok" || len(mcp.Images) != 1 || string(mcp.Images[0].Data) != "hi" || mcp.Images[0].MediaType != "image/jpeg" {
		t.Errorf("mcp item = %+v", items["mcp1"].Tool)
	}
	if img := items["img1"]; img.Tool == nil || img.Tool.Name != "imageGeneration" || img.Tool.Path != "/w/cat.png" || len(img.Tool.Images) != 1 || img.Tool.IsError {
		t.Errorf("image item = %+v", img)
	}
	if img := items["img2"]; img.Status != agent.ItemFailed || !img.Tool.IsError || img.Tool.Output != "usageLimitExceeded" {
		t.Errorf("failed image item = %+v", img)
	}
	if u := items["u1"]; u.ClientID != "c-1" || u.TurnID != "t1" {
		t.Errorf("user message = %+v", u)
	}
	if terminal == nil || terminal.ItemID != "c1" || terminal.ProcessID != "pty-1" || terminal.Stdin != "y\n" {
		t.Errorf("terminal interaction = %+v", terminal)
	}
	waitFor(t, scripted)
}
