package pi_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func TestOldSessionFormatsAndFlatSessionDir(t *testing.T) {
	ctx := context.Background()
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	// A configured directory is flat even when it is the default one.
	dir := filepath.Join(agentDir, "sessions")
	cwd := t.TempDir()
	writeLines(t, filepath.Join(dir, "2024_v1.jsonl"),
		map[string]any{"type": "session", "id": "v1", "cwd": cwd},
		map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": "old prompt"}},
		map[string]any{"type": "message", "message": map[string]any{"role": "hookMessage", "content": "hook note", "display": true}},
		map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "old answer"}}}},
	)
	p := pi.New(pi.Options{SessionDir: dir})
	sums, err := p.ListSessions(ctx, agent.ListSessionsOptions{Cwd: cwd})
	if err != nil || len(sums) != 1 {
		t.Fatalf("sessions = %+v, %v", sums, err)
	}
	items, err := p.ReadHistory(ctx, "v1", agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range items {
		texts = append(texts, it.Text)
	}
	if !slices.Equal(texts, []string{"old prompt", "hook note", "old answer"}) {
		t.Errorf("v1 history = %q", texts)
	}
}

func TestListModelsSkipsMCP(t *testing.T) {
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		models := h.RecvWhere(isCommand("get_available_models"))
		respond(h, models, map[string]any{"models": []any{}})
		h.WaitClosed()
	})
	if _, err := pi.New(pi.Options{Spawner: spawner}).ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := <-started; !slices.Contains(h.Cmd.Args, "--no-mcp") {
		t.Errorf("model listing args %v start MCP servers", h.Cmd.Args)
	}
}

// A session switch holds the slot: input sent meanwhile waits for the
// switch, and a switch while a turn runs is refused.
func TestSessionSwitchHoldsSlot(t *testing.T) {
	gotSwitch, release := make(chan struct{}), make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		sw := h.RecvWhere(isCommand("new_session"))
		close(gotSwitch)
		<-release
		respond(h, sw, map[string]any{"cancelled": false})
		st := h.Recv()
		if fakeproc.Str(st, "type") != "get_state" {
			t.Errorf("after new_session got %v, want get_state before any prompt", st)
		}
		respond(h, st, map[string]any{"sessionId": "pi-2"})
		cmds := h.RecvWhere(isCommand("get_commands"))
		respond(h, cmds, map[string]any{"commands": []any{}})
		prompt := h.RecvWhere(isCommand("prompt"))
		respond(h, prompt, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	switched := make(chan error, 1)
	go func() {
		_, err := sess.NewSession(ctx, "")
		switched <- err
	}()
	<-gotSwitch
	turn, err := sess.Send(ctx, agent.Text("after"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Clone(ctx); err == nil {
		t.Error("a second switch was accepted while the first holds the slot")
	}
	close(release)
	if err := <-switched; err != nil {
		t.Fatal(err)
	}
	if res, err := turn.Wait(ctx); err != nil || res.Status != agent.TurnSucceeded || sess.ID() != "pi-2" {
		t.Errorf("queued turn %+v, %v, session %s", res, err, sess.ID())
	}
}

// A resumed session file runs in the directory its header records, as Pi
// does, whatever Config.Cwd says.
func TestResumeFollowsSessionFileCwd(t *testing.T) {
	file := filepath.Join(t.TempDir(), "2026_r-1.jsonl")
	writeLines(t, file, map[string]any{"type": "session", "id": "r-1", "cwd": "/elsewhere"})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		st := h.RecvWhere(isCommand("get_state"))
		if st == nil {
			return
		}
		respond(h, st, map[string]any{"sessionId": "r-1", "sessionFile": file})
		cmds := h.RecvWhere(isCommand("get_commands"))
		respond(h, cmds, map[string]any{"commands": []any{}})
		h.WaitClosed()
	})
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(context.Background(), agent.Config{Cwd: t.TempDir(), Resume: file})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if cwd := sess.Info().Cwd; cwd != "/elsewhere" {
		t.Errorf("Info.Cwd = %q, want the session file's /elsewhere", cwd)
	}
}

func TestDontAskRunsReadOnlyTools(t *testing.T) {
	answered := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		token := ""
		for _, kv := range h.Cmd.Env {
			if v, ok := strings.CutPrefix(kv, "AGENT_CLI_BRIDGE="); ok {
				var cfg struct{ Token string }
				_ = json.Unmarshal([]byte(v), &cfg)
				token = cfg.Token
			}
		}
		if a := bridge(h, token, "g1", map[string]any{"kind": "permission", "toolName": "read", "toolCallId": "c1", "input": map[string]any{"path": "a"}}); a["allow"] != true {
			t.Errorf("dontAsk read answer = %v", a)
		}
		if a := bridge(h, token, "g2", map[string]any{"kind": "permission", "toolName": "bash", "toolCallId": "c2", "input": map[string]any{"command": "ls"}}); a["allow"] != false {
			t.Errorf("dontAsk bash answer = %v", a)
		}
		close(answered)
		h.WaitClosed()
	})
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(context.Background(), agent.Config{PermissionMode: agent.PermissionDontAsk})
	if err != nil {
		t.Fatal(err)
	}
	<-answered
	sess.Close()
}

func TestIgnoredConfigNotice(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startup(h) {
			h.WaitClosed()
		}
	})
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(context.Background(), agent.Config{AdditionalDirectories: []string{"/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	first, second := <-sess.Events(), <-sess.Events()
	n, ok := second.(*agent.Notice)
	if _, started := first.(*agent.SessionStarted); !started || !ok || n.Code != "config_ignored" || !strings.Contains(n.Message, "AdditionalDirectories") {
		t.Errorf("first events %T, %+v", first, second)
	}
}
