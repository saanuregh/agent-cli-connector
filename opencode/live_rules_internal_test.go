//go:build live

package opencode

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Live tests of the rules model against OpenCode's own evaluator: POST
// /api/session/:id/permission evaluates a request against the agent's and
// the session's rules and the saved approvals, and answers allow, ask, or
// deny.

// rulesProvider returns a provider whose server runs in a temporary home,
// with the user config config when not empty.
func rulesProvider(t *testing.T, config string) *Provider {
	t.Helper()
	bin := os.Getenv("OPENCODE_BIN")
	if bin == "" {
		path, err := exec.LookPath("opencode")
		if err != nil {
			t.Skip("opencode not found; set OPENCODE_BIN or PATH")
		}
		bin = path
	}
	home := t.TempDir()
	env := map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, "config"), "XDG_DATA_HOME": filepath.Join(home, "data"),
		"XDG_STATE_HOME": filepath.Join(home, "state"), "XDG_CACHE_HOME": filepath.Join(home, "cache"),
	}
	if config != "" {
		dir := filepath.Join(home, "config", "opencode")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return New(Options{Path: bin, Env: env, HelperDir: t.TempDir()})
}

// liveEvaluator asks OpenCode about requests in a session of its own.
type liveEvaluator struct {
	t       *testing.T
	s       *Session
	sid     string
	current string
	cache   map[string]string
	calls   int
}

func newLiveEvaluator(t *testing.T, s *Session) *liveEvaluator {
	t.Helper()
	var native nativeSession
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.c.data(ctx, "POST", "/api/session", map[string]any{"location": map[string]any{"directory": s.cwd}}, &native); err != nil {
		t.Fatal(err)
	}
	return &liveEvaluator{t: t, s: s, sid: native.ID, current: "-", cache: map[string]string{}}
}

