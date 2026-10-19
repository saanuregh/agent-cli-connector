package opencode_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Regression tests for the eleventh review; they port the reviewer's
// reproductions, asserting the fixed behavior.

// An agent selection that lands while a following session's write for a
// new default agent is in flight, naming the old default, gets rules for
// the selected agent.
func TestSelectionDuringFollowWrite(t *testing.T) {
	f := newFakeServer(t)
	agents := &switchableAgents{}
	agents.serve(f)
	var mu sync.Mutex
	var last []string
	var once sync.Once
	f.on("PATCH /api/session/"+fakeSession, func(r *fakeRequest) (int, any) {
		rules := ruleList(t, r.Body["permissions"])
		if slices.Contains(rules, "shell:*=deny") {
			once.Do(func() {
				// Another client selects build while the write is in flight.
				f.emit("session.agent.selected", map[string]any{"agent": "build"})
				time.Sleep(200 * time.Millisecond)
			})
		}
		mu.Lock()
		last = rules
		mu.Unlock()
		return 204, nil
	})
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	agents.set(true)
	send(t, sess, agent.Text("x"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		rules := slices.Clone(last)
		mu.Unlock()
		if slices.Contains(rules, "browser:*=deny") && !slices.Contains(rules, "shell:*=deny") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session pinned to build holds rules for locked: %v", rules)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Close waits for a background rule write in flight, which writes nothing
// more once Close started, subagents included.
func TestCloseWaitsForBackgroundRuleWrites(t *testing.T) {
	f := newFakeServer(t)
	agents := &switchableAgents{}
	agents.serve(f)
	l := logPatches(t, f, "ses_child")
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "general"})
	l.waitRules(t, "ses_child", func(r []string) bool { return len(r) > 0 })
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.on("PATCH /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		entered <- struct{}{}
		<-release
		return 204, nil
	})
	var mu sync.Mutex
	childAfter := 0
	closed := false
	f.on("PATCH /api/session/ses_child", func(*fakeRequest) (int, any) {
		mu.Lock()
		if closed {
			childAfter++
		}
		mu.Unlock()
		return 204, nil
	})
	agents.set(false, map[string]any{"action": "edit", "resource": "secrets/*", "effect": "deny"})
	emitLocated(f, "agent.updated", "/work")
	<-entered
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(release)
	}()
	mu.Lock()
	closed = true
	mu.Unlock()
	start := time.Now()
	sess.Close()
	if d := time.Since(start); d < 100*time.Millisecond || d > 5*time.Second {
		t.Fatalf("Close took %v", d)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if childAfter != 0 {
		t.Fatalf("subagent writes after Close started: %d", childAfter)
	}
}

// Rule changes after Close send nothing.
func TestRuleChangesAfterCloseSendNothing(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	sess.Close()
	before := f.count("PATCH", "/api/session/"+fakeSession)
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionBypass); err == nil {
		t.Error("SetPermissionMode after Close succeeded")
	}
	if err := sess.SetAgent(context.Background(), "plan"); err == nil {
		t.Error("SetAgent after Close succeeded")
	}
	if n := f.count("PATCH", "/api/session/"+fakeSession) - before; n != 0 {
		t.Fatalf("%d rule writes after Close", n)
	}
}

// A config reload while the event stream is down reaches the rules when
// it reconnects, and so does an agent switch made meanwhile.
func TestReloadDuringOutage(t *testing.T) {
	f := newFakeServer(t)
	agents := &switchableAgents{}
	agents.serve(f)
	l := logPatches(t, f, fakeSession)
	var mu sync.Mutex
	nativeAgent := ""
	f.on("GET /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		info := sessionInfo(fakeSession)
		mu.Lock()
		if nativeAgent != "" {
			info["agent"] = nativeAgent
		}
		mu.Unlock()
		return 200, data(info)
	})
	startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	f.dropStreams()
	agents.set(false, map[string]any{"action": "edit", "resource": "secrets/*", "effect": "deny"})
	mu.Lock()
	nativeAgent = "explore"
	mu.Unlock()
	rules := l.waitRules(t, fakeSession, func(r []string) bool { return slices.Contains(r, "edit:secrets/*=deny") })
	if !slices.Contains(rules, "*:*=deny") {
		t.Fatalf("rules after reconnecting are not explore's: %v", rules)
	}
}
