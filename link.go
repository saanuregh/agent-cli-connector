package agent

import (
	"context"

	"github.com/saanuregh/agent-cli-connector/internal/agentlink"
)

func init() {
	agentlink.NewTurn = func(id string) (any, func(any, error)) {
		t, complete := newTurn(id)
		return t, func(res any, err error) {
			r, _ := res.(*TurnResult)
			complete(r, err)
		}
	}
	agentlink.SetTurnID = func(t any, id string) { setTurnID(t.(*Turn), id) }
	agentlink.TurnEvent = func(t, ev any) { t.(*Turn).addEvent(ev.(Event)) }
	agentlink.NormalizeTurnResult = func(res any) { normalizeTurnResult(res.(*TurnResult)) }
	agentlink.NewPermissionRequest = func(req, respond any) any {
		return newPermissionRequest(req.(PermissionRequest), respond.(func(context.Context, PermissionResponse) error))
	}
	agentlink.NewUserInputRequest = func(req, respond any) any {
		return newUserInputRequest(req.(UserInputRequest), respond.(func(context.Context, UserInputResponse) error))
	}
	agentlink.NewElicitationRequest = func(req, respond any) any {
		return newElicitationRequest(req.(ElicitationRequest), respond.(func(context.Context, ElicitationResponse) error))
	}
	agentlink.WithdrawRequest = func(r any) { withdrawRequest(r.(Request)) }
}