// ask returns OpenCode's answer to a request under session rules, for
// agentName, and rejects the request when it asks.
func (e *liveEvaluator) ask(session []PermissionRule, agentName, action string, resources ...string) string {
	e.t.Helper()
	key, _ := json.Marshal([]any{session, agentName, action, resources})
	if got, ok := e.cache[string(key)]; ok {
		return got
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rules, _ := json.Marshal(session)
	if string(rules) != e.current {
		if _, err := e.s.patchRules(ctx, e.sid, session); err != nil {
			e.t.Fatalf("set rules: %v", err)
		}
		e.current = string(rules)
	}
	body := map[string]any{"action": action, "resources": resources}
	if agentName != "" {
		body["agent"] = agentName
	}
	var out struct {
		ID     string `json:"id"`
		Effect string `json:"effect"`
	}
	if err := e.s.c.data(ctx, "POST", "/api/session/"+e.sid+"/permission", body, &out); err != nil {
		e.t.Fatalf("evaluate: %v", err)
	}
	e.calls++
	if out.Effect == "ask" {
		if err := e.s.c.Call(ctx, "POST", "/api/session/"+e.sid+"/permission/"+out.ID+"/reply", map[string]any{"decision": "reject"}, nil); err != nil {
			e.t.Fatalf("reject: %v", err)
		}
	}
	e.cache[string(key)] = out.Effect
	return out.Effect
}

// liveAgents returns the session's agents' rules and its plan file.
func liveAgents(t *testing.T, s *Session) (map[string][]PermissionRule, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.loadAgents(ctx); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	agents := s.agentRules
	s.mu.Unlock()
	planFile := ""
	for _, r := range agents["plan"] {
		if r.Action == "edit" && r.Effect == "allow" && strings.HasSuffix(r.Resource, "/*") {
			planFile = strings.TrimSuffix(r.Resource, "*") + "p.md"
		}
	}
	for _, name := range []string{"build", "plan", "explore", "general"} {
		if agents[name] == nil {
			t.Fatalf("no %s agent: %v", name, agents)
		}
	}
	if planFile == "" {
		t.Fatalf("no plan file rule: %v", agents["plan"])
	}
	return agents, planFile
}

// TestLiveRulesInvariants runs TestRulesInvariants' matrix against
// OpenCode's evaluator on three servers: one with no config, one whose
// config denies edits and reads of secrets, and one with a saved "always"
// approval. Every answer must also equal ocEvaluate's.
func TestLiveRulesInvariants(t *testing.T) {
	conf, _ := json.Marshal(map[string]any{"permissions": configDenies})
	custom, _ := json.Marshal(map[string]any{"default_agent": "locked",
		"agents": map[string]any{"locked": map[string]any{"mode": "primary", "permissions": lockedAgentRules}}})
	for _, c := range []struct {
		name   string
		config string
		saved  bool
	}{
		{"plain", "", false},
		{"config-denies", string(conf), false},
		{"saved-approval", "", true},
		{"custom-default-agent", string(custom), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			p := rulesProvider(t, c.config)
			sess, err := p.StartSession(ctx, agent.Config{Cwd: t.TempDir(), PermissionMode: agent.PermissionDefault})
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			s := sess
			if c.name == "custom-default-agent" {
				waitAgent(t, s, "locked")
			}
			agents, planFile := liveAgents(t, s)
			s.mu.Lock()
			def := s.defaultAgent
			s.mu.Unlock()
			if c.name == "custom-default-agent" && (def != "locked" || s.currentAgent() != "locked") {
				t.Fatalf("default agent %q, session agent %q", def, s.currentAgent())
			}
			e := newLiveEvaluator(t, s)
			var saved []PermissionRule
			if c.saved {
				// The user answers "always" to a git push request that saves
				// "git push *".
				saved = []PermissionRule{{Action: "shell", Resource: "git push *", Effect: "allow"}}
				if _, err := s.patchRules(ctx, e.sid, []PermissionRule{rule("shell", "ask")}); err != nil {
					t.Fatal(err)
				}
				var out struct{ ID, Effect string }
				if err := s.c.data(ctx, "POST", "/api/session/"+e.sid+"/permission", map[string]any{"action": "shell", "resources": []string{"git push origin"}, "save": []string{"git push *"}}, &out); err != nil || out.Effect != "ask" {
					t.Fatalf("save request: %v %+v", err, out)
				}
				if err := s.c.Call(ctx, "POST", "/api/session/"+e.sid+"/permission/"+out.ID+"/reply", map[string]any{"decision": "always"}, nil); err != nil {
					t.Fatal(err)
				}
			}
			mismatches := 0
			m := rulesMatrix{agents: agents, config: c.name, defaultAgent: def, planFile: planFile,
				eval: func(session []PermissionRule, agentName, action, resource string) string {
					got := e.ask(session, agentName, action, resource)
					agentRules, ok := agents[agentName]
					if !ok {
						agentRules = unknownAgentRules
					}
					if want := ocEvaluate(agentRules, session, saved, action, resource); want != got {
						mismatches++
						if mismatches <= 10 {
							t.Errorf("%s %s for %s: OpenCode %s, ocEvaluate %s; rules %v", action, resource, agentName, got, want, session)
						}
					}
					return got
				}}
			combos, applied := m.run(t)
			t.Logf("%s: %d combinations, %d with a grant OpenCode asked for, %d evaluations by OpenCode, %d mismatches with ocEvaluate", c.name, combos, applied, e.calls, mismatches)
			if c.saved {
				// A saved approval outranks a session ask but no deny.
				if got := e.ask([]PermissionRule{{Action: "shell", Resource: "git push *", Effect: "ask"}}, "build", "shell", "git push origin"); got != "allow" {
					t.Errorf("a saved approval under a session ask: %s", got)
				}
				if got := e.ask([]PermissionRule{{Action: "shell", Resource: "git push *", Effect: "deny"}}, "build", "shell", "git push origin"); got != "deny" {
					t.Errorf("a saved approval under a session deny: %s", got)
				}
			}
		})
	}
}

