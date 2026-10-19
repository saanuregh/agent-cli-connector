//go:build live

package opencode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Live regression tests for the tenth review, against OpenCode's
// evaluator; they port the reviewer's reproductions.

// writeConfig replaces the user config of p's server.
func writeConfig(t *testing.T, p *Provider, config string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.opts.Env["XDG_CONFIG_HOME"], "opencode", "opencode.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

// waitDefault waits until the server lists name as its default agent.
func waitDefault(t *testing.T, s *Session, name string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		list, err := s.Agents(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if defaultAgentOf(list) == name {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the default agent never became %s", name)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A new default agent that a subagent's rule write saw first still gets
// its rules before the next input.
func TestLiveDefaultChangeSeenBySubagentWrite(t *testing.T) {
	ctx := context.Background()
	p, s, e := startRules(t, `{}`, agent.PermissionBypass)
	waitAgent(t, s, "build")
	writeConfig(t, p, `{"default_agent":"locked","agents":{"locked":{"mode":"primary","permissions":[{"action":"shell","resource":"*","effect":"deny"}]}}}`)
	waitDefault(t, s, "locked")
	var child nativeSession
	if err := s.c.data(ctx, "POST", "/api/session", map[string]any{"parentID": s.ID(), "agent": "general"}, &child); err != nil {
		t.Fatal(err)
	}
	eventually(t, s, e, child.ID, "subagent", "x", "deny")
	// What every input does first.
	if err := s.followDefaultAgent(ctx); err != nil {
		t.Fatal(err)
	}
	rules, _ := nativeRules(t, s, s.ID())
	if got := e.ask(rules, "", "shell", "ls"); got != "deny" {
		t.Fatalf("shell under the new default agent and bypass = %s; rules %v", got, rules)
	}
	if got := s.currentAgent(); got != "locked" {
		t.Fatalf("session agent = %q", got)
	}
}

// A config reload that adds a deny reaches the rules by itself, for a
// session that follows the default agent and for one pinned to an agent,
// and for their subagents.
func TestLiveReloadDenyApplied(t *testing.T) {
	ctx := context.Background()
	for _, pinned := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "pinned"}[pinned], func(t *testing.T) {
			p, s, e := startRules(t, `{}`, agent.PermissionBypass)
			waitAgent(t, s, "build")
			if pinned {
				if err := s.SetAgent(ctx, "build"); err != nil {
					t.Fatal(err)
				}
			}
			var child nativeSession
			if err := s.c.data(ctx, "POST", "/api/session", map[string]any{"parentID": s.ID(), "agent": "general"}, &child); err != nil {
				t.Fatal(err)
			}
			eventually(t, s, e, child.ID, "subagent", "x", "deny")
			writeConfig(t, p, `{"permissions":[{"action":"edit","resource":"secrets/*","effect":"deny"}]}`)
			eventually(t, s, e, s.ID(), "edit", "secrets/k", "deny")
			eventually(t, s, e, child.ID, "edit", "secrets/k", "deny")
			eventually(t, s, e, s.ID(), "edit", "src/a.go", "allow")
		})
	}
}
