package claude

import (
	"encoding/json"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

func TestPermissionResultMakesNoToolWideRule(t *testing.T) {
	suggestion := PermissionUpdate{Type: "addRules", Rules: []PermissionRule{{ToolName: "Bash", RuleContent: "ls:*"}}, Behavior: "allow", Destination: "localSettings"}
	cases := []struct {
		name     string
		details  PermissionDetails
		decision agent.PermissionDecision
		rules    int
		class    string
	}{
		{"session without suggestions", PermissionDetails{ToolName: "Bash"}, agent.DecisionAllowSession, 0, "user_temporary"},
		{"always without suggestions", PermissionDetails{ToolName: "Bash"}, agent.DecisionAllowAlways, 0, "user_temporary"},
		{"session suppressed", PermissionDetails{ToolName: "Bash", Suggestions: []PermissionUpdate{suggestion}, SuppressAlwaysAllow: true}, agent.DecisionAllowSession, 0, "user_temporary"},
		{"session with suggestion", PermissionDetails{ToolName: "Bash", Suggestions: []PermissionUpdate{suggestion}}, agent.DecisionAllowSession, 1, "user_permanent"},
		{"always with suggestion", PermissionDetails{ToolName: "Bash", Suggestions: []PermissionUpdate{suggestion}}, agent.DecisionAllowAlways, 1, "user_permanent"},
	}
	for _, c := range cases {
		c.details.Input = json.RawMessage(`{"command":"ls"}`)
		r, ok := permissionResult(c.details, agent.PermissionResponse{Decision: c.decision}).(PermissionResult)
		if !ok || r.Behavior != "allow" || len(r.UpdatedPermissions) != c.rules || r.DecisionClassification != c.class {
			t.Errorf("%s: result = %+v", c.name, r)
		}
		for _, u := range r.UpdatedPermissions {
			for _, rule := range u.Rules {
				if rule.RuleContent == "" {
					t.Errorf("%s: tool-wide rule %+v", c.name, rule)
				}
			}
		}
	}
}
