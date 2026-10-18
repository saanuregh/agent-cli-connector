package agent_test

import (
	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/opencode"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// The optional session interfaces each provider documents. A provider that
// drops one fails to compile here.
var (
	_ interface {
		agent.Titler
		agent.Compacter
		agent.Forker
		agent.Rewinder
		agent.TaskStopper
		agent.TurnInterrupter
	} = (*claude.Session)(nil)
	_ interface {
		agent.Titler
		agent.Compacter
		agent.Forker
		agent.Rewinder
		agent.TaskStopper
		agent.TurnInterrupter
	} = (*codex.Session)(nil)
	_ interface {
		agent.Titler
		agent.Compacter
		agent.Forker
		agent.Rewinder
		agent.TaskStopper
		agent.TurnInterrupter
	} = (*opencode.Session)(nil)
	_ interface {
		agent.Titler
		agent.Compacter
		agent.Forker
		agent.Rewinder
		agent.TurnInterrupter
	} = (*pi.Session)(nil)
)
