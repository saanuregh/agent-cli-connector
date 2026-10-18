// Package agentlink lets providerkit reach the unexported parts of package
// agent's turn and request types without exporting them to library users.
// Package agent fills in the functions when it is initialized; providerkit
// wraps them with typed functions.
package agentlink

var (
	NewTurn               func(id string) (turn any, complete func(result any, err error))
	SetTurnID             func(turn any, id string)
	TurnEvent             func(turn, event any)
	NormalizeTurnResult   func(result any)
	NewPermissionRequest  func(req, respond any) any
	NewUserInputRequest   func(req, respond any) any
	NewElicitationRequest func(req, respond any) any
	WithdrawRequest       func(req any)
)
