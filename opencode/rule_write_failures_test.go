package opencode_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Regression tests for the eighth review; the first is the reviewer's
// reproduction, asserting the fixed behavior.

// A rule write whose context ends after the server applied it still
// completes, so the reported mode and the server's rules agree, and the
// next mode change is written.
func TestAmbiguousRuleWriteKeepsModeAndRules(t *testing.T) {
	f := newFakeServer(t)
	var mu sync.Mutex
	var serverRules json.RawMessage
	slow := false
	f.on("PATCH /api/session/"+fakeSession, func(r *fakeRequest) (int, any) {
		mu.Lock()
		if p, ok := r.Body["permissions"]; ok {
			serverRules, _ = json.Marshal(p)
		}
		wait := slow
		mu.Unlock()
		if wait {
			time.Sleep(300 * time.Millisecond)
		}
		return 204, nil
	})
	sess, err := f.provider().StartSession(context.Background(), agent.Config{Cwd: t.TempDir(), PermissionMode: agent.PermissionDefault})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	mu.Lock()
	slow = true
	mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = sess.SetPermissionMode(ctx, agent.PermissionBypass)
	cancel()
	mu.Lock()
	slow = false
	got := string(serverRules)
	mu.Unlock()
	if err != nil || sess.Info().PermissionMode != agent.PermissionBypass {
		t.Fatalf("SetPermissionMode(bypass): %v; reported %q; server rules %s", err, sess.Info().PermissionMode, got)
	}
	before := f.count("PATCH", "/api/session/"+fakeSession)
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionDefault); err != nil {
		t.Fatal(err)
	}
	if f.count("PATCH", "/api/session/"+fakeSession) == before {
		t.Fatal("reported default while the server keeps the bypass rules")
	}
}

// A context that ended before the write sends nothing and changes no mode.
func TestCancelledRuleWriteChangesNothing(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sess.SetPermissionMode(ctx, agent.PermissionBypass); err == nil {
		t.Fatal("SetPermissionMode with a cancelled context succeeded")
	}
	if n := f.count("PATCH", "/api/session/"+fakeSession); n != 0 || sess.Info().PermissionMode != agent.PermissionDefault {
		t.Fatalf("%d rule writes; mode %q", n, sess.Info().PermissionMode)
	}
}

// A failed rule write is not trusted: the rollback writes the previous
// mode's rules again, so the server ends with the reported mode's rules.
func TestFailedRuleWriteRestoresRules(t *testing.T) {
	f := newFakeServer(t)
	rec := recordRules(t, f)
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	want := rec.get()
	failed := false
	f.on("PATCH /api/session/"+fakeSession, func(r *fakeRequest) (int, any) {
		if !failed {
			// The server applies the rules but the answer is an error.
			failed = true
			rec.mu.Lock()
			rec.rules = ruleList(t, r.Body["permissions"])
			rec.mu.Unlock()
			return 500, map[string]any{"name": "UnknownError", "message": "lost answer"}
		}
		rec.mu.Lock()
		rec.rules = ruleList(t, r.Body["permissions"])
		rec.mu.Unlock()
		return 204, nil
	})
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionBypass); err == nil {
		t.Fatal("SetPermissionMode succeeded")
	}
	if mode := sess.Info().PermissionMode; mode != agent.PermissionDefault || !slices.Equal(rec.get(), want) {
		t.Fatalf("mode %q with rules %v, want %v", mode, rec.get(), want)
	}
}

// A parent's mode change reaches the subagent sessions it already has.
func TestModeChangeRewritesSubagentRules(t *testing.T) {
	f := newFakeServer(t)
	var mu sync.Mutex
	var child []string
	f.on("PATCH /api/session/ses_child", func(r *fakeRequest) (int, any) {
		mu.Lock()
		child = ruleList(t, r.Body["permissions"])
		mu.Unlock()
		return 204, nil
	})
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionAcceptEdits})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "general"})
	// The new subagent session gets its own agent's rules.
	f.next("PATCH", "/api/session/ses_child")
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := slices.Clone(child)
	mu.Unlock()
	if effect(append([]string{"*:*=allow", "subagent:*=deny"}, got...), "edit", "src/a.go") != "deny" || !slices.Contains(got, "subagent:*=deny") {
		t.Fatalf("subagent rules after readOnly: %v", got)
	}
}

// Two AllowSession answers for one subagent at once both stay: the second
// arrives while the first one's rule write is on its way, and the server
// keeps the write that finishes last.
func TestConcurrentSubagentGrantsBothStay(t *testing.T) {
	f := newFakeServer(t)
	var mu sync.Mutex
	var child []string
	firstSent := make(chan struct{})
	var once sync.Once
	f.on("PATCH /api/session/ses_child", func(r *fakeRequest) (int, any) {
		rules := ruleList(t, r.Body["permissions"])
		if slices.Contains(rules, "shell:ls *=allow") && !slices.Contains(rules, "shell:pwd *=allow") {
			once.Do(func() { close(firstSent) })
			time.Sleep(150 * time.Millisecond)
		}
		mu.Lock()
		child = rules
		mu.Unlock()
		return 204, nil
	})
	f.on("POST /api/session/ses_child/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	send(t, sess, agent.Text("go"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "general"})
	// The new subagent session's own rules are written first.
	f.next("PATCH", "/api/session/ses_child")
	f.emit("permission.asked", map[string]any{"id": "per_1", "sessionID": "ses_child", "action": "shell", "resources": []any{"ls -la"}, "save": []any{"ls *"}})
	f.emit("permission.asked", map[string]any{"id": "per_2", "sessionID": "ses_child", "action": "shell", "resources": []any{"pwd"}, "save": []any{"pwd *"}})
	byID := func(id string) *agent.PermissionRequest {
		return ev.wait(t, func(e agent.Event) bool {
			r, ok := e.(*agent.PermissionRequest)
			return ok && r.ID == id
		}).(*agent.PermissionRequest)
	}
	first, second := byID("per_1"), byID("per_2")
	var wg sync.WaitGroup
	allow := func(r *agent.PermissionRequest) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.AllowForSession(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	allow(first)
	<-firstSent
	allow(second)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(child, "shell:ls *=allow") || !slices.Contains(child, "shell:pwd *=allow") {
		t.Fatalf("subagent rules = %v", child)
	}
}