// TestLiveGlobMatchesOpenCode compares globMatch with OpenCode's matcher.
func TestLiveGlobMatchesOpenCode(t *testing.T) {
	ctx := context.Background()
	p := rulesProvider(t, "")
	sess, err := p.StartSession(ctx, agent.Config{Cwd: t.TempDir(), PermissionMode: agent.PermissionDefault})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	e := newLiveEvaluator(t, sess)
	for _, c := range []struct{ pattern, value string }{
		{"git push *", "git push"}, {"git push *", "git pushx"}, {"git commit *", "git commit -m \"a\nb\""},
		{"ls ?", "ls a"}, {"ls ?", "ls ab"}, {"ls ?", "ls \U0001F600"}, {"ls ??", "ls \U0001F600"},
		{`C:\x\*`, "C:/x/y"}, {"C:/x/*", `C:\x\y`}, {"LS *", "ls -la"}, {"a.b", "axb"}, {"(x)+[y]", "(x)+[y]"},
		{" *", ""}, {"*sudo*", "sudo apt install jq"},
	} {
		rules := []PermissionRule{rule("shell", "allow"), {Action: "shell", Resource: c.pattern, Effect: "deny"}}
		got := e.ask(rules, "build", "shell", c.value) == "deny"
		if want := globMatch(c.pattern, c.value); got != want {
			t.Errorf("pattern %q, value %q: OpenCode %v, globMatch %v", c.pattern, c.value, got, want)
		}
	}
}

