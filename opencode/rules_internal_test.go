package opencode

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

// fixtureAgents returns the rules of OpenCode 2.0.32's build, plan,
// explore, and general agents for a project at /work, as GET /api/agent
// reports them, with config's permissions where OpenCode inserts them:
// before each agent's last rule, the browser deny.
func fixtureAgents(config []PermissionRule) map[string][]PermissionRule {
	env := []PermissionRule{
		rule("*", "allow"),
		{Action: "read", Resource: "*.env", Effect: "ask"},
		{Action: "read", Resource: "*.env.*", Effect: "ask"},
		{Action: "read", Resource: "*.env.example", Effect: "allow"},
	}
	agents := map[string][]PermissionRule{
		"build": append(slices.Clone(env), rule("question", "allow")),
		"plan": append(slices.Clone(env), rule("question", "allow"), rule("edit", "deny"),
			PermissionRule{Action: "edit", Resource: "/work/.opencode/plan/*", Effect: "allow"}),
		"general": append(slices.Clone(env), rule("question", "deny"), rule("subagent", "deny")),
		"explore": append(slices.Clone(env), rule("*", "deny"), rule("shell", "allow"), rule("grep", "allow"),
			rule("glob", "allow"), rule("webfetch", "allow"), rule("websearch", "allow"), rule("read", "allow"),
			PermissionRule{Action: "read", Resource: "*.env", Effect: "ask"},
			PermissionRule{Action: "read", Resource: "*.env.*", Effect: "ask"},
			PermissionRule{Action: "read", Resource: "*.env.example", Effect: "allow"},
			rule("subagent", "deny"), rule("external_directory", "allow")),
	}
	for name, rules := range agents {
		agents[name] = append(append(rules, config...), rule("browser", "deny"))
	}
	return agents
}

// configDenies is a user config that denies edits except docs/, which
// ask, but not docs/private/, and reads of secrets/.
// lockedAgentRules are the own rules of a custom primary agent that denies
// shell commands and asks before docs edits.
var lockedAgentRules = []PermissionRule{rule("shell", "deny"), {Action: "edit", Resource: "docs/*", Effect: "ask"}}

var configDenies = []PermissionRule{
	rule("edit", "deny"),
	{Action: "edit", Resource: "docs/*", Effect: "ask"},
	{Action: "edit", Resource: "docs/private/*", Effect: "deny"},
	{Action: "read", Resource: "secrets/*", Effect: "deny"},
}

// ocEvaluate is OpenCode 2.0.32's evaluation of a request: deny when the
// agent's rules followed by the session's deny any resource; otherwise ask
// when any resource still asks with the saved approvals added last.
func ocEvaluate(agentRules, session, saved []PermissionRule, action string, resources ...string) string {
	rules := append(slices.Clone(agentRules), session...)
	for _, res := range resources {
		if i := lastMatch(rules, action, res); i >= 0 && rules[i].Effect == "deny" {
			return "deny"
		}
	}
	rules = append(rules, saved...)
	for _, res := range resources {
		if i := lastMatch(rules, action, res); i < 0 || rules[i].Effect == "ask" {
			return "ask"
		}
	}
	return "allow"
}

// lastMatch returns the index of the rule that decides action on
// resource, or -1 when none matches.
func lastMatch(rules []PermissionRule, action, resource string) int {
	at := -1
	for i, r := range rules {
		if globMatch(r.Action, action) && globMatch(r.Resource, resource) {
			at = i
		}
	}
	return at
}

type probe struct{ action, resource string }

const testHost = "agenthost-test"

func rulesProbes(planFile string) []probe {
	return []probe{
		{"edit", "src/a.go"}, {"edit", "secrets/key"}, {"edit", "docs/a.md"}, {"edit", "docs/private/k"}, {"edit", planFile},
		{"shell", "git push origin"}, {"shell", "ls -la"}, {"shell", "rm -rf /"},
		{"shell", "sudo apt install jq"}, {"shell", "sudo rm -rf /var/lib"},
		{"webfetch", "https://x"}, {testHost + "_secret", "*"}, {"agenthost-other_x", "*"},
		{"read", ".env"}, {"read", "secrets/k"}, {"read", "src/a.go"},
		{"subagent", "general"}, {"browser", "*"}, {"question", "*"},
	}
}

var ruleModes = []agent.PermissionMode{"", agent.PermissionDefault, agent.PermissionAcceptEdits, agent.PermissionPlan, agent.PermissionReadOnly, agent.PermissionBypass}

type namedRules struct {
	name  string
	rules []PermissionRule
}

