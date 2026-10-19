package opencode_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Regression tests for the tenth review; the first two are the reviewer's
// reproductions, asserting the fixed behavior.

// A caller context cancelled while the parent's rules are written still
// lets the subagents' rules follow.
func TestCancelDuringParentWriteStillWritesSubagents(t *testing.T) {
	f := newFakeServer(t)
	l := logPatches(t, f, "ses_child")
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "general",
		"permissions": []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"}}})
	l.waitRules(t, "ses_child", func(r []string) bool { return len(r) > 0 })
	ctx, cancel := context.WithCancel(context.Background())
	f.on("PATCH /api/session/"+fakeSession, func(*fakeRequest) (int, any) { cancel(); return 204, nil })
	if err := sess.SetPermissionMode(ctx, agent.PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	if rules := l.get("ses_child"); !slices.Contains(rules, "*:*=deny") {
		t.Fatalf("readOnly reported while the subagent has %v", rules)
	}
}

// An agent list that cannot be read fails the input before it is sent.
func TestAgentListFailureFailsInput(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	f.on("GET /api/agent", func(*fakeRequest) (int, any) { return 500, map[string]any{"message": "boom"} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	turn, err := sess.Send(ctx, agent.Text("hi"))
	if err == nil || turn != nil || f.count("POST", "/api/session/"+fakeSession+"/prompt") != 0 {
		t.Fatalf("Send = %v, %v; prompts %d", turn, err, f.count("POST", "/api/session/"+fakeSession+"/prompt"))
	}
}

// switchableAgents serves the fake's agents, with a default agent
// "locked" that denies shell once switched on, and config rules.
type switchableAgents struct {
	mu     sync.Mutex
	locked bool
	config []any
}

func (a *switchableAgents) serve(f *fakeServer) {
	f.on("GET /api/agent", func(*fakeRequest) (int, any) {
		a.mu.Lock()
		defer a.mu.Unlock()
		list := fakeAgents()
		for _, ag := range list {
			m := ag.(map[string]any)
			m["permissions"] = append(m["permissions"].([]any), a.config...)
		}
		if a.locked {
			locked := map[string]any{"id": "locked", "name": "locked", "mode": "primary", "permissions": []any{
				map[string]any{"action": "*", "resource": "*", "effect": "allow"},
				map[string]any{"action": "shell", "resource": "*", "effect": "deny"},
			}}
			list = append([]any{locked}, list...)
		}
		return 200, data(list)
	})
}

func (a *switchableAgents) set(locked bool, config ...any) {
	a.mu.Lock()
	a.locked, a.config = locked, config
	a.mu.Unlock()
}

// A new default agent seen by a subagent's rule write still reaches this
// session's rules before the next input.
func TestDefaultChangeSeenBySubagentWrite(t *testing.T) {
	f := newFakeServer(t)
	agents := &switchableAgents{}
	agents.serve(f)
	l := logPatches(t, f, fakeSession, "ses_child")
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	agents.set(true)
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "general"})
	l.waitRules(t, "ses_child", func(r []string) bool { return len(r) > 0 })
	send(t, sess, agent.Text("x"))
	if rules := l.get(fakeSession); !slices.Contains(rules, "shell:*=deny") {
		t.Fatalf("rules before the input = %v", rules)
	}
}

// A failed rule write for a new default agent is tried again on the next
// input.
func TestFailedDefaultAgentWriteRetried(t *testing.T) {
	f := newFakeServer(t)
	agents := &switchableAgents{}
	agents.serve(f)
	l := logPatches(t, f, fakeSession)
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	agents.set(true)
	l.setFail(fakeSession, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sess.Send(ctx, agent.Text("x")); err == nil {
		t.Fatal("Send succeeded with its rule write failing")
	}
	l.setFail(fakeSession, false)
	send(t, sess, agent.Text("y"))
	if rules := l.get(fakeSession); !slices.Contains(rules, "shell:*=deny") {
		t.Fatalf("rules after the retry = %v", rules)
	}
}

// A mode change of a session that follows the default agent does not pin
// it to an agent, and writes the rules for the default agent as it is.
func TestModeChangeKeepsFollowingDefault(t *testing.T) {
	f := newFakeServer(t)
	agents := &switchableAgents{}
	agents.serve(f)
	l := logPatches(t, f, fakeSession)
	l2 := logPatches(t, f, "ses_child")
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	agents.set(true)
	// A subagent's rule write reads the new default first.
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "general"})
	l2.waitRules(t, "ses_child", func(r []string) bool { return len(r) > 0 })
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionAcceptEdits); err != nil {
		t.Fatal(err)
	}
	if n := f.count("POST", "/api/session/"+fakeSession+"/agent"); n != 0 {
		t.Fatalf("the mode change switched the agent %d times", n)
	}
	if rules := l.get(fakeSession); !slices.Contains(rules, "shell:*=deny") {
		t.Fatalf("acceptEdits rules = %v", rules)
	}
	// Still following: a later default change reaches the rules.
	agents.set(false)
	send(t, sess, agent.Text("x"))
	if rules := l.get(fakeSession); slices.Contains(rules, "shell:*=deny") {
		t.Fatalf("rules after the default went back to build = %v", rules)
	}
}

