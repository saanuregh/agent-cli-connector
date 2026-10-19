package claude_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

// A rewind restart of a claimed spare relaunches without --await-claim and
// keeps the settings the spare was prewarmed with.
func TestClaimedSpareRestartKeepsSettings(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const claimedID = "77777777-2222-4333-8444-555555555555"
	writeTranscript(t, configDir, cwd, claimedID, sampleTranscript()[:7])
	var mu sync.Mutex
	n := 0
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		if handshake(h) == nil {
			return
		}
		if k == 1 {
			req := h.RecvWhere(isControl("claim_session"))
			h.Send(controlResponse(fakeproc.Str(req, "request_id"), map[string]any{"status": "ok", "cwd": cwd, "session_id": claimedID}))
			refuseNativeRewind(h)
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	p := claude.New(claude.Options{Spawner: spawner})
	spare, err := p.Prewarm(ctx, agent.Config{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}, PermissionMode: agent.PermissionDefault})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	sess, err := spare.Claim(ctx, agent.Config{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	t.Logf("info permission mode after claim without mode: %q", sess.Info().PermissionMode)
	if err := sess.Rewind(ctx, "u2"); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	h := <-started
	t.Logf("restart args: %v", h.Cmd.Args)
	if slices.Contains(h.Cmd.Args, "--await-claim") {
		t.Errorf("restart of a claimed session passes --await-claim")
	}
	if !slices.ContainsFunc(h.Cmd.Args, func(a string) bool { return strings.HasPrefix(a, "--permission-mode") }) {
		t.Errorf("restart dropped the prewarm permission mode")
	}
}

func TestReinitializeDoesNotReaskAnsweredPrompt(t *testing.T) {
	answers := make(chan map[string]any, 4)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		live := map[string]any{"type": "control_request", "request_id": "p1",
			"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ls"}, "tool_use_id": "tu1"}}
		h.Send(live)
		init := h.RecvWhere(isControl("initialize"))
		// The host's answer to p1 crosses the CLI's initialize response.
		answers <- h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "p1" })
		h.Send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": fakeproc.Str(init, "request_id"),
			"response":                    map[string]any{},
			"pending_permission_requests": []any{live}}})
		answers <- h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "p1" })
		h.WaitClosed()
	})
	var mu sync.Mutex
	asked := 0
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{
		Handlers: agent.Handlers{Permission: func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			mu.Lock()
			asked++
			mu.Unlock()
			return agent.PermissionResponse{Decision: agent.DecisionAllow}
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	time.Sleep(50 * time.Millisecond)
	if _, err := sess.Reinitialize(ctx); err != nil {
		t.Fatal(err)
	}
	<-answers
	select {
	case <-answers:
		t.Errorf("an answered prompt was asked and answered again after Reinitialize")
	case <-time.After(300 * time.Millisecond):
	}
	mu.Lock()
	t.Logf("asked %d times", asked)
	mu.Unlock()
}

func TestSessionLookupRejectsPathIDs(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	outside := t.TempDir()
	writeTranscript(t, outside, cwd, "secret", sampleTranscript()[:3])
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	p := claude.New(claude.Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	// configDir/projects/<x>/../../../<outside>/projects/<proj>/secret
	os.MkdirAll(filepath.Join(configDir, "projects", "someproj"), 0o755)
	rel := strings.Repeat("../", 20) + outside[1:] + "/projects/*/secret"
	sum, err := p.SessionInfo(context.Background(), rel, "")
	t.Logf("SessionInfo(%q) = %+v, %v", rel, sum, err)
	if err == nil {
		t.Errorf("SessionInfo read a transcript outside the config dir")
	}
	items, err := p.ReadHistory(context.Background(), rel, agent.ReadHistoryOptions{})
	t.Logf("ReadHistory items=%d err=%v", len(items), err)
}

// Close between retiring the old process and storing the restarted one
// must not leave Rewind stopping a process whose output nobody reads.
func TestCloseDuringRewindRestartReturns(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "88888888-2222-4333-8444-555555555555"
	writeTranscript(t, configDir, cwd, id, sampleTranscript()[:7])
	inner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		refuseNativeRewind(h)
		h.WaitClosed()
	})
	var sess *claude.Session
	var spawns atomic.Int32
	spawner := agent.SpawnFunc(func(ctx context.Context, cmd agent.Command) (agent.Process, error) {
		proc, err := inner.Spawn(ctx, cmd)
		if spawns.Add(1) == 2 {
			_ = sess.Close()
		}
		return proc, err
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{
		Cwd: cwd, Resume: id, Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	done := make(chan error, 1)
	go func() { done <- sess.Rewind(ctx, "u2") }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("rewind succeeded on a closed session")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rewind did not return after Close")
	}
}

// A retry that reuses a ClientID gets a turn of its own, not the earlier
// turn's result.
func TestReusedClientIDStartsNewTurn(t *testing.T) {
	const clientID = "99999999-1111-4111-8111-111111111111"
	uuids := make(chan string, 2)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		for _, text := range []string{"first", "second"} {
			uuid := startTurn(h)
			uuids <- uuid
			h.Send(map[string]any{"type": "result", "subtype": "success", "result": text, "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	in := agent.Input{Parts: []agent.Part{agent.TextPart("hi")}, ClientID: clientID}
	first, err := sess.Send(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := first.Wait(waitCtx); err != nil || res.Text != "first" {
		t.Fatalf("first turn = %+v, %v", res, err)
	}
	second, err := sess.Send(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if second == first || second.ID() == first.ID() {
		t.Fatalf("retry reused turn %s", first.ID())
	}
	res, err := second.Wait(waitCtx)
	if err != nil || res.Text != "second" {
		t.Fatalf("second turn = %+v, %v", res, err)
	}
	if a, b := <-uuids, <-uuids; a != clientID || a == b {
		t.Errorf("message uuids = %s, %s; want %s then a fresh one", a, b, clientID)
	}
	log.wait(t, "retry user message", func(ev agent.Event) bool {
		c, ok := ev.(*agent.ItemCompleted)
		return ok && c.Item.Kind == agent.ItemUserMessage && c.Item.ID == second.ID() && c.Item.ClientID == clientID
	})
}

// A NoQuery message reports its user message before the turn completes.
func TestNoQueryReportsUserMessageInTurn(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	turn, err := sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("note")}, ProviderOptions: &claude.TurnOptions{NoQuery: true}})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := turn.Wait(waitCtx)
	if err != nil {
		t.Fatal(err)
	}
	isUserItem := func(ev agent.Event) bool {
		c, ok := ev.(*agent.ItemCompleted)
		return ok && c.Item.Kind == agent.ItemUserMessage && c.Item.Text == "note"
	}
	if !slices.ContainsFunc(res.Items, func(it agent.Item) bool { return it.Kind == agent.ItemUserMessage && it.Text == "note" }) {
		t.Errorf("result items = %+v; want the user message", res.Items)
	}
	if !slices.ContainsFunc(slices.Collect(turn.Events()), isUserItem) {
		t.Error("turn events lack the user message")
	}
	log.wait(t, "TurnCompleted", func(ev agent.Event) bool { _, ok := ev.(*agent.TurnCompleted); return ok })
	all := log.all()
	item := slices.IndexFunc(all, isUserItem)
	done := slices.IndexFunc(all, func(ev agent.Event) bool { _, ok := ev.(*agent.TurnCompleted); return ok })
	if item < 0 || item > done {
		t.Errorf("session events: user message at %d, TurnCompleted at %d", item, done)
	}
}

// A turn the CLI finishes at once still lists its user message.
func TestFastTurnIncludesUserMessage(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		uuid := startTurn(h)
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "ok", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	turn, err := sess.Send(ctx, agent.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := turn.Wait(waitCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Items, func(it agent.Item) bool { return it.Kind == agent.ItemUserMessage && it.Text == "hi" }) {
		t.Errorf("result items = %+v; want the user message", res.Items)
	}
}
