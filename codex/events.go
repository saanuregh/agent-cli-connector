package codex

import agent "github.com/saanuregh/agent-cli-connector"

// Codex-specific events. They are delivered on Session.Events next to the
// uniform events.

// DiffUpdated carries the aggregated unified diff of the current turn.
type DiffUpdated struct {
	agent.EventMeta
	Diff string
}

// OtherRateLimitUpdated reports the usage windows of an allowance other
// than the main Codex one, such as a model-specific allowance. The main
// allowance arrives as agent.RateLimitUpdated.
type OtherRateLimitUpdated struct {
	agent.EventMeta
	LimitID   string
	LimitName string
	Limits    []agent.RateLimit
}

// TerminalInteraction reports input written to the stdin of a running
// command: ItemID is the commandExecution item and ProcessID its process.
type TerminalInteraction struct {
	agent.EventMeta
	ItemID    string
	ProcessID string
	Stdin     string
}
