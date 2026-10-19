//go:build live

package opencode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Live regression tests for the ninth review, against OpenCode's
// evaluator; they port the reviewer's reproductions.

// startRules starts a session in mode on a server with config and drains
// its events.
func startRules(t *testing.T, config string, mode agent.PermissionMode) (*Provider, *Session, *liveEvaluator) {
	t.Helper()
	p := rulesProvider(t, config)
	sess, err := p.StartSession(context.Background(), agent.Config{Cwd: t.TempDir(), PermissionMode: mode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	go func() {
		for range sess.Events() {
		}
	}()
	return p, sess, newLiveEvaluator(t, sess)
}

// nativeRules reads session id's rules and agent from the server.
func nativeRules(t *testing.T, s *Session, id string) ([]PermissionRule, string) {
	t.Helper()
	var native nativeSession
	if err := s.c.data(context.Background(), "GET", "/api/session/"+id, nil, &native); err != nil {
		t.Fatal(err)
	}
	return native.Permissions, native.Agent
}

// eventually asks OpenCode about a request in session id, under the
// session's own rules and agent, until the answer is want.
func eventually(t *testing.T, s *Session, e *liveEvaluator, id, action, resource, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rules, agentName := nativeRules(t, s, id)
		got := e.ask(rules, agentName, action, resource)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s in %s (agent %q) = %s, want %s; rules %v", action, resource, id, agentName, got, want, rules)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

const lockedConfig = `{"agents":{"locked":{"mode":"primary","permissions":[{"action":"shell","resource":"*","effect":"deny"}]}}}`

// waitAgent waits until the server lists agent name, which a server that
// just started can list a moment late, and then lets the session follow
// its default agent as before an input.
func waitAgent(t *testing.T, s *Session, name string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		list, err := s.Agents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(list, func(a AgentInfo) bool { return a.ID == name }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never listed agent %s", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := s.followDefaultAgent(ctx); err != nil {
		t.Fatal(err)
	}
}

// EnabledTools deny the MCP server's other tools.
func TestLiveEnabledToolsDenyOtherTools(t *testing.T) {
	_, s, e := startRules(t, "", agent.PermissionDefault)
	s.cfg.MCPServers = map[string]agent.MCPServer{"docs": {EnabledTools: []string{"search"}}}
	for _, mode := range []agent.PermissionMode{agent.PermissionDefault, agent.PermissionBypass} {
		rules := s.buildRules(mode, "build", nil)
		if got := e.ask(rules, "build", "docs_delete", "*"); got != "deny" {
			t.Errorf("%s: docs_delete = %s; rules %v", mode, got, rules)
		}
		if got := e.ask(rules, "build", "docs_search", "*"); got != "allow" {
			t.Errorf("%s: docs_search = %s; rules %v", mode, got, rules)
		}
	}
}

// A session created without an agent runs the config's default_agent,
// whose denies the rules keep.
func TestLiveDefaultAgent(t *testing.T) {
	conf := `{"default_agent":"locked","agents":{"locked":{"mode":"primary","permissions":[{"action":"shell","resource":"*","effect":"deny"}]}}}`
	_, s, e := startRules(t, conf, agent.PermissionBypass)
	waitAgent(t, s, "locked")
	if got := s.currentAgent(); got != "locked" {
		t.Fatalf("session agent = %q", got)
	}
	rules, _ := nativeRules(t, s, s.ID())
	// No agent named: OpenCode uses the session's.
	if got := e.ask(rules, "", "shell", "ls"); got != "deny" {
		t.Fatalf("shell under the default agent and bypass = %s; rules %v", got, rules)
	}
	if got := e.ask(rules, "", "edit", "src/a.go"); got != "allow" {
		t.Fatalf("edit under bypass = %s", got)
	}
}

// An agent switch by another client gets the rules for the new agent.
func TestLiveAgentSwitchElsewhere(t *testing.T) {
	ctx := context.Background()
	_, s, e := startRules(t, lockedConfig, agent.PermissionBypass)
	if err := s.c.Call(ctx, "POST", "/api/session/"+s.ID()+"/agent", map[string]any{"agent": "locked"}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, s, e, s.ID(), "shell", "ls", "deny")
	if mode := s.Info().PermissionMode; mode != agent.PermissionBypass {
		t.Fatalf("mode = %q", mode)
	}
}

// Config and agents reloaded mid-session reach the next rule write: a new
// config deny stays a deny, and an agent added later is not locked out.
func TestLiveConfigReload(t *testing.T) {
	ctx := context.Background()
	p, s, e := startRules(t, `{}`, agent.PermissionBypass)
	conf := `{"agents":{"late":{"mode":"primary","permissions":[{"action":"webfetch","resource":"*","effect":"deny"}]}},"permissions":[{"action":"edit","resource":"secrets/*","effect":"deny"}]}`
	if err := os.WriteFile(filepath.Join(p.opts.Env["XDG_CONFIG_HOME"], "opencode", "opencode.json"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		list, err := s.Agents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(list, func(a AgentInfo) bool { return a.ID == "late" }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server never listed the new agent")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := s.SetPermissionMode(ctx, agent.PermissionDefault); err != nil {
		t.Fatal(err)
	}
	rules, ag := nativeRules(t, s, s.ID())
	if got := e.ask(rules, ag, "edit", "secrets/k"); got != "deny" {
		t.Errorf("edit secrets/k after the config denies it = %s; rules %v", got, rules)
	}
	if err := s.SetAgent(ctx, "late"); err != nil {
		t.Fatal(err)
	}
	rules, ag = nativeRules(t, s, s.ID())
	for res, want := range map[[2]string]string{{"read", "src/a.go"}: "allow", {"edit", "src/a.go"}: "ask", {"webfetch", "https://x"}: "deny"} {
		if got := e.ask(rules, ag, res[0], res[1]); got != want {
			t.Errorf("agent %s, %s %s = %s, want %s; rules %v", ag, res[0], res[1], got, want, rules)
		}
	}
}

// A subagent session resumed with another agent gets that agent's rules.
func TestLiveSubagentAgentSwitch(t *testing.T) {
	ctx := context.Background()
	_, s, e := startRules(t, "", agent.PermissionBypass)
	var child nativeSession
	if err := s.c.data(ctx, "POST", "/api/session", map[string]any{"parentID": s.ID(), "agent": "general"}, &child); err != nil {
		t.Fatal(err)
	}
	eventually(t, s, e, child.ID, "subagent", "x", "deny")
	// What the subagent tool does to resume it with another agent.
	if err := s.c.Call(ctx, "POST", "/api/session/"+child.ID+"/agent", map[string]any{"agent": "explore"}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, s, e, child.ID, "edit", "src/a.go", "deny")
	if _, ag := nativeRules(t, s, child.ID); ag != "explore" {
		t.Fatalf("subagent agent = %q", ag)
	}
}

// A new subagent session starts from the rules OpenCode gave it, so rules
// from an older parent state are replaced.
func TestLiveSubagentReportedRules(t *testing.T) {
	ctx := context.Background()
	_, s, e := startRules(t, `{"agents":{"helper":{"mode":"subagent"}}}`, agent.PermissionReadOnly)
	var child nativeSession
	if err := s.c.data(ctx, "POST", "/api/session", map[string]any{"parentID": s.ID(), "agent": "helper", "permissions": []PermissionRule{rule("*", "allow")}}, &child); err != nil {
		t.Fatal(err)
	}
	eventually(t, s, e, child.ID, "edit", "src/a.go", "deny")
}

// Subagent sessions created while the mode changes all end with the
// mode's rules: 40 rounds of a creation racing a mode change.
func TestLiveSubagentCreationRace(t *testing.T) {
	ctx := context.Background()
	_, s, _ := startRules(t, `{"agents":{"helper":{"mode":"subagent"}}}`, agent.PermissionBypass)
	modes := []agent.PermissionMode{agent.PermissionReadOnly, agent.PermissionBypass}
	var ids []string
	for i := 0; i < 40; i++ {
		var wg sync.WaitGroup
		var child nativeSession
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := s.c.data(ctx, "POST", "/api/session", map[string]any{"parentID": s.ID(), "agent": "helper"}, &child); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i%5) * time.Millisecond)
			if err := s.SetPermissionMode(ctx, modes[i%2]); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		ids = append(ids, child.ID)
	}
	want := s.buildRules(s.Info().PermissionMode, "helper", nil)
	stale := 0
	for _, id := range ids {
		deadline := time.Now().Add(5 * time.Second)
		for {
			rules, _ := nativeRules(t, s, id)
			if slices.Equal(rules, want) {
				break
			}
			if time.Now().After(deadline) {
				stale++
				t.Errorf("subagent %s rules %v, want %v", id, rules, want)
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	t.Logf("%d of %d subagent sessions have stale rules", stale, len(ids))
}