func ruleOpenings(planDir string) []namedRules {
	return []namedRules{
		{"new", nil},
		{"resumed-allow-then-ask", []PermissionRule{rule("shell", "allow"), {Action: "shell", Resource: "git push*", Effect: "ask"}}},
		{"resumed-ask-then-deny", []PermissionRule{rule("edit", "ask"), {Action: "edit", Resource: "secrets/*", Effect: "deny"}}},
		{"resumed-plan-like", []PermissionRule{rule("edit", "deny"), {Action: "edit", Resource: planDir, Effect: "allow"}}},
		{"resumed-old-host-tools", []PermissionRule{rule(hostToolPrefix+"*", "deny"), rule("agenthost-old_*", "allow")}},
		{"resumed-two-asks", []PermissionRule{rule("shell", "ask"), {Action: "shell", Resource: "git push*", Effect: "ask"}, rule("webfetch", "ask")}},
		{"resumed-sudo-ask", []PermissionRule{rule("shell", "allow"), {Action: "shell", Resource: "*sudo*", Effect: "ask"}}},
	}
}

type namedGrant struct {
	name  string
	grant *permissionGrant
}

var ruleGrants = []namedGrant{
	{"none", nil},
	{"shell", &permissionGrant{Action: "shell", Asked: "ls -la", Patterns: []string{"ls *"}}},
	{"shell-all", &permissionGrant{Action: "shell", Asked: "ls -la", Patterns: []string{"*"}}},
	{"edit", &permissionGrant{Action: "edit", Asked: "src/a.go", Patterns: []string{"*"}}},
	{"docs-edit", &permissionGrant{Action: "edit", Asked: "docs/a.md", Patterns: []string{"*"}}},
	{"docs-dir", &permissionGrant{Action: "edit", Asked: "docs/a.md", Patterns: []string{"docs/*"}}},
	{"unrelated", &permissionGrant{Action: "webfetch", Asked: "https://x", Patterns: []string{"*"}}},
	{"host-tool", &permissionGrant{Action: testHost + "_secret", Asked: "*", Patterns: []string{"*"}}},
	{"env-read", &permissionGrant{Action: "read", Asked: ".env", Patterns: []string{"*"}}},
	{"sudo", &permissionGrant{Action: "shell", Asked: "sudo apt install jq", Patterns: []string{"sudo apt *"}}},
}

// rulesSession returns a session with what buildRules reads: a host tool
// "secret", a shared server, the agents' rules, and the opening rules.
func rulesSession(needsApproval bool, inherited []PermissionRule, agents map[string][]PermissionRule) *Session {
	return &Session{
		cfg:        agent.Config{Tools: []agent.Tool{{Name: "secret", NeedsApproval: needsApproval}}},
		host:       &hostTools{name: testHost},
		srv:        &serverLease{shared: true},
		inherited:  inherited,
		agentRules: agents,
	}
}

// evaluator answers a request about action on resource under session
// rules, for agent; ocEvaluate in unit tests, the server in live tests.
type evaluator func(session []PermissionRule, agentName, action, resource string) string

// rulesMatrix holds what one run of the invariant matrix covers.
type rulesMatrix struct {
	agents map[string][]PermissionRule
	// config names the server's config; defaultAgent is its default agent,
	// empty for build.
	config       string
	defaultAgent string
	planFile     string
	eval         evaluator
}

