package codex

import (
	"context"
	"fmt"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

// A caller that gives up after Codex accepted review/start must not
// abandon the review: its turn still runs and holds the running slot.
func TestReviewStartSurvivesCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStart(h) {
			return
		}
		rs := h.RecvWhere(fakeproc.Method("review/start"))
		cancel()
		time.Sleep(50 * time.Millisecond)
		h.Reply(rs, map[string]any{"turn": map[string]any{"id": "r1", "status": "inProgress", "items": []any{}}, "reviewThreadId": rThread})
		h.Notify("turn/started", map[string]any{"threadId": rThread, "turn": map[string]any{"id": "r1", "status": "inProgress", "items": []any{}}})
		rTurnDone(h, "r1", "completed")
		h.WaitClosed()
	})
	s, err := New(Options{Spawner: spawner}).StartSession(context.Background(), agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	turn, err := s.Review(ctx, ReviewUncommitted())
	if err != nil {
		t.Fatalf("Review after the caller canceled = %v, want the accepted review", err)
	}
	wait, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	res, err := turn.Wait(wait)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnID != "r1" {
		t.Errorf("review turn = %q, want r1", res.TurnID)
	}
}

// Closing a session with a running turn on a shared server receives the
// interrupt's turn/completed after the thread unregistered. Those held
// notifications must not outlive the thread and fill the orphan bounds.
func TestClosingSharedSessionsForgetsTheirThreads(t *testing.T) {
	const sessions = maxOrphanThreads + 8
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		threads := 0
		for {
			m := h.Recv()
			if m == nil {
				return
			}
			method, _ := m["method"].(string)
			thread := fakeproc.Str(m, "params", "threadId")
			switch {
			case m["id"] == nil:
			case method == "initialize":
				h.Reply(m, map[string]any{"userAgent": "fake", "codexHome": "/h", "platformFamily": "unix", "platformOs": "linux"})
			case method == "thread/start":
				threads++
				id := fmt.Sprintf("th-%d", threads)
				h.Reply(m, map[string]any{"thread": map[string]any{"id": id, "cwd": "/w"}, "model": "m", "cwd": "/w"})
			case method == "turn/start":
				turn := map[string]any{"id": "t-" + thread, "status": "inProgress", "items": []any{}}
				h.Reply(m, map[string]any{"turn": turn})
				h.Notify("turn/started", map[string]any{"threadId": thread, "turn": turn})
			case method == "turn/interrupt":
				h.Reply(m, map[string]any{})
				h.Notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": "t-" + thread, "status": "interrupted", "items": []any{}}})
			default:
				h.Reply(m, map[string]any{})
			}
		}
	})
	ctx := context.Background()
	p := New(Options{Spawner: spawner, SharedServer: true})
	defer p.Close()
	var client *Client
	for range sessions {
		s, err := p.StartSession(ctx, agent.Config{})
		if err != nil {
			t.Fatal(err)
		}
		client = s.client
		if _, err := s.Send(ctx, agent.Text("go")); err != nil {
			t.Fatal(err)
		}
		for {
			if id, starting := s.rt.Running(); id != "" && !starting {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	client.mu.Lock()
	held := len(client.orphans)
	client.mu.Unlock()
	if held != 0 {
		t.Errorf("orphan threads after closing %d sessions = %d, want 0", sessions, held)
	}
}
