package opencode_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Regression tests for the ninth review; TestFailedGrantIsDropped is the
// reviewer's reproduction, asserting the fixed behavior.

// patchLog records the rules each session last received.
type patchLog struct {
	mu    sync.Mutex
	rules map[string][]string
	fail  map[string]bool
}

func logPatches(t *testing.T, f *fakeServer, ids ...string) *patchLog {
	l := &patchLog{rules: map[string][]string{}, fail: map[string]bool{}}
	for _, id := range ids {
		f.on("PATCH /api/session/"+id, func(r *fakeRequest) (int, any) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.fail[id] {
				return 500, map[string]any{"name": "UnknownError", "message": "down"}
			}
			l.rules[id] = ruleList(t, r.Body["permissions"])
			return 204, nil
		})
	}
	return l
}

func (l *patchLog) get(id string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.rules[id])
}

func (l *patchLog) setFail(id string, fail bool) {
	l.mu.Lock()
	l.fail[id] = fail
	l.mu.Unlock()
}

// waitRules waits until session id's rules satisfy ok.
func (l *patchLog) waitRules(t *testing.T, id string, ok func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rules := l.get(id)
		if ok(rules) {
			return rules
		}
		if time.Now().After(deadline) {
			t.Fatalf("rules of %s = %v", id, rules)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An AllowSession answer whose rule write fails leaves no grant behind: a
// later mode change does not allow what the user then denied.
func TestFailedGrantIsDropped(t *testing.T) {
	f := newFakeServer(t)
	l := logPatches(t, f, fakeSession)
	var mu sync.Mutex
	var replies []any
	f.on("POST /api/session/"+fakeSession+"/permission/*", func(r *fakeRequest) (int, any) {
		mu.Lock()
		replies = append(replies, r.Body["decision"])
		mu.Unlock()
		return 204, nil
	})
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	send(t, sess, agent.Text("x"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "shell", "resources": []any{"ls -la"}, "save": []any{"ls *"}})
	req := waitFor[*agent.PermissionRequest](t, ev)
	l.setFail(fakeSession, true)
	if err := req.AllowForSession(context.Background()); err == nil {
		t.Fatal("AllowForSession succeeded")
	}
	l.setFail(fakeSession, false)
	if err := req.Deny(context.Background(), "no"); err != nil {
		t.Fatal(err)
	}
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionAcceptEdits); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if rules := l.get(fakeSession); slices.Contains(rules, "shell:ls *=allow") || !slices.Equal(replies, []any{"reject"}) {
		t.Fatalf("replies %v; rules after the mode change %v", replies, rules)
	}
}

// A subagent session whose rule write fails does not fail its parent's
// mode change; the next write sends its rules again.
func TestSubagentWriteFailureKeepsParentChange(t *testing.T) {
	f := newFakeServer(t)
	l := logPatches(t, f, fakeSession, "ses_child")
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "general"})
	l.waitRules(t, "ses_child", func(r []string) bool { return len(r) > 0 })
	l.setFail("ses_child", true)
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionReadOnly); err != nil {
		t.Fatalf("parent mode change failed with its subagent: %v", err)
	}
	if mode := sess.Info().PermissionMode; mode != agent.PermissionReadOnly {
		t.Fatalf("mode = %q", mode)
	}
	l.setFail("ses_child", false)
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	if rules := l.get("ses_child"); !slices.Contains(rules, "*:*=deny") {
		t.Fatalf("subagent rules = %v", rules)
	}
}

// An agent switch by another client gets the rules of the mode for the
// new agent.
func TestAgentSwitchElsewhereWritesRules(t *testing.T) {
	f := newFakeServer(t)
	l := logPatches(t, f, fakeSession)
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	f.emit("session.agent.selected", map[string]any{"agent": "explore"})
	rules := l.waitRules(t, fakeSession, func(r []string) bool { return slices.Contains(r, "*:*=deny") })
	if mode := sess.Info().PermissionMode; mode != agent.PermissionBypass {
		t.Fatalf("mode = %q", mode)
	}
	// explore denies edits, which bypass does not lift.
	if effect(append([]string{"*:*=allow", "*:*=deny"}, rules...), "edit", "src/a.go") != "deny" {
		t.Fatalf("rules for explore = %v", rules)
	}
}

// A session created without an agent runs the server's default agent, and
// its rules are built for that agent.
func TestDefaultAgentFromServer(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/agent", func(*fakeRequest) (int, any) {
		locked := map[string]any{"id": "locked", "name": "locked", "mode": "primary", "permissions": []any{
			map[string]any{"action": "*", "resource": "*", "effect": "allow"},
			map[string]any{"action": "shell", "resource": "*", "effect": "deny"},
		}}
		return 200, data(append([]any{locked}, fakeAgents()...))
	})
	f.on("POST /api/session/"+fakeSession+"/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	created := f.next("POST", "/api/session")
	rules := ruleList(t, created.Body["permissions"])
	if _, ok := created.Body["agent"]; ok || !slices.Contains(rules, "shell:*=deny") {
		t.Fatalf("create body %v", created.Body)
	}
	// Later writes, such as a grant's, are built for that agent too.
	send(t, sess, agent.Text("x"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "edit", "resources": []any{"a.go"}, "save": []any{"*"}})
	if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rules := ruleList(t, f.next("PATCH", "/api/session/"+fakeSession).Body["permissions"]); !slices.Contains(rules, "shell:*=deny") || !slices.Contains(rules, "edit:*=allow") {
		t.Fatalf("grant rules for the default agent = %v", rules)
	}
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionPlan); err != nil {
		t.Fatal(err)
	}
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionDefault); err != nil {
		t.Fatal(err)
	}
	var back *fakeRequest
	for back == nil || back.Body["agent"] == "plan" {
		back = f.next("POST", "/api/session/"+fakeSession+"/agent")
	}
	if back.Body["agent"] != "locked" {
		t.Fatalf("leaving plan switched to %v", back.Body["agent"])
	}
}