// run checks the rules model's invariants with eval for every mode,
// opening, grant, host tool setting, and agent: the session's primary
// agent and the explore and general subagents. It returns the number of
// combinations and of those with a grant OpenCode asked for.
func (m rulesMatrix) run(t *testing.T) (combos, applied int) {
	t.Helper()
	planDir := strings.TrimSuffix(m.planFile, "p.md") + "*"
	probes := rulesProbes(m.planFile)
	agentOf := func(name string) []PermissionRule {
		if r, ok := m.agents[name]; ok {
			return r
		}
		return unknownAgentRules
	}
	for _, mode := range ruleModes {
		for _, opening := range ruleOpenings(planDir) {
			for _, ng := range ruleGrants {
				for _, needsApproval := range []bool{false, true} {
					for _, who := range []string{"primary", "explore", "general"} {
						combos++
						name := fmt.Sprintf("config=%s/%q/%s/%s/approval=%v/%s", m.config, mode, opening.name, ng.name, needsApproval, who)
						s := rulesSession(needsApproval, opening.rules, m.agents)
						s.defaultAgent = m.defaultAgent
						primary := s.wantAgent(mode)
						if opening.name == "new" {
							// A new session opens with the rules created for its mode.
							s.inherited = s.buildRules(mode, primary, nil)
						}
						agentName := primary
						if who != "primary" {
							agentName = who
						}
						before := s.buildRules(mode, agentName, nil)
						after := before
						g := ng.grant
						answer, grantAction := -1, ""
						if g != nil {
							if m.eval(before, agentName, g.Action, g.Asked) != "ask" {
								g = nil // OpenCode would not ask, so no grant arises
							} else {
								applied++
								answer, grantAction = lastMatch(append(slices.Clone(agentOf(agentName)), before...), g.Action, g.Asked), g.Action
								after = s.buildRules(mode, agentName, []permissionGrant{*g})
								if got := m.eval(after, agentName, g.Action, g.Asked); got != "allow" {
									t.Errorf("%s: the granted request is %s; rules %v", name, got, after)
								}
							}
						}
						all := append(slices.Clone(agentOf(agentName)), before...)
						effects := map[probe]string{}
						for _, p := range probes {
							b := m.eval(before, agentName, p.action, p.resource)
							a := m.eval(after, agentName, p.action, p.resource)
							effects[p] = a
							if b == "deny" && a != "deny" {
								t.Errorf("%s: the grant lifted the deny of %v: %s", name, p, a)
							}
							if a != b {
								by := lastMatch(all, p.action, p.resource)
								sameRule := by == answer && (by >= 0 || p.action == grantAction)
								if b != "ask" || a != "allow" || !sameRule {
									t.Errorf("%s: the grant changed %v from %s to %s; it was decided by rule %d, the grant answered rule %d", name, p, b, a, by, answer)
								}
							}
							// No mode lifts an agent or config deny.
							if mode != "" && p.action != testHost+"_secret" && m.eval(nil, agentName, p.action, p.resource) == "deny" && a != "deny" {
								t.Errorf("%s: %v, which the agent denies, is %s", name, p, a)
							}
						}
						// The grant stays with the session across mode changes; in
						// any later mode it never lifts a deny of that mode.
						if g != nil {
							for _, later := range ruleModes {
								laterAgent := s.wantAgent(later)
								if who != "primary" {
									laterAgent = who
								}
								plain := s.buildRules(later, laterAgent, nil)
								withGrant := s.buildRules(later, laterAgent, []permissionGrant{*g})
								for _, p := range probes {
									if m.eval(plain, laterAgent, p.action, p.resource) == "deny" && m.eval(withGrant, laterAgent, p.action, p.resource) != "deny" {
										t.Errorf("%s then %q: the grant lifted the deny of %v", name, later, p)
									}
								}
							}
						}
						host := effects[probe{testHost + "_secret", "*"}]
						switch {
						case needsApproval && ng.name != "host-tool" && host != "ask":
							t.Errorf("%s: a NeedsApproval host tool is %s", name, host)
						case !needsApproval && host != "allow":
							t.Errorf("%s: the session's own host tool is %s", name, host)
						}
						if other := effects[probe{"agenthost-other_x", "*"}]; other != "deny" {
							t.Errorf("%s: another session's host tool is %s", name, other)
						}
						if mode == agent.PermissionPlan || mode == agent.PermissionReadOnly {
							for _, res := range []string{"src/a.go", "secrets/key", "docs/a.md"} {
								if got := effects[probe{"edit", res}]; got != "deny" {
									t.Errorf("%s: edit %s is %s", name, res, got)
								}
							}
						}
						if mode == agent.PermissionPlan {
							// The plan agent writes its plan unless its config denies
							// that; no subagent does.
							want := "deny"
							if who == "primary" {
								want = m.eval(nil, "plan", "edit", m.planFile)
							}
							if got := effects[probe{"edit", m.planFile}]; got != want {
								t.Errorf("%s: plan file edit is %s, want %s", name, got, want)
							}
						}
					}
				}
			}
		}
	}
	return combos, applied
}

const fixturePlanFile = "/work/.opencode/plan/p.md"

// TestRulesInvariants checks the rules model against ocEvaluate for every
// mode, opening, grant, host tool setting, and agent, without and with a
// config that denies edits. TestLiveRulesInvariants runs the same matrix
// against OpenCode's own evaluator.
func TestRulesInvariants(t *testing.T) {
	for _, config := range []string{"plain", "config-denies", "custom-default-agent"} {
		var conf []PermissionRule
		if config == "config-denies" {
			conf = configDenies
		}
		agents := fixtureAgents(conf)
		def := ""
		if config == "custom-default-agent" {
			def = "locked"
			agents["locked"] = append(slices.Clone(agents["build"][:4]), lockedAgentRules...)
			agents["locked"] = append(agents["locked"], rule("browser", "deny"))
		}
		m := rulesMatrix{agents: agents, config: config, defaultAgent: def, planFile: fixturePlanFile,
			eval: func(session []PermissionRule, agentName, action, resource string) string {
				rules, ok := agents[agentName]
				if !ok {
					rules = unknownAgentRules
				}
				return ocEvaluate(rules, session, nil, action, resource)
			}}
		combos, applied := m.run(t)
		t.Logf("config=%s: %d combinations, %d with a grant OpenCode would ask for", config, combos, applied)
		if want := len(ruleModes) * 7 * len(ruleGrants) * 2 * 3; combos != want {
			t.Fatalf("combinations = %d, want %d", combos, want)
		}
	}
}

