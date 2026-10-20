package agentd

import (
	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// eventTypes lists every event type agentd can forward, one zero value
// each, so the protocol schema can describe them. TestEventTypesComplete
// keeps it in step with the library.
var eventTypes = []agent.Event{
	&agent.Compaction{},
	&agent.ConversationReset{},
	&agent.ElicitationRequest{},
	&agent.EventsDropped{},
	&agent.GoalUpdated{},
	&agent.HookRun{},
	&agent.ItemCompleted{},
	&agent.ItemStarted{},
	&agent.ItemUpdated{},
	&agent.ItemsRemoved{},
	&agent.Notice{},
	&agent.PermissionRequest{},
	&agent.PlanDelta{},
	&agent.PlanUpdated{},
	&agent.RateLimitUpdated{},
	&agent.RawEvent{},
	&agent.ReasoningDelta{},
	&agent.RequestResolved{},
	&agent.Retry{},
	&agent.SessionStarted{},
	&agent.SessionUpdated{},
	&agent.StatusChanged{},
	&agent.TaskUpdated{},
	&agent.TextDelta{},
	&agent.ToolInputDelta{},
	&agent.ToolOutputDelta{},
	&agent.TurnCompleted{},
	&agent.TurnStarted{},
	&agent.UsageUpdated{},
	&agent.UserInputRequest{},
	&claude.BackgroundTasksChanged{},
	&claude.CommandLifecycle{},
	&claude.DeferredToolUse{},
	&claude.PromptSuggestion{},
	&claude.ToolProgress{},
	&claude.ToolUseSummary{},
	&codex.DiffUpdated{},
	&codex.OtherRateLimitUpdated{},
	&codex.TerminalInteraction{},
	&pi.EntryAppended{},
	&pi.ExtensionError{},
	&pi.QueueUpdated{},
	&pi.UIUpdate{},
}

// EventTypes returns the event table. TurnCompleted is encoded as
// TurnCompletedWire.
func EventTypes() []agent.Event { return eventTypes }
