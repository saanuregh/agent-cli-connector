package pi_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func TestPermissionModeNeedsLoadedBridge(t *testing.T) {
	idle := func(h *fakeproc.Harness) {
		if startup(h) {
			h.WaitClosed()
		}
	}
	ctx := context.Background()
	for _, mode := range []agent.PermissionMode{agent.PermissionReadOnly, agent.PermissionDefault} {
		spawner, _ := fakeproc.Spawner(t, idle)
		_, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{PermissionMode: mode})
		if !errors.Is(err, agent.ErrUnsupported) {
			t.Errorf("mode %s without the bridge: err = %v", mode, err)
		}
	}
	spawner, _ := fakeproc.Spawner(t, idle)
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{PermissionMode: agent.PermissionBypass})
	if err != nil {
		t.Fatalf("bypassPermissions without the bridge: %v", err)
	}
	sess.Close()

	spawner, _ = fakeproc.Spawner(t, idle)
	_, err = pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, agent.Config{PermissionMode: "bogus"})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("unknown mode: err = %v", err)
	}

	// The bridge did not load: its rewind command is missing.
	spawner, _ = fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startupWith(h, baseState(), []any{}) {
			h.WaitClosed()
		}
	})
	_, err = pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, agent.Config{PermissionMode: agent.PermissionReadOnly})
	if err == nil || !strings.Contains(err.Error(), "bridge extension did not load") {
		t.Errorf("readOnly with an unloaded bridge: err = %v", err)
	}
}

func TestGateModeOutranksGrantsAndAllowedTools(t *testing.T) {
	type step struct {
		tool  string
		input map[string]any
		allow bool
	}
	phase := make(chan []step)
	results := make(chan []bool)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		token := tokenOf(t, h)
		n := 0
		for steps := range phase {
			var got []bool
			for _, s := range steps {
				n++
				id := "g" + string(rune('a'+n))
				answer := bridge(h, token, id, map[string]any{"kind": "permission", "toolName": s.tool, "toolCallId": id, "input": s.input})
				got = append(got, answer["allow"] == true)
			}
			results <- got
		}
		h.WaitClosed()
	})
	asked := make(chan string, 16)
	cfg := agent.Config{
		PermissionMode: agent.PermissionDefault,
		AllowedTools:   []string{"bash"},
		Tools: []agent.Tool{{Name: "add", Handler: func(ctx context.Context, call agent.ToolCallRequest) (agent.ToolResult, error) {
			return agent.TextResult("5"), nil
		}}},
		Handlers: agent.Handlers{Permission: func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			asked <- req.ToolName
			return agent.PermissionResponse{Decision: agent.DecisionAllowSession}
		}},
	}
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	run := func(name string, steps []step) {
		t.Helper()
		phase <- steps
		got := <-results
		for i, s := range steps {
			if got[i] != s.allow {
				t.Errorf("%s: %s %v allowed = %v, want %v", name, s.tool, s.input, got[i], s.allow)
			}
		}
	}
	edit := func(path string) map[string]any { return map[string]any{"path": path, "oldText": "a", "newText": "b"} }
	run("default", []step{
		{"edit", edit("a.go"), true}, // asks, granted for the session
		{"edit", edit("a.go"), true}, // the grant
		{"bash", map[string]any{"command": "rm -rf /"}, true},
		{"add", map[string]any{}, true},
	})
	if len(asked) != 1 {
		t.Errorf("asked %d times, want once for the first edit", len(asked))
	}
	if err := sess.SetPermissionMode(ctx, agent.PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	notReadOnly := []step{
		{"edit", edit("a.go"), false},
		{"bash", map[string]any{"command": "ls"}, false},
		{"add", map[string]any{}, false},
		{"read", map[string]any{"path": "a.go"}, true},
	}
	run("readOnly", notReadOnly)
	if err := sess.SetPermissionMode(ctx, agent.PermissionPlan); err != nil {
		t.Fatal(err)
	}
	run("plan", notReadOnly)
	if err := sess.SetPermissionMode(ctx, agent.PermissionDontAsk); err != nil {
		t.Fatal(err)
	}
	run("dontAsk", []step{
		{"edit", edit("a.go"), false},
		{"add", map[string]any{}, false},
		{"bash", map[string]any{"command": "ls"}, true}, // in AllowedTools
	})
	if len(asked) != 1 {
		t.Errorf("asked %d times in total, want once", len(asked))
	}
	close(phase)
}

func TestAllowSessionGrantIsNarrowAndEndsWithSession(t *testing.T) {
	phase := make(chan []map[string]any)
	results := make(chan []bool)
	switched := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		token := tokenOf(t, h)
		n := 0
		ask := func(input map[string]any) bool {
			n++
			id := "g" + string(rune('a'+n))
			tool, _ := input["tool"].(string)
			return bridge(h, token, id, map[string]any{"kind": "permission", "toolName": tool, "toolCallId": id, "input": input})["allow"] == true
		}
		var got []bool
		for _, in := range <-phase {
			got = append(got, ask(in))
		}
		results <- got
		cmd := h.RecvWhere(isCommand("new_session"))
		respond(h, cmd, map[string]any{"cancelled": false})
		respond(h, h.RecvWhere(isCommand("get_state")), map[string]any{"sessionId": "pi-2"})
		respond(h, h.RecvWhere(isCommand("get_commands")), map[string]any{"commands": []any{map[string]any{"name": "agent-cli-rewind", "source": "extension"}}})
		<-switched
		got = nil
		for _, in := range <-phase {
			got = append(got, ask(in))
		}
		results <- got
		h.WaitClosed()
	})
	asked := make(chan string, 16)
	drain := func() []string {
		var out []string
		for len(asked) > 0 {
			out = append(out, <-asked)
		}
		return out
	}
	cfg := agent.Config{
		PermissionMode: agent.PermissionDefault,
		Handlers: agent.Handlers{Permission: func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			key := req.ToolName + " " + req.Command
			if len(req.Changes) == 1 {
				key += req.Changes[0].Path
			}
			asked <- key
			if strings.Contains(key, "deny") {
				return agent.PermissionResponse{Decision: agent.DecisionDeny}
			}
			return agent.PermissionResponse{Decision: agent.DecisionAllowSession}
		}},
	}
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	phase <- []map[string]any{
		{"tool": "bash", "command": "ls"},
		{"tool": "bash", "command": "ls"},       // granted
		{"tool": "bash", "command": "ls; deny"}, // another command asks
		{"tool": "write", "path": "a.txt"},      // asks
		{"tool": "write", "path": "a.txt"},      // granted
		{"tool": "write", "path": "deny/b.txt"}, // another path asks
		{"tool": "edit", "path": "a.txt"},       // another tool asks
	}
	if got, want := <-results, []bool{true, true, false, true, true, false, true}; !slices.Equal(got, want) {
		t.Errorf("answers = %v, want %v", got, want)
	}
	if got, want := drain(), []string{"bash ls", "bash ls; deny", "write a.txt", "write deny/b.txt", "edit a.txt"}; !slices.Equal(got, want) {
		t.Errorf("asked %q, want %q", got, want)
	}
	if _, err := sess.NewSession(ctx, ""); err != nil {
		t.Fatal(err)
	}
	close(switched)
	phase <- []map[string]any{{"tool": "bash", "command": "ls"}}
	<-results
	if got, want := drain(), []string{"bash ls"}; !slices.Equal(got, want) {
		t.Errorf("after NewSession asked %q, want %q", got, want)
	}
}
