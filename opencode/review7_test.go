package opencode_test

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/opencode"
)

// Regression tests for the seventh review. effect evaluates rules as
// OpenCode 2.0.32 does: the last matching rule decides.
func effect(rules []string, action, resource string) string {
	glob := func(p, s string) bool {
		re := "^" + strings.ReplaceAll(regexp.QuoteMeta(p), `\*`, ".*") + "$"
		return regexp.MustCompile(re).MatchString(s)
	}
	out := "ask"
	for _, r := range rules {
		lhs, eff, _ := strings.Cut(r, "=")
		i := strings.LastIndex(lhs, ":")
		if glob(lhs[:i], action) && glob(lhs[i+1:], resource) {
			out = eff
		}
	}
	return out
}

var buildAgentRules = []string{"*:*=allow", "read:*.env=ask", "browser:*=deny"}
var planAgentRules = []string{"*:*=allow", "edit:*=deny", "edit:/plans/*=allow", "browser:*=deny"}

// An unrelated grant keeps a NeedsApproval host tool asking, in an empty
// mode and after leaving bypass for an empty mode.
func TestUnrelatedGrantKeepsHostToolAsk(t *testing.T) {
	for _, mode := range []agent.PermissionMode{"", agent.PermissionBypass} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFakeServer(t)
			f.on("PUT /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
			f.on("DELETE /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
			f.on("POST /api/session/"+fakeSession+"/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
			tool := agent.Tool{Name: "secret", NeedsApproval: true, Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) { return agent.ToolResult{}, nil }}
			sess, ev := startFake(t, f, agent.Config{PermissionMode: mode, Tools: []agent.Tool{tool}})
			created := ruleList(t, f.next("POST", "/api/session").Body["permissions"])
			var action string
			for _, r := range created {
				if strings.HasSuffix(r, ":*=ask") && strings.HasPrefix(r, "agenthost-") {
					action = strings.TrimSuffix(r, ":*=ask")
				}
			}
			if mode == agent.PermissionBypass {
				// The empty mode keeps the rules bypass created, where nothing
				// asks but the host tool, so no other grant arises.
				if err := sess.SetPermissionMode(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
				if got := effect(append(buildAgentRules, created...), action, "*"); got != "ask" || f.count("PATCH", "/api/session/"+fakeSession) != 0 {
					t.Fatalf("NeedsApproval host tool after bypass then empty: %s (%v)", got, created)
				}
				return
			}
			send(t, sess, agent.Text("go"))
			f.emit("session.execution.started", map[string]any{})
			// The build agent asks before reading .env files.
			f.emit("permission.asked", map[string]any{"id": "per_1", "action": "read", "resources": []any{".env"}, "save": []any{"*"}})
			if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(context.Background()); err != nil {
				t.Fatal(err)
			}
			var rules []string
			for effect(append(buildAgentRules, rules...), "read", ".env") != "allow" || len(rules) == 0 {
				rules = ruleList(t, f.next("PATCH", "/api/session/"+fakeSession).Body["permissions"])
			}
			if got := effect(append(buildAgentRules, rules...), "read", "prod.env"); got != "ask" {
				t.Fatalf("a grant for .env answered another read: %s (%v)", got, rules)
			}
			if got := effect(append(buildAgentRules, rules...), action, "*"); got != "ask" {
				t.Fatalf("NeedsApproval host tool after an unrelated grant: %s (%v)", got, rules)
			}
		})
	}
}

// rulesRecorder keeps the session rules the fake server last received.
type rulesRecorder struct {
	mu    sync.Mutex
	rules []string
}

func recordRules(t *testing.T, f *fakeServer) *rulesRecorder {
	r := &rulesRecorder{}
	set := func(req *fakeRequest) {
		if perms, ok := req.Body["permissions"]; ok {
			r.mu.Lock()
			r.rules = ruleList(t, perms)
			r.mu.Unlock()
		}
	}
	f.on("POST /api/session", func(req *fakeRequest) (int, any) {
		set(req)
		info := sessionInfo(fakeSession)
		info["permissions"] = req.Body["permissions"]
		return 200, data(info)
	})
	f.on("PATCH /api/session/"+fakeSession, func(req *fakeRequest) (int, any) { set(req); return 204, nil })
	return r
}

func (r *rulesRecorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.rules...)
}

