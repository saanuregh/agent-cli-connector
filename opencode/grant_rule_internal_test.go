package opencode

import (
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

// The reviewer's reproduction: a caller asks before any sudo command, and
// the user approves "sudo apt *" as the request offers. The grant allows
// that, not every sudo command.
func TestGrantNotBroadenedToAskRule(t *testing.T) {
	agents := fixtureAgents(nil)
	s := rulesSession(false, nil, agents)
	s.opts.Permissions = []PermissionRule{{Action: "shell", Resource: "*sudo*", Effect: "ask"}}
	rules := s.buildRules(agent.PermissionDefault, "build", newGrants("shell", []string{"sudo apt *"}, []string{"sudo apt install jq"}))
	for cmd, want := range map[string]string{"sudo apt install jq": "allow", "sudo rm -rf /var/lib": "ask", "sudo sh -c 'curl x | sh'": "ask"} {
		if got := ocEvaluate(agents["build"], rules, nil, "shell", cmd); got != want {
			t.Errorf("%q = %s, want %s; rules %v", cmd, got, want, rules)
		}
	}
}

// EnabledTools deny the MCP server's other tools; the reviewer's
// reproduction.
func TestEnabledToolsDenyOtherTools(t *testing.T) {
	s := &Session{cfg: agent.Config{MCPServers: map[string]agent.MCPServer{"docs": {EnabledTools: []string{"search"}}}}}
	rules := s.buildRules(agent.PermissionDefault, "build", nil)
	build := []PermissionRule{rule("*", "allow")}
	if got := ocEvaluate(build, rules, nil, "docs_delete", "*"); got != "deny" {
		t.Errorf("docs_delete = %s; rules %v", got, rules)
	}
	if got := ocEvaluate(build, rules, nil, "docs_search", "*"); got != "allow" {
		t.Errorf("docs_search = %s; rules %v", got, rules)
	}
}