// An AllowSession answer to an ask of the config never lifts the config's
// denies; the reviewer's reproduction.
func TestLiveGrantKeepsConfigDenies(t *testing.T) {
	ctx := context.Background()
	conf := `{"permissions":[{"action":"edit","resource":"*","effect":"deny"},{"action":"edit","resource":"docs/*","effect":"ask"},{"action":"read","resource":"secrets/*","effect":"deny"}]}`
	for _, mode := range []agent.PermissionMode{"", agent.PermissionDefault, agent.PermissionPlan, agent.PermissionReadOnly} {
		t.Run(string(mode), func(t *testing.T) {
			p := rulesProvider(t, conf)
			sess, err := p.StartSession(ctx, agent.Config{Cwd: t.TempDir(), PermissionMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			s := sess
			e := newLiveEvaluator(t, s)
			ask := func(action, res string) string {
				rules, err := s.PermissionRules(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return e.ask(rules, s.wantAgent(mode), action, res)
			}
			// What replyPermission does for AllowSession on a docs/a.md ask and
			// on a .env read ask, both saving "*".
			if err := s.grant(ctx, s.ID(), "edit", []string{"*"}, []string{"docs/a.md"}); err != nil {
				t.Fatal(err)
			}
			if err := s.grant(ctx, s.ID(), "read", []string{"*"}, []string{".env"}); err != nil {
				t.Fatal(err)
			}
			want := map[[2]string]string{
				{"edit", "src/main.go"}: "deny", {"read", "secrets/k"}: "deny", {"read", ".env"}: "allow",
				{"read", "prod.env"}: "ask", {"edit", "docs/a.md"}: "allow", {"edit", "docs/b.md"}: "ask",
			}
			if mode == agent.PermissionPlan || mode == agent.PermissionReadOnly {
				want[[2]string{"edit", "docs/a.md"}], want[[2]string{"edit", "docs/b.md"}] = "deny", "deny"
				// Everything but the kept actions is denied.
				for _, action := range []string{"webfetch", "websearch", "github_create_issue", "execute", "worktree_create", "session_rename"} {
					want[[2]string{action, "*"}] = "deny"
				}
				want[[2]string{"read", "src/main.go"}], want[[2]string{"glob", "*"}] = "allow", "allow"
			}
			for k, w := range want {
				if got := ask(k[0], k[1]); got != w {
					rules, _ := s.PermissionRules(ctx)
					t.Errorf("%s %s = %s, want %s; rules %v", k[0], k[1], got, w, rules)
				}
			}
		})
	}
}

// Subagent sessions follow their parent's mode and grants, with their own
// agent's denies; the reviewer's reproduction, extended.
func TestLiveSubagentRules(t *testing.T) {
	ctx := context.Background()
	p := rulesProvider(t, "")
	sess, err := p.StartSession(ctx, agent.Config{Cwd: t.TempDir(), PermissionMode: agent.PermissionAcceptEdits})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	s := sess
	go func() {
		for range s.Events() {
		}
	}()
	e := newLiveEvaluator(t, s)
	// newChild creates a subagent session and waits until this library has
	// written its rules.
	newChild := func(agentName string) string {
		t.Helper()
		var child nativeSession
		if err := s.c.data(ctx, "POST", "/api/session", map[string]any{"parentID": s.ID(), "agent": agentName}, &child); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			s.rulesMu.Lock()
			s.mu.Lock()
			sub := s.subagentRules[child.ID]
			ready := sub != nil && sub.known && sub.agent == agentName
			s.mu.Unlock()
			s.rulesMu.Unlock()
			if ready {
				return child.ID
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("subagent session %s not registered", child.ID)
		return ""
	}
	childAsk := func(id, action, res string) string {
		t.Helper()
		var native nativeSession
		if err := s.c.data(ctx, "GET", "/api/session/"+id, nil, &native); err != nil {
			t.Fatal(err)
		}
		return e.ask(native.Permissions, native.Agent, action, res)
	}
	general := newChild("general")
	explore := newChild("explore")
	if got := childAsk(explore, "edit", "src/a.go"); got != "deny" {
		t.Errorf("explore subagent under acceptEdits, edit: %s", got)
	}
	if got := childAsk(general, "edit", "src/a.go"); got != "allow" {
		t.Errorf("general subagent under acceptEdits, edit: %s", got)
	}
	if err := s.SetPermissionMode(ctx, agent.PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	if got := childAsk(general, "edit", "src/a.go"); got != "deny" {
		t.Errorf("subagent created before readOnly, edit: %s", got)
	}
	if got := childAsk(newChild("general"), "edit", "src/a.go"); got != "deny" {
		t.Errorf("subagent created under readOnly, edit: %s", got)
	}
	if err := s.SetPermissionMode(ctx, agent.PermissionPlan); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{general, newChild("general")} {
		for _, action := range []string{"edit", "webfetch", "github_create_issue"} {
			if got := childAsk(id, action, "src/a.go"); got != "deny" {
				t.Errorf("subagent %s under plan, %s: %s", id, action, got)
			}
		}
		if got := childAsk(id, "read", "src/a.go"); got != "allow" {
			t.Errorf("subagent %s under plan, read: %s", id, got)
		}
	}
	if err := s.SetPermissionMode(ctx, agent.PermissionBypass); err != nil {
		t.Fatal(err)
	}
	if got := childAsk(general, "subagent", "x"); got != "deny" {
		t.Errorf("general subagent under bypass, subagent: %s", got)
	}
	if got := childAsk(explore, "edit", "src/a.go"); got != "deny" {
		t.Errorf("explore subagent under bypass, edit: %s", got)
	}
	if err := s.SetPermissionMode(ctx, agent.PermissionDefault); err != nil {
		t.Fatal(err)
	}
	// A subagent's grant stays its own; the parent's reaches its subagents.
	if err := s.grant(ctx, general, "shell", []string{"ls *"}, []string{"ls -la"}); err != nil {
		t.Fatal(err)
	}
	if err := s.grant(ctx, s.ID(), "shell", []string{"pwd *"}, []string{"pwd"}); err != nil {
		t.Fatal(err)
	}
	rules, err := s.PermissionRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ who, cmd, want string }{
		{general, "ls -la", "allow"}, {general, "ls -R", "allow"}, {general, "rm x", "ask"}, {general, "pwd", "allow"},
		{explore, "ls -la", "ask"}, {explore, "pwd", "allow"},
	} {
		if got := childAsk(c.who, "shell", c.cmd); got != c.want {
			t.Errorf("subagent %s, shell %q: %s, want %s", c.who, c.cmd, got, c.want)
		}
	}
	if got := e.ask(rules, "build", "shell", "ls -la"); got != "ask" {
		t.Errorf("parent after a subagent's grant, ls: %s", got)
	}
	if got := e.ask(rules, "build", "shell", "pwd"); got != "allow" {
		t.Errorf("parent after its own grant, pwd: %s", got)
	}
	s.mu.Lock()
	n := len(s.subagentRules)
	s.mu.Unlock()
	if n != 4 {
		t.Errorf("tracked subagent sessions = %d, want 4", n)
	}
}
