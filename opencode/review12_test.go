package opencode_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Regression tests for the twelfth review; the first is the reviewer's
// reproduction, asserting the fixed behavior.

// Another client selects explore while the reconnection's read of the
// session is in flight; the read answers with the agent before the switch,
// which must not undo the selection.
func TestResyncReadOvertakenBySelection(t *testing.T) {
	f := newFakeServer(t)
	agents := &switchableAgents{}
	agents.serve(f)
	l := logPatches(t, f, fakeSession)
	var mu sync.Mutex
	nativeAgent := ""
	armed := false
	var once sync.Once
	f.on("GET /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		info := sessionInfo(fakeSession)
		mu.Lock()
		if nativeAgent != "" {
			info["agent"] = nativeAgent
		}
		a := armed
		mu.Unlock()
		if a {
			once.Do(func() {
				mu.Lock()
				nativeAgent = "explore"
				mu.Unlock()
				f.emit("session.agent.selected", map[string]any{"agent": "explore"})
				time.Sleep(200 * time.Millisecond)
			})
		}
		return 200, data(info)
	})
	startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	mu.Lock()
	armed = true
	mu.Unlock()
	f.dropStreams()
	time.Sleep(time.Second)
	l.waitRules(t, fakeSession, func(r []string) bool { return slices.Contains(r, "*:*=deny") })
	time.Sleep(500 * time.Millisecond)
	if rules := l.get(fakeSession); !slices.Contains(rules, "*:*=deny") {
		t.Fatalf("the server runs explore; rules = %v", rules)
	}
}

// Rewrite requests while one waits to start are coalesced into it.
func TestRewritesCoalesce(t *testing.T) {
	f := newFakeServer(t)
	agents := &switchableAgents{}
	agents.serve(f)
	release := make(chan struct{})
	entered := make(chan struct{}, 64)
	var mu sync.Mutex
	writes := 0
	f.on("PATCH /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		mu.Lock()
		writes++
		first := writes == 1
		mu.Unlock()
		entered <- struct{}{}
		if first {
			<-release
		}
		return 204, nil
	})
	startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	agents.set(false, map[string]any{"action": "edit", "resource": "a/*", "effect": "deny"})
	emitLocated(f, "agent.updated", "/work")
	<-entered
	reads := f.count("GET", "/api/agent")
	// While the first rewrite writes, each reload changes the rules again.
	for i := 0; i < 20; i++ {
		agents.set(false, map[string]any{"action": "edit", "resource": "b/*", "effect": "deny"}, map[string]any{"action": "read", "resource": string(rune('a'+i)) + "/*", "effect": "deny"})
		emitLocated(f, "agent.updated", "/work")
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	// Each rewrite reads the agents once.
	if writes != 2 || f.count("GET", "/api/agent")-reads != 1 {
		t.Fatalf("%d rule writes and %d agent reads for 21 reloads, want the one in flight and one more", writes, f.count("GET", "/api/agent")-reads)
	}
}

// A stream that keeps ending right after it opens is reopened with a
// growing delay.
func TestReconnectBacksOff(t *testing.T) {
	f := newFakeServer(t)
	startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	start := f.streamCount()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.dropStreams()
		time.Sleep(10 * time.Millisecond)
	}
	// 250 ms, 500 ms, and 1 s of backoff fit in 2 s.
	if n := f.streamCount() - start; n < 2 || n > 5 {
		t.Fatalf("%d streams opened in 2 s", n)
	}
}
