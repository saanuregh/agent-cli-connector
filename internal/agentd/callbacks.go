package agentd

import (
	"context"
	"encoding/json"
	"fmt"

	agent "github.com/saanuregh/agent-cli-connector"
)

// cancelMethod is the notification agentd sends when it stops waiting for
// a reverse request, such as when the harness withdraws a permission
// request. Its params are {"id": <the request's JSON-RPC ID>}.
const cancelMethod = "$/cancelRequest"

// call sends a reverse request to the client. When ctx ends first, the
// client is told with $/cancelRequest.
func (s *server) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	data, err := s.encode(params)
	if err != nil {
		return nil, err
	}
	return s.conn.CallCancelable(ctx, method, data, cancelMethod)
}

// callInto sends a reverse request and decodes its result into T.
func callInto[T any](ctx context.Context, s *server, method string, params any) (T, error) {
	var out T
	raw, err := s.call(ctx, method, params)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("%s result: %w", method, err)
	}
	return out, nil
}

// callOr sends a reverse request and returns its result, or fallback's
// answer when the client cannot be reached or answers with an error.
func callOr[T any](ctx context.Context, s *server, method string, params any, fallback func(error) T) T {
	resp, err := callInto[T](ctx, s, method, params)
	if err != nil {
		return fallback(err)
	}
	return resp
}

// bindCallbacks sets the config's handlers, tool handlers, and wire tap to
// reach the client, as the start params ask. When the client cannot be
// reached or answers with an error, a permission is denied, a question or
// elicitation is cancelled, and a tool call fails, so nothing is allowed
// by accident and no turn waits forever.
func (s *server) bindCallbacks(session, provider string, p StartParams, cfg *agent.Config) {
	if p.Handlers.Permission {
		cfg.Handlers.Permission = func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			return callOr(ctx, s, "handler/permission", PermissionCall{Session: session, Request: req}, func(err error) agent.PermissionResponse {
				return agent.PermissionResponse{Decision: agent.DecisionDeny, Message: "The client did not answer the permission request: " + err.Error()}
			})
		}
	}
	if p.Handlers.UserInput {
		cfg.Handlers.UserInput = func(ctx context.Context, req *agent.UserInputRequest) agent.UserInputResponse {
			return callOr(ctx, s, "handler/userInput", UserInputCall{Session: session, Request: req}, func(error) agent.UserInputResponse {
				return agent.UserInputResponse{Cancelled: true}
			})
		}
	}
	if p.Handlers.Elicitation {
		cfg.Handlers.Elicitation = func(ctx context.Context, req *agent.ElicitationRequest) agent.ElicitationResponse {
			return callOr(ctx, s, "handler/elicitation", ElicitationCall{Session: session, Request: req}, func(error) agent.ElicitationResponse {
				return agent.ElicitationResponse{Action: agent.ElicitationCancel}
			})
		}
	}
	for i := range cfg.Tools {
		name := cfg.Tools[i].Name
		cfg.Tools[i].Handler = func(ctx context.Context, call agent.ToolCallRequest) (agent.ToolResult, error) {
			return callInto[agent.ToolResult](ctx, s, "tool/call", ToolCall{Session: session, Tool: name, Call: call})
		}
	}
	if p.WireTap {
		cfg.WireTap = s.wireTap(session, provider)
	}
}
