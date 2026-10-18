package pi_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// argAfter returns the argument that follows flag.
func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestStartArgsToolsAndCrossDirectoryResume(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "--elsewhere--", "2026_s-1.jsonl")
	writeLines(t, file, map[string]any{"type": "session", "id": "s-1", "cwd": "/elsewhere"})
	add := agent.Tool{Name: "add", Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) {
		return agent.TextResult("x"), nil
	}}
	for _, c := range []struct {
		tools []string
		want  string
	}{
		{[]string{"read"}, "read,add,structured_output"},
		{[]string{"+read", "-bash"}, "+read,-bash,+add,+structured_output"},
	} {
		spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
			if startup(h) {
				h.WaitClosed()
			}
		})
		p := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts", SessionDir: dir})
		sess, err := p.StartSession(context.Background(), agent.Config{Cwd: t.TempDir(), Resume: "s-1", Tools: []agent.Tool{add},
			ProviderOptions: &pi.SessionOptions{Tools: c.tools}})
		if err != nil {
			t.Fatal(err)
		}
		h := <-started
		if got := argAfter(h.Cmd.Args, "--tools"); got != c.want {
			t.Errorf("--tools %s, want %s", got, c.want)
		}
		if got := argAfter(h.Cmd.Args, "--session"); got != file {
			t.Errorf("--session %s, want the session file %s", got, file)
		}
		sess.Close()
	}
	_, err := pi.New(pi.Options{Spawner: agent.SpawnFunc(func(context.Context, agent.Command) (agent.Process, error) {
		t.Fatal("started with NoTools and host tools")
		return nil, nil
	})}).StartSession(context.Background(), agent.Config{Tools: []agent.Tool{add}, ProviderOptions: &pi.SessionOptions{NoTools: true}})
	if err == nil {
		t.Error("NoTools with host tools accepted")
	}
	if _, err := pi.New(pi.Options{}).StartSession(context.Background(), agent.Config{Tools: []agent.Tool{{Name: "structured_output"}}}); err == nil {
		t.Error("reserved tool name accepted")
	}
}

func TestInterruptWhilePromptStarts(t *testing.T) {
	gotPrompt, interrupted := make(chan struct{}), make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		prompt := h.RecvWhere(isCommand("prompt"))
		close(gotPrompt)
		<-interrupted
		respond(h, prompt, map[string]any{"disposition": "started"})
		// The abort follows the reply; nothing was sent while the prompt
		// started.
		if next := h.Recv(); fakeproc.Str(next, "type") != "abort" {
			t.Errorf("after the started reply got %v, want abort", next)
		} else {
			respond(h, next, nil)
		}
		h.Send(map[string]any{"type": "agent_start"})
		h.Send(map[string]any{"type": "agent_settled", "aborted": true})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	type sent struct {
		turn *agent.Turn
		err  error
	}
	done := make(chan sent, 1)
	go func() {
		turn, err := sess.Send(ctx, agent.Text("go"))
		done <- sent{turn, err}
	}()
	<-gotPrompt
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	close(interrupted)
	s := <-done
	if s.err != nil {
		t.Fatal(s.err)
	}
	res, err := s.turn.Wait(ctx)
	if !errors.Is(err, agent.ErrInterrupted) || res.Status != agent.TurnInterrupted {
		t.Errorf("result %+v, %v", res, err)
	}
}

func TestStructuredOutputClientIDAndHistoryDetails(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		prompt := h.RecvWhere(isCommand("prompt"))
		respond(h, prompt, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		// The bridge stripped the schema line before Pi recorded the
		// message.
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "user", "content": "answer", "timestamp": 1}})
		h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "out", "toolName": "structured_output",
			"result": map[string]any{"content": []any{}, "details": map[string]any{"ok": true}}})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var tr agent.Transcript
	go func() {
		for ev := range sess.Events() {
			tr.Apply(ev)
		}
	}()
	res, err := sess.Run(ctx, agent.Input{Parts: []agent.Part{{Type: agent.PartText, Text: "answer"}}, ClientID: "c-1",
		OutputSchema: json.RawMessage(`{"type":"object"}`)})
	if err != nil || string(res.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("result %+v, %v", res, err)
	}
	found := false
	for deadline := time.Now().Add(2 * time.Second); !found && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		found = slices.ContainsFunc(tr.Items(), func(it agent.Item) bool { return it.Kind == agent.ItemUserMessage && it.ClientID == "c-1" })
	}
	if !found {
		t.Errorf("no user item with the ClientID in %+v", tr.Items())
	}

	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, "2026_h-1.jsonl"),
		map[string]any{"type": "session", "id": "h-1", "cwd": dir},
		map[string]any{"type": "message", "id": "u1", "parentId": nil, "message": map[string]any{"role": "user", "content": "go"}},
		map[string]any{"type": "message", "id": "a1", "parentId": "u1", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "toolCall", "id": "b1", "name": "bash", "arguments": map[string]any{"command": "false"}}}}},
		map[string]any{"type": "message", "id": "r1", "parentId": "a1", "message": map[string]any{"role": "toolResult", "toolCallId": "b1", "toolName": "bash",
			"isError": true, "content": []any{map[string]any{"type": "text", "text": "exit 1"}}, "details": map[string]any{"exitCode": 1}}},
	)
	items, err := pi.New(pi.Options{SessionDir: dir}).ReadHistory(ctx, "h-1", agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(items, func(it agent.Item) bool { return it.Tool != nil })
	if i < 0 || items[i].Tool.ExitCode == nil || *items[i].Tool.ExitCode != 1 || !strings.Contains(string(items[i].Tool.OutputData), "exitCode") {
		t.Errorf("history tool item = %+v", items)
	}
}