// A config reload, reported by agent.updated, writes the rules again for
// the session and its subagents; one for another directory does not.
func TestAgentUpdatedRewritesRules(t *testing.T) {
	f := newFakeServer(t)
	agents := &switchableAgents{}
	agents.serve(f)
	l := logPatches(t, f, fakeSession, "ses_child")
	startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "general"})
	l.waitRules(t, "ses_child", func(r []string) bool { return len(r) > 0 })
	deny := map[string]any{"action": "edit", "resource": "secrets/*", "effect": "deny"}
	agents.set(false, deny)
	emitLocated(f, "agent.updated", "/elsewhere")
	time.Sleep(100 * time.Millisecond)
	if rules := l.get(fakeSession); slices.Contains(rules, "edit:secrets/*=deny") {
		t.Fatalf("another directory's reload changed the rules: %v", rules)
	}
	emitLocated(f, "agent.updated", "/work")
	l.waitRules(t, fakeSession, func(r []string) bool { return slices.Contains(r, "edit:secrets/*=deny") })
	l.waitRules(t, "ses_child", func(r []string) bool { return slices.Contains(r, "edit:secrets/*=deny") })
	// A session pinned to an agent gets the reloaded rules too.
	agents.set(false, deny, map[string]any{"action": "read", "resource": "keys/*", "effect": "deny"})
	emitLocated(f, "agent.updated", "/work")
	l.waitRules(t, fakeSession, func(r []string) bool { return slices.Contains(r, "read:keys/*=deny") })
}

// emitLocated emits a server event of no session for directory dir.
func emitLocated(f *fakeServer, typ, dir string) {
	raw, _ := json.Marshal(map[string]any{"id": "evt_" + typ, "type": typ, "data": map[string]any{}, "location": map[string]any{"directory": dir}})
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		ch <- raw
	}
}

// Server names collide by their cleaned tool action prefixes, in both
// directions for EnabledTools and only by possible action for
// ApprovedTools.
func TestMCPNameCollisionDirections(t *testing.T) {
	enabled := agent.MCPServer{Command: "x", EnabledTools: []string{"search"}}
	approved := agent.MCPServer{Command: "x", ApprovedTools: []string{"search"}}
	plain := agent.MCPServer{Command: "x"}
	for _, c := range []struct {
		name      string
		servers   map[string]agent.MCPServer
		directory []string
		refused   bool
	}{
		{"enabled vs longer", map[string]agent.MCPServer{"docs": enabled, "docs_x": plain}, nil, true},
		{"enabled vs shorter", map[string]agent.MCPServer{"docs_x": enabled, "docs": plain}, nil, true},
		{"enabled vs cleaned directory name", map[string]agent.MCPServer{"docs_v2": enabled}, []string{"docs.v2"}, true},
		{"enabled vs shorter directory name", map[string]agent.MCPServer{"docs_v2": enabled}, []string{"docs"}, true},
		{"approved next to longer", map[string]agent.MCPServer{"github": approved, "github_enterprise": plain}, nil, false},
		{"approved action of shorter", map[string]agent.MCPServer{"github_enterprise": approved, "github": plain}, nil, true},
		{"approved unrelated", map[string]agent.MCPServer{"docs": approved, "web": plain}, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeServer(t)
			f.on("PUT /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
			f.on("DELETE /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
			var list []any
			for _, n := range c.directory {
				list = append(list, map[string]any{"name": n, "status": map[string]any{"status": "connected"}})
			}
			f.on("GET /api/mcp", func(*fakeRequest) (int, any) { return 200, data(list) })
			sess, err := f.provider().StartSession(context.Background(), agent.Config{Cwd: "/work", MCPServers: c.servers})
			if err == nil {
				defer sess.Close()
			}
			if refused := err != nil && strings.Contains(err.Error(), "rename one"); refused != c.refused {
				t.Fatalf("StartSession = %v, want refused %v", err, c.refused)
			}
		})
	}
}
