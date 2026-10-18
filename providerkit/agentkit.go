package providerkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/agentlink"
	"github.com/saanuregh/agent-cli-connector/internal/envutil"
)

// This file holds what provider implementations need from package agent
// that library users do not: request constructors and state changes,
// environment helpers, and tool invocation, plus the turn helpers Runtime
// uses.

// newTurn returns a pending turn and the function that completes it. The
// complete function may be called more than once; only the first call has
// an effect.
func newTurn(id string) (*agent.Turn, func(*agent.TurnResult, error)) {
	t, complete := agentlink.NewTurn(id)
	return t.(*agent.Turn), func(res *agent.TurnResult, err error) { complete(res, err) }
}

// setTurnID replaces a turn's provisional ID with the harness's ID.
func setTurnID(t *agent.Turn, id string) { agentlink.SetTurnID(t, id) }

// addTurnEvent records ev as an event of turn t for Turn.Events. Runtime
// does this for every event it delivers with the turn's ID.
func addTurnEvent(t *agent.Turn, ev agent.Event) { agentlink.TurnEvent(t, ev) }

// NewPermissionRequest attaches a responder to req.
func NewPermissionRequest(req agent.PermissionRequest, respond func(context.Context, agent.PermissionResponse) error) *agent.PermissionRequest {
	return agentlink.NewPermissionRequest(req, respond).(*agent.PermissionRequest)
}

// NewUserInputRequest attaches a responder to req.
func NewUserInputRequest(req agent.UserInputRequest, respond func(context.Context, agent.UserInputResponse) error) *agent.UserInputRequest {
	return agentlink.NewUserInputRequest(req, respond).(*agent.UserInputRequest)
}

// NewElicitationRequest attaches a responder to req.
func NewElicitationRequest(req agent.ElicitationRequest, respond func(context.Context, agent.ElicitationResponse) error) *agent.ElicitationRequest {
	return agentlink.NewElicitationRequest(req, respond).(*agent.ElicitationRequest)
}

// WithdrawRequest marks a request as withdrawn by the harness without
// sending an answer.
func WithdrawRequest(r agent.Request) { agentlink.WithdrawRequest(r) }

// Unsupported returns an *agent.UnsupportedError.
func Unsupported(provider, feature string) error {
	return &agent.UnsupportedError{Provider: provider, Feature: feature}
}

// EnvList converts a map to "KEY=value" entries in a stable order.
func EnvList(env map[string]string) []string { return envutil.List(env) }

// MergeEnv returns base with the "KEY=value" entries of overrides applied.
// Later entries win.
func MergeEnv(base, overrides []string) []string { return envutil.Merge(base, overrides) }

// UnsetEnv returns base without the variables named in names.
func UnsetEnv(base, names []string) []string { return envutil.Unset(base, names) }

// isExecutableNotFound reports whether err means the harness executable
// was not found.
func isExecutableNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// CallTool runs t's handler and converts a handler error or panic into an
// error result.
func CallTool(ctx context.Context, t agent.Tool, call agent.ToolCallRequest) (res agent.ToolResult) {
	if t.Handler == nil {
		return agent.ErrorResult("tool " + t.Name + " has no handler")
	}
	defer func() {
		if p := recover(); p != nil {
			res = agent.ErrorResult(fmt.Sprintf("tool %s panicked: %v", t.Name, p))
		}
	}()
	res, err := t.Handler(ctx, call)
	if err != nil {
		return agent.ErrorResult(err.Error())
	}
	return res
}