// A new subagent session starts from the rules OpenCode reports it copied,
// not from the parent's current rules.
func TestSubagentStartsFromReportedRules(t *testing.T) {
	f := newFakeServer(t)
	l := logPatches(t, f, "ses_child", "ses_grandchild")
	startFake(t, f, agent.Config{PermissionMode: agent.PermissionReadOnly})
	// Copied from an older parent state.
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "build",
		"permissions": []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"}}})
	l.waitRules(t, "ses_child", func(r []string) bool { return slices.Contains(r, "*:*=deny") })
	f.emit("session.created", map[string]any{"sessionID": "ses_grandchild", "parentID": "ses_child", "agent": "general",
		"permissions": []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"}}})
	l.waitRules(t, "ses_grandchild", func(r []string) bool { return slices.Contains(r, "*:*=deny") })
}

// A subagent session resumed with another agent gets that agent's rules,
// whether its event or the subagent call reports the switch first.
func TestSubagentAgentSwitchWritesRules(t *testing.T) {
	f := newFakeServer(t)
	l := logPatches(t, f, "ses_child")
	startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "general"})
	l.waitRules(t, "ses_child", func(r []string) bool { return slices.Contains(r, "subagent:*=deny") })
	f.emit("session.agent.selected", map[string]any{"sessionID": "ses_child", "agent": "explore"})
	rules := l.waitRules(t, "ses_child", func(r []string) bool { return slices.Contains(r, "*:*=deny") })
	if effect(append([]string{"*:*=allow", "*:*=deny"}, rules...), "edit", "src/a.go") != "deny" {
		t.Fatalf("explore subagent rules under bypass = %v", rules)
	}
}

// EnabledTools deny the server's other tools, and a server name that the
// deny would also cover is refused.
func TestEnabledToolsNameCollision(t *testing.T) {
	f := newFakeServer(t)
	servers := map[string]agent.MCPServer{
		"gh":            {Command: "gh-mcp", EnabledTools: []string{"search"}},
		"gh_enterprise": {Command: "ghe-mcp"},
	}
	_, err := f.provider().StartSession(context.Background(), agent.Config{Cwd: "/work", MCPServers: servers})
	if err == nil || !strings.Contains(err.Error(), "gh_enterprise") {
		t.Fatalf("StartSession = %v", err)
	}
	// A server the directory configures collides too.
	f.on("GET /api/mcp", func(*fakeRequest) (int, any) {
		return 200, data([]any{map[string]any{"name": "gh_enterprise", "status": map[string]any{"status": "connected"}}})
	})
	_, err = f.provider().StartSession(context.Background(), agent.Config{Cwd: "/work", MCPServers: map[string]agent.MCPServer{"gh": servers["gh"]}})
	if err == nil || !strings.Contains(err.Error(), "gh_enterprise") {
		t.Fatalf("StartSession with a configured server = %v", err)
	}
}

// A session without an agent of its own follows the server's default
// agent, which a server that just started can list a moment late: the
// rules are written again for it before the next input.
func TestFollowsLateDefaultAgent(t *testing.T) {
	f := newFakeServer(t)
	l := logPatches(t, f, fakeSession)
	var mu sync.Mutex
	late := false
	f.on("GET /api/agent", func(*fakeRequest) (int, any) {
		mu.Lock()
		defer mu.Unlock()
		if !late {
			return 200, data(fakeAgents())
		}
		locked := map[string]any{"id": "locked", "name": "locked", "mode": "primary", "permissions": []any{
			map[string]any{"action": "*", "resource": "*", "effect": "allow"},
			map[string]any{"action": "shell", "resource": "*", "effect": "deny"},
		}}
		return 200, data(append([]any{locked}, fakeAgents()...))
	})
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionBypass})
	mu.Lock()
	late = true
	mu.Unlock()
	send(t, sess, agent.Text("x"))
	if rules := l.get(fakeSession); !slices.Contains(rules, "shell:*=deny") {
		t.Fatalf("rules before the input = %v", rules)
	}
	// A session given an agent keeps it.
	if err := sess.SetAgent(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	send(t, sess, agent.Text("y"))
	if rules := l.get(fakeSession); slices.Contains(rules, "shell:*=deny") {
		t.Fatalf("rules after SetAgent(build) = %v", rules)
	}
}
