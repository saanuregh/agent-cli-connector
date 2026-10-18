package providerkit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

func TestVersionStatus(t *testing.T) {
	missing, err := VersionStatus(context.Background(), nil, "agent-cli-connector-no-such-binary", nil)
	if err != nil || missing.Installed || missing.Err == nil {
		t.Errorf("missing executable status %+v, %v", missing, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if st, err := VersionStatus(ctx, nil, "sh", nil); st != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled check = %+v, %v", st, err)
	}
}

func TestInheritConfig(t *testing.T) {
	allow := func(context.Context, *agent.PermissionRequest) agent.PermissionResponse {
		return agent.PermissionResponse{Decision: agent.DecisionAllow}
	}
	base := agent.Config{Cwd: "/w", Model: "old", Effort: agent.EffortLow, SystemPrompt: "sys", OutputSchema: json.RawMessage(`{}`), Env: map[string]string{"A": "1"},
		UnsetEnv: []string{"B"}, Handlers: agent.Handlers{Permission: allow}, Resume: "r", SessionID: "s", Title: "t"}
	live := agent.SessionInfo{Model: "new", PermissionMode: agent.PermissionPlan}
	got := InheritConfig(agent.Config{Effort: agent.EffortHigh}, base, live)
	if got.Cwd != "/w" || got.Model != "new" || got.Effort != agent.EffortHigh || got.PermissionMode != agent.PermissionPlan ||
		got.SystemPrompt != "sys" || string(got.OutputSchema) != "{}" || got.Env["A"] != "1" || len(got.UnsetEnv) != 1 || got.Handlers.Permission == nil ||
		got.Resume != "" || got.SessionID != "" || got.Title != "" {
		t.Errorf("inherited = %+v", got)
	}
	own := func(context.Context, *agent.UserInputRequest) agent.UserInputResponse {
		return agent.UserInputResponse{}
	}
	if got := InheritConfig(agent.Config{Handlers: agent.Handlers{UserInput: own}}, base, live); got.Handlers.Permission != nil {
		t.Error("handlers merged into a config that sets its own")
	}
}
