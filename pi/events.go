package pi

import (
	"encoding/json"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Pi-specific events. They are delivered on Session.Events next to the
// uniform events.

// QueueUpdated carries the complete pending steering and follow-up queues.
type QueueUpdated struct {
	agent.EventMeta
	Steering []string
	FollowUp []string
}

// UIUpdate is a fire-and-forget extension UI request: "setStatus",
// "setWidget", "setTitle", or "set_editor_text". Fields holds the request.
type UIUpdate struct {
	agent.EventMeta
	Method string
	Fields json.RawMessage
}

// ExtensionError reports an extension handler that threw.
type ExtensionError struct {
	agent.EventMeta
	ExtensionPath string
	Event         string
	Error         string
}

// EntryAppended reports a custom session entry an extension appended.
type EntryAppended struct {
	agent.EventMeta
	Entry json.RawMessage
}