// The reported permission mode always matches the rules written: SetAgent
// writes the rules of the mode it reports, from every starting mode.
func TestReportedModeMatchesRules(t *testing.T) {
	type step struct {
		name string
		do   func(s interface {
			SetAgent(context.Context, string) error
			SetPermissionMode(context.Context, agent.PermissionMode) error
		}) error
	}
	ctx := context.Background()
	steps := []step{
		{"plan agent", func(s interface {
			SetAgent(context.Context, string) error
			SetPermissionMode(context.Context, agent.PermissionMode) error
		}) error {
			return s.SetAgent(ctx, "plan")
		}},
		{"build agent", func(s interface {
			SetAgent(context.Context, string) error
			SetPermissionMode(context.Context, agent.PermissionMode) error
		}) error {
			return s.SetAgent(ctx, "build")
		}},
		{"readOnly", func(s interface {
			SetAgent(context.Context, string) error
			SetPermissionMode(context.Context, agent.PermissionMode) error
		}) error {
			return s.SetPermissionMode(ctx, agent.PermissionReadOnly)
		}},
		{"plan agent again", func(s interface {
			SetAgent(context.Context, string) error
			SetPermissionMode(context.Context, agent.PermissionMode) error
		}) error {
			return s.SetAgent(ctx, "plan")
		}},
		{"build agent again", func(s interface {
			SetAgent(context.Context, string) error
			SetPermissionMode(context.Context, agent.PermissionMode) error
		}) error {
			return s.SetAgent(ctx, "build")
		}},
	}
	want := map[agent.PermissionMode]string{
		agent.PermissionDefault: "ask", agent.PermissionAcceptEdits: "allow", agent.PermissionBypass: "allow",
		agent.PermissionPlan: "deny", agent.PermissionReadOnly: "deny",
	}
	for _, start := range []agent.PermissionMode{agent.PermissionDefault, agent.PermissionAcceptEdits, agent.PermissionBypass, agent.PermissionPlan} {
		t.Run(string(start), func(t *testing.T) {
			f := newFakeServer(t)
			rec := recordRules(t, f)
			sess, _ := startFake(t, f, agent.Config{PermissionMode: start})
			check := func(after string) {
				mode := sess.Info().PermissionMode
				agentRules := buildAgentRules
				if mode == agent.PermissionPlan {
					agentRules = planAgentRules
				}
				got := effect(append(append([]string(nil), agentRules...), rec.get()...), "edit", "/work/a.go")
				if w, ok := want[mode]; !ok || got != w {
					t.Fatalf("after %s: mode %q but edit is %s (rules %v)", after, mode, got, rec.get())
				}
			}
			check("start")
			for _, st := range steps {
				if err := st.do(sess); err != nil {
					t.Fatal(err)
				}
				check(st.name)
			}
		})
	}
}

// A grant on a subagent session never lifts the subagent's own denies.
func TestChildGrantKeepsChildDeny(t *testing.T) {
	f := newFakeServer(t)
	f.on("POST /api/session/ses_child/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	child := sessionInfo("ses_child")
	child["parentID"] = fakeSession
	f.on("GET /api/session/ses_child", func(*fakeRequest) (int, any) { return 200, data(child) })
	deny := &opencode.SessionOptions{Permissions: []opencode.PermissionRule{{Action: "edit", Resource: "/work/secrets/*", Effect: "deny"}}}
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault, ProviderOptions: deny})
	send(t, sess, agent.Text("go"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "title": "child"})
	f.emit("permission.asked", map[string]any{"id": "per_c", "sessionID": "ses_child", "action": "edit", "resources": []any{"/work/a.go"}, "save": []any{"*"}})
	if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rules []string
	for effect(rules, "edit", "/work/a.go") != "allow" {
		rules = ruleList(t, f.next("PATCH", "/api/session/ses_child").Body["permissions"])
	}
	if effect(rules, "edit", "/work/secrets/key") != "deny" {
		t.Fatalf("child rules after the grant: %v", rules)
	}
}

// A session opened in plan asks before edits after leaving plan.
func TestLeavingPlanOpenedInPlanAsks(t *testing.T) {
	f := newFakeServer(t)
	rec := recordRules(t, f)
	sess, _ := startFake(t, f, agent.Config{PermissionMode: agent.PermissionPlan})
	if err := sess.SetAgent(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if mode := sess.Info().PermissionMode; mode != agent.PermissionDefault {
		t.Fatalf("mode = %q", mode)
	}
	if got := effect(append(buildAgentRules, rec.get()...), "edit", "/work/a.go"); got != "ask" {
		t.Fatalf("edit after leaving plan = %s (%v)", got, rec.get())
	}
}