// TestGlobMatchesOpenCode covers the cases where OpenCode's rb differs
// from a plain "*" glob.
func TestGlobMatchesOpenCode(t *testing.T) {
	for _, c := range []struct {
		pattern, value string
		want           bool
	}{
		{"git push *", "git push", true},
		{"git push *", "git push origin", true},
		{"git push *", "git pushx", false},
		{"git commit *", "git commit -m \"a\nb\"", true},
		{"ls ?", "ls a", true},
		{"ls ?", "ls ab", false},
		{"ls ?", "ls \U0001F600", false}, // two UTF-16 units
		{"ls ??", "ls \U0001F600", true},
		{`C:\x\*`, "C:/x/y", true},
		{"C:/x/*", `C:\x\y`, true},
		{"LS *", "ls -la", false},
		{"a.b", "axb", false},
		{"(x)+[y]", "(x)+[y]", true},
		{"*", "", true},
		{" *", "", true},
	} {
		if got := globMatch(c.pattern, c.value); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.value, got, c.want)
		}
	}
}

// TestGlobCovers checks that globCovers proves only true containments.
func TestGlobCovers(t *testing.T) {
	for _, c := range []struct {
		outer, inner string
		want         bool
	}{
		{"*", "anything *", true},
		{"git *", "git push *", true},
		{"git push*", "git push *", true},
		{"git push *", "git push", true},
		{"git push *", "git push*", false},
		{"*sudo*", "sudo apt *", true},
		{"sudo apt *", "*sudo*", false},
		{"ls ?", "ls *", false},
		{"ls *", "ls ?", true},
		{"ls ?", "ls ?", true},
		{"docs/*", "src/a.go", false},
		{"src/*", "src/a.go", true},
	} {
		if got := globCovers(c.outer, c.inner); got != c.want {
			t.Errorf("globCovers(%q, %q) = %v, want %v", c.outer, c.inner, got, c.want)
		}
	}
}

// TestGrantPlacement covers single grants against session and agent rules.
func TestGrantPlacement(t *testing.T) {
	agents := fixtureAgents(configDenies)
	eval := func(session []PermissionRule, action, res string) string {
		return ocEvaluate(agents["build"], session, nil, action, res)
	}
	// A session ask: the grant goes right after it, so a later deny stays.
	rules := []PermissionRule{rule("edit", "ask"), {Action: "edit", Resource: "secrets/*", Effect: "deny"}}
	got := insertGrant(nil, rules, permissionGrant{Action: "edit", Asked: "src/a.go", Patterns: []string{"*"}})
	if eval(got, "edit", "secrets/key") != "deny" || eval(got, "edit", "src/a.go") != "allow" {
		t.Fatalf("grant after a session ask = %v", got)
	}
	// An agent ask: the agent's later denies keep deciding.
	got = insertGrant(agents["build"], nil, permissionGrant{Action: "edit", Asked: "docs/a.md", Patterns: []string{"*"}})
	if eval(got, "edit", "src/main.go") != "deny" || eval(got, "edit", "docs/a.md") != "allow" || eval(got, "edit", "docs/b.md") != "ask" {
		t.Fatalf("grant of a config ask = %v", got)
	}
	got = insertGrant(agents["build"], nil, permissionGrant{Action: "read", Asked: ".env", Patterns: []string{"*"}})
	if eval(got, "read", "secrets/k") != "deny" || eval(got, "read", ".env") != "allow" || eval(got, "read", "prod.env") != "ask" {
		t.Fatalf("grant of an agent ask = %v", got)
	}
	// A save pattern the answering rule does not provably contain gives
	// way to the asked resource.
	rules = []PermissionRule{rule("shell", "allow"), {Action: "shell", Resource: "*sudo*", Effect: "ask"}}
	got = insertGrant(nil, rules, permissionGrant{Action: "shell", Asked: "sudo apt install jq", Patterns: []string{"*"}})
	if eval(got, "shell", "sudo rm -rf /") != "ask" || eval(got, "shell", "sudo apt install jq") != "allow" {
		t.Fatalf("grant under *sudo* = %v", got)
	}
	// A resource with wildcards is no pattern for itself.
	rules = []PermissionRule{rule("shell", "ask")}
	got = insertGrant(nil, rules, permissionGrant{Action: "shell", Asked: "rm -rf *"})
	if !slices.Equal(got, rules) {
		t.Fatalf("grant of a wildcard command = %v", got)
	}
	// A resource no longer asked about gets no grant.
	rules = []PermissionRule{rule("edit", "deny")}
	if got = insertGrant(nil, rules, permissionGrant{Action: "edit", Asked: "a.go", Patterns: []string{"*"}}); !slices.Equal(got, rules) {
		t.Fatalf("grant under a deny = %v", got)
	}
}

