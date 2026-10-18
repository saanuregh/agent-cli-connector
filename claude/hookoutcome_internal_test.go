package claude

import (
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

func TestHookOutcome(t *testing.T) {
	code := func(c int) *int { return &c }
	for _, tc := range []struct {
		outcome  string
		exitCode *int
		want     agent.HookOutcome
	}{
		{"success", code(0), agent.HookSucceeded},
		{"success", nil, agent.HookSucceeded},
		{"error", code(1), agent.HookFailed},
		{"error", nil, agent.HookFailed},
		{"error", code(2), agent.HookBlocked},
		{"cancelled", nil, agent.HookFailed},
		{"", nil, ""},
		{"something-new", nil, ""},
	} {
		if got := hookOutcome(tc.outcome, tc.exitCode); got != tc.want {
			t.Errorf("hookOutcome(%q, %v) = %q, want %q", tc.outcome, tc.exitCode, got, tc.want)
		}
	}
}