// TestGrantsPairWithResources covers the grants of a request about
// several resources.
func TestGrantsPairWithResources(t *testing.T) {
	s := rulesSession(false, nil, fixtureAgents(nil))
	s.opts.Permissions = []PermissionRule{{Action: "shell", Resource: "git push*", Effect: "ask"}}
	asked := []string{"ls -la", "git push origin"}
	grants := newGrants("shell", []string{"ls *", "git push *"}, asked)
	if grants[0].Patterns[0] != "ls *" || grants[1].Patterns[0] != "git push *" {
		t.Fatalf("grants = %+v", grants)
	}
	rules := s.buildRules(agent.PermissionDefault, "build", grants)
	if got := ocEvaluate(fixtureAgents(nil)["build"], rules, nil, "shell", asked...); got != "allow" {
		t.Fatalf("the same compound command is %s; rules %v", got, rules)
	}
	// A caller's "git push *" ask; the model runs bare "git push".
	s.opts.Permissions = []PermissionRule{{Action: "shell", Resource: "git push *", Effect: "ask"}}
	rules = s.buildRules(agent.PermissionDefault, "build", newGrants("shell", []string{"git push *"}, []string{"git push"}))
	if got := ocEvaluate(fixtureAgents(nil)["build"], rules, nil, "shell", "git push"); got != "allow" {
		t.Fatalf("bare git push after AllowSession is %s; rules %v", got, rules)
	}
}

// TestModesKeepAgentDenies covers the mode rules against agent and config
// denies, including bypassPermissions.
func TestModesKeepAgentDenies(t *testing.T) {
	agents := fixtureAgents(configDenies)
	s := rulesSession(false, nil, agents)
	for _, c := range []struct {
		mode              agent.PermissionMode
		agentName         string
		action, res, want string
	}{
		{agent.PermissionAcceptEdits, "explore", "edit", "src/a.go", "deny"},
		{agent.PermissionDefault, "build", "edit", "src/a.go", "deny"},
		{agent.PermissionDefault, "build", "edit", "docs/a.md", "ask"},
		{agent.PermissionAcceptEdits, "build", "edit", "docs/a.md", "ask"},
		{agent.PermissionBypass, "general", "subagent", "x", "deny"},
		{agent.PermissionBypass, "build", "read", "secrets/k", "deny"},
		{agent.PermissionBypass, "build", "read", ".env", "allow"},
		{agent.PermissionBypass, "build", "edit", "docs/a.md", "allow"},
		{agent.PermissionBypass, "explore", "edit", "src/a.go", "deny"},
		{agent.PermissionBypass, "explore", "shell", "ls", "allow"},
		{agent.PermissionDefault, "explore", "shell", "ls", "ask"},
		{agent.PermissionPlan, "plan", "edit", fixturePlanFile, "deny"},
	} {
		rules := s.buildRules(c.mode, c.agentName, nil)
		if got := ocEvaluate(agents[c.agentName], rules, nil, c.action, c.res); got != c.want {
			t.Errorf("%s/%s: %s %s = %s, want %s; rules %v", c.mode, c.agentName, c.action, c.res, got, c.want, rules)
		}
	}
	// Without the config the plan agent writes its plan.
	plain := fixtureAgents(nil)
	s = rulesSession(false, nil, plain)
	if got := ocEvaluate(plain["plan"], s.buildRules(agent.PermissionPlan, "plan", nil), nil, "edit", fixturePlanFile); got != "allow" {
		t.Errorf("plan file edit = %s", got)
	}
}

// TestToolActionCleansCodeUnits checks the MCP tool action against
// OpenCode, which replaces each UTF-16 code unit.
func TestToolActionCleansCodeUnits(t *testing.T) {
	if got := toolAction("srv.x", "t\U0001F600é"); got != "srv_x_t___" {
		t.Fatalf("toolAction = %q", got)
	}
}
