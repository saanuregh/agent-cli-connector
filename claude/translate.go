package claude

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

// translator turns stdout messages into events. It runs on the reader
// goroutine, except for the fields guarded by taskMu and mu, which other
// goroutines also use.
type translator struct {
	s *Session

	// blocks maps "messageID/index" to the item a stream content block
	// belongs to.
	blocks map[string]blockRef
	// streamMessage is the message being streamed per parent tool call
	// ("" for the main conversation).
	streamMessage map[string]string
	// frames counts assistant frames per message ID; frame n carries
	// content block n.
	frames map[string]messageFrames
	// tools holds tool calls waiting for their results.
	tools map[string]*agent.Item
	// toolTurn maps a tool call to the turn it started in, for calls that
	// finish later, such as background Agent calls.
	toolTurn map[string]string
	tasks    map[string]*agent.Task
	// agents maps a spawning tool call to the subagent it started.
	agents map[string]string
	// plan mirrors the TaskCreate/TaskUpdate task list.
	plan []agent.PlanStep

	startedTurns map[string]bool
	lastInit     string

	// pendingSubagentItems holds items of subagent frames that arrived
	// before task_started named their subagent, by parent tool call;
	// subagentModels is the model of such a subagent's first frame.
	pendingSubagentItems map[string][]heldItem
	subagentModels       map[string]string
	// tails are the running shell output tails by tool call.
	tailMu sync.Mutex
	tails  map[string]*outputTail
	// turnError is the API error class of the current turn's assistant
	// error frame; limitResetsAt is when the latest exhausted rate limit
	// resets.
	turnError     string
	limitResetsAt time.Time

	// turnUsage accumulates the main conversation's usage of the current
	// turn; contextWindow and contextTokens are the latest known values.
	// request holds the counters of the main model request being streamed,
	// and mainModel the model main assistant frames report.
	turnUsage     agent.Usage
	contextWindow int64
	contextTokens int64
	request       requestUsage
	mainModel     string
	// hidden holds SubagentHandback calls, which become assistant messages
	// instead of tool calls.
	hidden map[string]bool
	// compacting is set between the first "compacting" status and the end
	// of that compaction.
	compacting bool
	// sessionMissing records a result saying the resumed session does not
	// exist, and dropsTurnRejected one refusing a --resume-drops-turn
	// resume.
	sessionMissing    atomic.Bool
	dropsTurnRejected atomic.Bool
	// turnErrorText is the text of the current turn's assistant error
	// frame, and pluginErrors the latest reported plugin load errors.
	turnErrorText string
	pluginErrors  string

	// taskMu guards tasks and pendingEffort, which effort hooks update
	// from another goroutine. pendingEffort holds effort reported before
	// the subagent's task_started.
	taskMu        sync.Mutex
	pendingEffort map[string]agent.Effort

	// mu guards lastCost, the Rewind lookups below, and overageLimit,
	// which Rewind and control calls use from other goroutines.
	mu       sync.Mutex
	lastCost float64
	// messageItems maps a wire message UUID to the items it produced and
	// itemMessage maps back; cursors maps a turn to its latest main
	// assistant message UUID. They grow with the conversation, because
	// Rewind resolves any of its items and turns, and are cleared when
	// the conversation is reset.
	messageItems map[string][]string
	itemMessage  map[string]string
	cursors      map[string]string
	// overageLimit names the model-scoped allowance get_usage reported,
	// which rate-limit events call "seven_day_overage_included".
	overageLimit string
}

type blockRef struct {
	itemID           string
	kind             agent.ItemKind
	parent, subagent string
}

// messageFrames counts the assistant frames of one message and records
// the parent tool call of its thread.
type messageFrames struct {
	n      int
	parent string
}

// heldItem is an item of a subagent frame that arrived before
// task_started named its subagent. A done item's ItemCompleted waits for
// the subagent, so the completion carries its SubagentID and stays final.
type heldItem struct {
	item agent.Item
	meta agent.EventMeta
	done bool
}

func newTranslator(s *Session) *translator {
	return &translator{
		s:                    s,
		blocks:               make(map[string]blockRef),
		streamMessage:        make(map[string]string),
		frames:               make(map[string]messageFrames),
		tools:                make(map[string]*agent.Item),
		tasks:                make(map[string]*agent.Task),
		agents:               make(map[string]string),
		startedTurns:         make(map[string]bool),
		pendingSubagentItems: make(map[string][]heldItem),
		subagentModels:       make(map[string]string),
		tails:                make(map[string]*outputTail),
		messageItems:         make(map[string][]string),
		itemMessage:          make(map[string]string),
		cursors:              make(map[string]string),
		toolTurn:             make(map[string]string),
		hidden:               make(map[string]bool),
		pendingEffort:        make(map[string]agent.Effort),
	}
}

// keepLookups carries the item, message, and turn lookups of the
// translator a Rewind replaced, so IDs from before the rewind still
// resolve.
func (t *translator) keepLookups(old *translator) {
	old.mu.Lock()
	defer old.mu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	maps.Copy(t.messageItems, old.messageItems)
	maps.Copy(t.itemMessage, old.itemMessage)
	maps.Copy(t.cursors, old.cursors)
}

// forget drops the lookups of rewound turns and items.
func (t *translator) forget(turns, ids []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, turn := range turns {
		delete(t.cursors, turn)
	}
	for _, id := range ids {
		if u, ok := t.itemMessage[id]; ok {
			delete(t.messageItems, u)
			delete(t.itemMessage, id)
		}
	}
}

// remember records that the wire message uuid produced item id.
func (t *translator) remember(uuid, id string) {
	if uuid == "" || id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !slices.Contains(t.messageItems[uuid], id) {
		t.messageItems[uuid] = append(t.messageItems[uuid], id)
	}
	t.itemMessage[id] = uuid
}

// messageOf returns the wire message UUID that produced a live item, or
// id itself.
func (t *translator) messageOf(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.itemMessage[id]; ok {
		return m
	}
	return id
}

// cursor returns the latest main assistant message UUID of a turn.
func (t *translator) cursor(turnID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cursors[turnID]
}

// retract emits ItemsRemoved for the items the given messages produced.
func (t *translator) retract(uuids []string, meta agent.EventMeta) {
	var ids []string
	t.mu.Lock()
	for _, u := range uuids {
		for _, id := range t.messageItems[u] {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
			delete(t.itemMessage, id)
		}
		delete(t.messageItems, u)
	}
	t.mu.Unlock()
	for _, id := range ids {
		delete(t.tools, id)
		delete(t.toolTurn, id)
		t.stopTail(id)
	}
	if len(ids) > 0 {
		t.emit(&agent.ItemsRemoved{EventMeta: meta, IDs: ids})
	}
}

// stop ends the translator's background work.
func (t *translator) stop() {
	t.tailMu.Lock()
	ids := slices.Collect(maps.Keys(t.tails))
	t.tailMu.Unlock()
	for _, id := range ids {
		t.stopTail(id)
	}
}

func (t *translator) emit(ev agent.Event) { t.s.emit(ev) }

func (t *translator) raw(env envelope, line []byte) {
	typ := env.Type
	if env.Subtype != "" {
		typ += "/" + env.Subtype
	}
	t.emit(&agent.RawEvent{EventMeta: agent.EventMeta{Raw: line}, Type: typ})
}

// moveSession records that the CLI moved the conversation to session id.
func (t *translator) moveSession(id string) {
	t.s.rt.SetSessionID(id)
	t.s.updateInfo(func(i *agent.SessionInfo) { i.ID = id })
}

func (t *translator) handle(env envelope, line []byte) {
	meta := agent.EventMeta{Raw: line}
	if env.SessionID != "" && providerutil.Deref(env.ParentToolUseID) == "" && env.SessionID != t.s.rt.SessionID() {
		// The CLI moved the conversation to a new session, such as after a
		// hook or /clear; Resume must name the new one.
		t.moveSession(env.SessionID)
	}
	switch env.Type {
	case "stream_event":
		var f streamFrame
		if json.Unmarshal(line, &f) == nil {
			t.stream(f, meta)
		}
	case "assistant":
		var f assistantFrame
		if json.Unmarshal(line, &f) == nil {
			t.assistant(f, meta)
		}
	case "user":
		var f userFrame
		if json.Unmarshal(line, &f) == nil {
			t.user(f, meta)
		}
	case "result":
		var f resultFrame
		if json.Unmarshal(line, &f) == nil {
			t.result(f, line)
		}
	case "system":
		t.system(env, line)
	case "command_lifecycle":
		var f struct {
			CommandUUID string `json:"command_uuid"`
			State       string `json:"state"`
		}
		if json.Unmarshal(line, &f) == nil {
			switch f.State {
			case "started":
				t.turnStarted(f.CommandUUID, meta)
			case "cancelled":
				t.s.rt.AbandonTurn(f.CommandUUID, agent.ErrCancelled)
			case "completed":
				// The CLI completes a message whose uuid its transcript
				// already holds without running it, and nothing will answer
				// it. A message folded into a running turn is finished by
				// that turn's result instead.
				if !t.startedTurns[f.CommandUUID] && len(t.startedTurns) == 0 && t.s.rt.HasTurn(f.CommandUUID) {
					t.s.rt.AbandonTurn(f.CommandUUID, errNotStarted)
				}
			}
		}
		t.emit(&CommandLifecycle{EventMeta: meta, CommandUUID: f.CommandUUID, State: f.State})
	case "rate_limit_event":
		t.rateLimit(line, meta)
	case "conversation_reset":
		var f struct {
			NewConversationID string `json:"new_conversation_id"`
			Trigger           string `json:"trigger"`
		}
		_ = json.Unmarshal(line, &f)
		if id := f.NewConversationID; id != "" && id != t.s.rt.SessionID() {
			t.moveSession(id)
		}
		// Rewind cannot reach the dropped conversation's turns any more.
		t.s.turnItems.reset()
		t.mu.Lock()
		clear(t.messageItems)
		clear(t.itemMessage)
		clear(t.cursors)
		t.mu.Unlock()
		t.plan = nil
		t.emit(&agent.ConversationReset{EventMeta: meta, Trigger: f.Trigger, NewSessionID: f.NewConversationID})
	case "active_goal":
		var f struct {
			Value *struct {
				Condition  string `json:"condition"`
				Iterations int    `json:"iterations"`
			} `json:"value"`
		}
		_ = json.Unmarshal(line, &f)
		ev := &agent.GoalUpdated{EventMeta: meta}
		if f.Value != nil {
			ev.Goal = &agent.Goal{Objective: f.Value.Condition, Status: "active", Iterations: f.Value.Iterations}
		}
		t.emit(ev)
	case "prompt_suggestion":
		var f struct {
			Suggestion string `json:"suggestion"`
		}
		_ = json.Unmarshal(line, &f)
		t.emit(&PromptSuggestion{EventMeta: meta, Suggestion: f.Suggestion})
	case "tool_progress":
		var f struct {
			ToolUseID       string  `json:"tool_use_id"`
			ToolName        string  `json:"tool_name"`
			ParentToolUseID *string `json:"parent_tool_use_id"`
			Elapsed         float64 `json:"elapsed_time_seconds"`
			TaskID          string  `json:"task_id"`
		}
		_ = json.Unmarshal(line, &f)
		t.emit(&ToolProgress{EventMeta: meta, ToolUseID: f.ToolUseID, ToolName: f.ToolName, ParentToolUseID: providerutil.Deref(f.ParentToolUseID),
			Elapsed: time.Duration(f.Elapsed * float64(time.Second)), TaskID: f.TaskID})
	case "tool_use_summary":
		var f struct {
			Summary string   `json:"summary"`
			IDs     []string `json:"preceding_tool_use_ids"`
		}
		_ = json.Unmarshal(line, &f)
		t.emit(&ToolUseSummary{EventMeta: meta, Summary: f.Summary, ToolUseIDs: f.IDs})
	case "auth_status":
		var f struct {
			IsAuthenticating bool     `json:"isAuthenticating"`
			Output           []string `json:"output"`
			Error            string   `json:"error"`
		}
		_ = json.Unmarshal(line, &f)
		if f.Error != "" {
			t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: "auth", Message: f.Error})
		}
		t.raw(env, line)
	default:
		t.raw(env, line)
	}
}

var errNotStarted = errors.New("claude: the CLI acknowledged the message but never started a turn for it")

// turnStarted marks a submitted message's turn as started. Messages the
// host did not send (such as automatic follow-ups) are tracked too.
func (t *translator) turnStarted(id string, meta agent.EventMeta) {
	if id == "" || t.startedTurns[id] {
		return
	}
	t.startedTurns[id] = true
	if !t.s.rt.HasTurn(id) {
		t.s.rt.TrackTurn(id)
	}
	meta.TurnID = id
	t.emit(&agent.TurnStarted{EventMeta: meta})
	t.emit(&agent.StatusChanged{EventMeta: meta, Status: agent.StatusRunning})
}

func (t *translator) stream(f streamFrame, meta agent.EventMeta) {
	parent := providerutil.Deref(f.ParentToolUseID)
	sub := t.subagentOf(parent, "")
	ev := f.Event
	switch ev.Type {
	case "message_start":
		// A thread's requests run one after another, so the previous
		// message of this thread has delivered all its frames.
		if prev, ok := t.streamMessage[parent]; ok && prev != ev.Message.ID {
			delete(t.frames, prev)
		}
		t.streamMessage[parent] = ev.Message.ID
		if parent == "" {
			t.request = requestUsage{}
			if json.Unmarshal(ev.Message.Usage, &t.request) == nil && t.request.InputTokens != nil {
				t.contextTokens = t.request.total()
				t.emitUsage(meta)
			}
		}
	case "content_block_start":
		msgID := t.streamMessage[parent]
		key := msgID + "/" + strconv.Itoa(ev.Index)
		b := ev.ContentBlock
		switch b.Type {
		case "text", "thinking", "redacted_thinking":
			kind := agent.ItemAssistantMessage
			if b.Type != "text" {
				kind = agent.ItemReasoning
			}
			id := key
			t.blocks[key] = blockRef{itemID: id, kind: kind, parent: parent, subagent: sub}
			t.emit(&agent.ItemStarted{EventMeta: meta, Item: agent.Item{ID: id, Kind: kind, Status: agent.ItemRunning, ParentID: parent, SubagentID: sub}})
			if kind == agent.ItemReasoning && b.Thinking != "" {
				t.emit(&agent.ReasoningDelta{EventMeta: meta, ItemID: id, ParentID: parent, SubagentID: sub, Delta: b.Thinking})
			}
		case "tool_use", "server_tool_use", "mcp_tool_use":
			t.blocks[key] = blockRef{itemID: b.ID, kind: agent.ItemToolCall, parent: parent, subagent: sub}
			if b.Name == handbackTool {
				t.hidden[b.ID] = true
				return
			}
			t.startTool(b, parent, sub, meta)
		}
	case "content_block_delta":
		ref, ok := t.blocks[t.streamMessage[parent]+"/"+strconv.Itoa(ev.Index)]
		if !ok {
			return
		}
		switch ev.Delta.Type {
		case "text_delta":
			t.emit(&agent.TextDelta{EventMeta: meta, ItemID: ref.itemID, ParentID: parent, SubagentID: sub, Delta: ev.Delta.Text})
		case "thinking_delta":
			t.emit(&agent.ReasoningDelta{EventMeta: meta, ItemID: ref.itemID, ParentID: parent, SubagentID: sub, Delta: ev.Delta.Thinking})
		case "input_json_delta":
			if t.hidden[ref.itemID] {
				return
			}
			t.emit(&agent.ToolInputDelta{EventMeta: meta, ItemID: ref.itemID, ParentID: parent, SubagentID: sub, Delta: ev.Delta.PartialJSON})
		}
	case "message_delta":
		var api apiUsage
		if len(ev.Usage) > 0 && parent == "" && json.Unmarshal(ev.Usage, &api) == nil {
			t.turnUsage.Add(usageOf(api))
			// Delta counters are whole-request totals that replace the
			// message_start ones; some gateways only know them here.
			var next requestUsage
			_ = json.Unmarshal(ev.Usage, &next)
			t.request.merge(next)
			t.contextTokens = t.request.total()
			t.emitUsage(meta)
		}
	}
}

// emitUsage reports the turn's usage so far with the current context use.
func (t *translator) emitUsage(meta agent.EventMeta) {
	snapshot := t.turnUsage
	snapshot.ContextTokens = t.contextTokens
	snapshot.ContextWindow = t.contextWindow
	t.emit(&agent.UsageUpdated{EventMeta: meta, Usage: snapshot})
}

// hookOutcome normalizes a hook_response outcome. The CLI reports any
// non-zero exit as "error"; exit code 2 is Claude Code's blocking error,
// which stops the action the hook ran for. "cancelled" (aborted or timed
// out) counts as failed.
func hookOutcome(outcome string, exitCode *int) agent.HookOutcome {
	switch outcome {
	case "success":
		return agent.HookSucceeded
	case "error":
		if exitCode != nil && *exitCode == 2 {
			return agent.HookBlocked
		}
		return agent.HookFailed
	case "cancelled":
		return agent.HookFailed
	}
	return ""
}

// handbackTool is the tool a subagent calls to deliver its final report
// (Claude Code 2.1.271 and later, in auto mode).
const handbackTool = "SubagentHandback"

func usageOf(u apiUsage) agent.Usage {
	return agent.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		ReasoningTokens:  u.OutputTokensDetails.ThinkingTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
	}
}

// startTool records a new tool call and announces it.
func (t *translator) startTool(b contentBlock, parent, sub string, meta agent.EventMeta) *agent.Item {
	item := t.toolItem(b, parent, sub)
	t.tools[b.ID] = item
	t.toolTurn[b.ID] = t.s.rt.CurrentTurnID()
	t.emit(&agent.ItemStarted{EventMeta: meta, Item: *item})
	return item
}

func (t *translator) toolItem(b contentBlock, parent, sub string) *agent.Item {
	call := toolCallOf(b, t.s.toolServerName())
	return &agent.Item{ID: b.ID, Kind: agent.ItemToolCall, Status: agent.ItemRunning, ParentID: parent, SubagentID: sub, Tool: call}
}

// toolCallOf describes a tool_use, server_tool_use, or mcp_tool_use block,
// for the live stream and history. An MCP connector call is named
// "mcp__<server>__<tool>" like other MCP tools.
func toolCallOf(b contentBlock, hostServer string) *agent.ToolCall {
	name := b.Name
	if b.Type == "mcp_tool_use" && b.ServerName != "" {
		name = "mcp__" + b.ServerName + "__" + b.Name
	}
	call := &agent.ToolCall{Name: name, Kind: agent.ToolKind(toolKind(name, hostServer))}
	if call.Kind == agent.ToolMCP || call.Kind == agent.ToolHost {
		call.Server = mcpServerOf(name)
	}
	return call
}

// serverToolResultTypes are the content blocks that carry the result of a
// server-side tool call in the assistant message itself.
var serverToolResultTypes = []string{"web_search_tool_result", "web_fetch_tool_result", "mcp_tool_result", "code_execution_tool_result",
	"bash_code_execution_tool_result", "text_editor_code_execution_tool_result", "advisor_tool_result", "tool_search_tool_result"}

func (t *translator) assistant(f assistantFrame, meta agent.EventMeta) {
	parent := providerutil.Deref(f.ParentToolUseID)
	sub := t.subagentOf(parent, f.AgentID)
	msgID := f.Message.ID
	if len(f.Supersedes) > 0 {
		t.retract(f.Supersedes, meta)
	}
	if f.Error != "" {
		text := textOfBlocks(f.Message.Content)
		if parent == "" {
			t.turnError = f.Error
			t.turnErrorText = text
		}
		t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: f.Error, Message: text})
	}
	if parent == "" && f.UUID != "" {
		t.mu.Lock()
		t.cursors[t.s.rt.CurrentTurnID()] = f.UUID
		t.mu.Unlock()
	}
	if parent != "" {
		t.subagentModel(parent, sub, f.Message.Model, meta)
	} else if m := f.Message.Model; m != "" && m != "<synthetic>" {
		t.mainModel = m
	}
	for _, b := range f.Message.Content {
		n := t.frames[msgID].n
		t.frames[msgID] = messageFrames{n: n + 1, parent: parent}
		key := msgID + "/" + strconv.Itoa(n)
		ref, streamed := t.blocks[key]
		delete(t.blocks, key)
		status := agent.ItemDone
		if f.Aborted {
			status = agent.ItemCancelled
		}
		switch b.Type {
		case "text", "thinking", "redacted_thinking":
			kind := agent.ItemAssistantMessage
			text := b.Text
			if b.Type != "text" {
				kind = agent.ItemReasoning
				text = b.Thinking
			}
			id := key
			if streamed {
				id = ref.itemID
			} else {
				t.emit(&agent.ItemStarted{EventMeta: meta, Item: agent.Item{ID: id, Kind: kind, Status: agent.ItemRunning, ParentID: parent, SubagentID: sub}})
			}
			item := agent.Item{ID: id, Kind: kind, Status: status, ParentID: parent, SubagentID: sub, Text: text, Raw: providerutil.Raw(b)}
			t.remember(f.UUID, id)
			t.completeItem(item, meta)
		case "tool_use", "server_tool_use", "mcp_tool_use":
			if b.Name == handbackTool {
				t.handback(b, parent, sub, f.UUID, meta)
				continue
			}
			item, ok := t.tools[b.ID]
			if !ok {
				item = t.startTool(b, parent, sub, meta)
			}
			item.Tool.Input = b.Input
			item.Raw = providerutil.Raw(b)
			fillToolDetails(item.Tool, b.Input)
			for _, m := range f.ToolUseMeta {
				if m.ID == b.ID {
					item.Tool.DisplayName = m.DisplayName
					item.Tool.ServerDisplayName = m.ServerDisplayName
					item.Tool.IconURL = m.IconURL
				}
			}
			t.remember(f.UUID, b.ID)
			t.holdSubagentItem(parent, sub, *item)
			t.emit(&agent.ItemUpdated{EventMeta: meta, Item: item.Clone()})
			// A subagent's to-do list is its own, not the session plan.
			if b.Name == "TodoWrite" && parent == "" {
				t.emitTodos(b.ID, b.Input, meta)
			}
		default:
			if slices.Contains(serverToolResultTypes, b.Type) {
				t.remember(f.UUID, b.ToolUseID)
				t.completeTool(b.ToolUseID, b.Content, b.IsError || isErrorContent(b.Content), nil, "", meta)
			}
		}
	}
	if f.Message.StopReason != nil {
		delete(t.frames, msgID)
	}
}

// handback shows a subagent's SubagentHandback call as its final message.
// The call and its acknowledging result are not reported.
func (t *translator) handback(b contentBlock, parent, sub, uuid string, meta agent.EventMeta) {
	t.hidden[b.ID] = true
	var in struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(b.Input, &in)
	if strings.TrimSpace(in.Message) == "" {
		return
	}
	item := agent.Item{ID: b.ID, Kind: agent.ItemAssistantMessage, Status: agent.ItemDone, ParentID: parent, SubagentID: sub, Text: in.Message, Raw: providerutil.Raw(b)}
	t.remember(uuid, b.ID)
	t.completeItem(item, meta)
}

// isErrorContent reports a server tool result whose content is an error,
// such as {"type": "web_search_tool_result_error"}.
func isErrorContent(content json.RawMessage) bool {
	var c struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(content, &c) == nil && strings.HasSuffix(c.Type, "_tool_result_error")
}

// holdSubagentItem keeps a running item of a subagent frame whose
// subagent is not known yet, so task_started can fill in its SubagentID.
func (t *translator) holdSubagentItem(parent, sub string, item agent.Item) {
	if parent == "" || sub != "" {
		return
	}
	t.hold(parent, heldItem{item: item})
}

func (t *translator) hold(parent string, h heldItem) {
	held := t.pendingSubagentItems[parent]
	if i := slices.IndexFunc(held, func(x heldItem) bool { return x.item.ID == h.item.ID }); i >= 0 {
		held[i] = h
		return
	}
	t.pendingSubagentItems[parent] = append(held, h)
}

// completeItem emits ItemCompleted, or holds it while the item belongs to
// a subagent that task_started has not named yet.
func (t *translator) completeItem(item agent.Item, meta agent.EventMeta) {
	if item.ParentID != "" && item.SubagentID == "" {
		meta.TurnID = cmp.Or(meta.TurnID, t.s.eventTurn())
		t.hold(item.ParentID, heldItem{item: item, meta: meta, done: true})
		return
	}
	t.emit(&agent.ItemCompleted{EventMeta: meta, Item: item})
}

// releaseHeldItems completes the held items of a parent call whose
// subagent was never named, such as when the call ends first.
func (t *translator) releaseHeldItems(parent string) {
	for _, h := range t.pendingSubagentItems[parent] {
		if h.done {
			t.emit(&agent.ItemCompleted{EventMeta: h.meta, Item: h.item})
		}
	}
	delete(t.pendingSubagentItems, parent)
}

// forgetThread drops the per-thread state of a finished parent tool call.
func (t *translator) forgetThread(parent string) {
	t.releaseHeldItems(parent)
	delete(t.subagentModels, parent)
	delete(t.streamMessage, parent)
	for id, fr := range t.frames {
		if fr.parent == parent {
			delete(t.frames, id)
		}
	}
}

// subagentModel records the model a subagent's frames report.
func (t *translator) subagentModel(parent, sub, model string, meta agent.EventMeta) {
	if model == "" || model == "<synthetic>" {
		return
	}
	if sub == "" {
		if t.subagentModels[parent] == "" {
			t.subagentModels[parent] = model
		}
		return
	}
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	if task, ok := t.tasks[sub]; ok && task.Subagent != nil && task.Subagent.Model == "" {
		task.Subagent.Model = model
		t.emit(&agent.TaskUpdated{EventMeta: meta, Task: task.Clone()})
	}
}

// subagentEffort records the effort a subagent's hooks report. It runs on
// a hook goroutine.
func (t *translator) subagentEffort(agentID string, effort agent.Effort) {
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	task, ok := t.tasks[agentID]
	if !ok || task.Subagent == nil {
		if len(t.pendingEffort) < maxPendingEffort {
			t.pendingEffort[agentID] = effort
		}
		return
	}
	if task.Subagent.Effort == effort {
		return
	}
	task.Subagent.Effort = effort
	t.emit(&agent.TaskUpdated{Task: task.Clone()})
}

// maxPendingEffort bounds effort remembered for subagents not seen yet.
const maxPendingEffort = 256

// failRunningTasks reports the tasks still running as failed, because
// they die with the CLI process. It runs after the read loop ended.
func (t *translator) failRunningTasks() {
	t.endRunningTasks(agent.TaskFailed, "the claude process exited")
}

// endRunningTasks ends the tasks still running with status, and completes
// the spawning calls of background subagents among them. It runs after
// the read loop ended.
func (t *translator) endRunningTasks(status agent.TaskStatus, reason string) {
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(t.tasks)) {
		task := t.tasks[id]
		if task.Status != agent.TaskRunning {
			continue
		}
		task.Status = status
		task.Error = cmp.Or(task.Error, reason)
		t.emit(&agent.TaskUpdated{Task: task.Clone()})
		t.completeAsyncCall(task, agent.EventMeta{})
	}
}

// endOpenItems completes the items left open: streamed blocks that no
// assistant frame closed get blockStatus, and tool calls without a result
// are cancelled. With keepBackground, the items of background subagents,
// which outlive the turn, stay open, and so does the call keep, which the
// CLI deferred to the host.
func (t *translator) endOpenItems(blockStatus agent.ItemStatus, keepBackground bool, keep string) {
	for _, key := range slices.Sorted(maps.Keys(t.blocks)) {
		ref := t.blocks[key]
		if keepBackground && (t.inBackground(ref.parent) || t.backgroundTask(ref.subagent)) {
			continue
		}
		delete(t.blocks, key)
		if ref.kind == agent.ItemToolCall {
			continue
		}
		t.completeItem(agent.Item{ID: ref.itemID, Kind: ref.kind, Status: blockStatus, ParentID: ref.parent, SubagentID: ref.subagent}, agent.EventMeta{})
	}
	for _, id := range slices.Sorted(maps.Keys(t.tools)) {
		item, ok := t.tools[id]
		if !ok || id == keep || keepBackground && (t.inBackground(id) || t.backgroundTask(item.SubagentID)) {
			continue
		}
		t.stopTail(id)
		delete(t.tools, id)
		meta := agent.EventMeta{TurnID: t.toolTurn[id]}
		delete(t.toolTurn, id)
		t.forgetThread(id)
		item.Status = agent.ItemCancelled
		t.completeItem(*item, meta)
	}
	for _, parent := range slices.Sorted(maps.Keys(t.pendingSubagentItems)) {
		if !keepBackground || !t.inBackground(parent) {
			t.releaseHeldItems(parent)
		}
	}
	for id, fr := range t.frames {
		if !keepBackground || !t.inBackground(fr.parent) {
			delete(t.frames, id)
		}
	}
}

// inBackground reports whether parent is, or runs inside, the spawning call
// of a background subagent: a call its launch acknowledgement left open.
func (t *translator) inBackground(parent string) bool {
	for range maxSubagentDepth {
		call, ok := t.tools[parent]
		if parent == "" || !ok {
			return false
		}
		if call.Tool != nil && len(call.Tool.OutputData) > 0 {
			return true
		}
		parent = call.ParentID
	}
	return false
}

// maxSubagentDepth bounds the walk up nested subagent calls.
const maxSubagentDepth = 16

// backgroundTask reports whether id is a running background subagent.
func (t *translator) backgroundTask(id string) bool {
	if id == "" {
		return false
	}
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	task, ok := t.tasks[id]
	return ok && task.Status == agent.TaskRunning && task.Subagent != nil && task.Subagent.Background
}

func textOfBlocks(blocks []contentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (t *translator) emitTodos(itemID string, input json.RawMessage, meta agent.EventMeta) {
	if steps, ok := todoSteps(input); ok {
		t.emit(&agent.PlanUpdated{EventMeta: meta, Steps: steps, ItemID: itemID})
	}
}

// todoSteps decodes a TodoWrite input.
func todoSteps(input json.RawMessage) ([]agent.PlanStep, bool) {
	var in struct {
		Todos []struct {
			Content    string `json:"content"`
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
		} `json:"todos"`
	}
	if json.Unmarshal(input, &in) != nil {
		return nil, false
	}
	steps := make([]agent.PlanStep, 0, len(in.Todos))
	for _, td := range in.Todos {
		steps = append(steps, agent.PlanStep{Text: td.Content, Status: planStatus(td.Status)})
	}
	return steps, true
}

// fillToolDetails decodes well-known tool arguments into the uniform
// fields.
func fillToolDetails(call *agent.ToolCall, input json.RawMessage) {
	var in struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Pattern  string `json:"pattern"`
		Query    string `json:"query"`
		URL      string `json:"url"`
		Notebook string `json:"notebook_path"`
		Server   string `json:"server"`
	}
	_ = json.Unmarshal(input, &in)
	switch call.Kind {
	case agent.ToolMCP:
		// The MCP resource tools name their server in the arguments.
		if call.Server == "" {
			call.Server = in.Server
		}
	case agent.ToolShell:
		call.Command = in.Command
	case agent.ToolRead:
		call.Path = cmp.Or(in.FilePath, in.Notebook)
	case agent.ToolEdit:
		call.Path = cmp.Or(in.FilePath, in.Notebook)
		call.Changes = editChanges(call.Name, input, nil)
	case agent.ToolSearch:
		call.Query = cmp.Or(in.Pattern, in.Query)
		call.Path = in.Path
	case agent.ToolWeb:
		call.URL = in.URL
		call.Query = in.Query
	}
}

// editChanges describes the file changes of an edit tool from its input
// and, once known, its structured result.
func editChanges(tool string, input, result json.RawMessage) []agent.FileChange {
	var in struct {
		FilePath  string `json:"file_path"`
		Notebook  string `json:"notebook_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	_ = json.Unmarshal(input, &in)
	path := cmp.Or(in.FilePath, in.Notebook)
	if path == "" {
		return nil
	}
	change := agent.FileChange{Path: path, Kind: agent.FileModified}
	var res struct {
		Type            string  `json:"type"`
		OriginalFile    *string `json:"originalFile"`
		StructuredPatch []struct {
			OldStart int      `json:"oldStart"`
			OldLines int      `json:"oldLines"`
			NewStart int      `json:"newStart"`
			NewLines int      `json:"newLines"`
			Lines    []string `json:"lines"`
		} `json:"structuredPatch"`
	}
	if len(result) > 0 && json.Unmarshal(result, &res) == nil {
		if res.Type == "create" || (tool == "Write" && res.OriginalFile == nil && res.Type != "update") {
			change.Kind = agent.FileAdded
		}
		if len(res.StructuredPatch) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
			for _, h := range res.StructuredPatch {
				fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
				for _, l := range h.Lines {
					b.WriteString(l)
					b.WriteByte('\n')
				}
			}
			change.Diff = b.String()
		}
	}
	if change.Diff == "" {
		switch {
		case tool == "Write":
			change.Diff = simpleDiff(path, "", in.Content)
		case len(in.Edits) > 0:
			var b strings.Builder
			for _, e := range in.Edits {
				b.WriteString(simpleDiff(path, e.OldString, e.NewString))
			}
			change.Diff = b.String()
		case in.OldString != "" || in.NewString != "":
			change.Diff = simpleDiff(path, in.OldString, in.NewString)
		}
	}
	return []agent.FileChange{change}
}

// simpleDiff renders a replacement as a unified diff without context.
func simpleDiff(path, oldText, newText string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", min(1, len(oldLines)), len(oldLines), min(1, len(newLines)), len(newLines))
	for _, l := range oldLines {
		fmt.Fprintf(&b, "-%s\n", l)
	}
	for _, l := range newLines {
		fmt.Fprintf(&b, "+%s\n", l)
	}
	return b.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func (t *translator) user(f userFrame, meta agent.EventMeta) {
	if f.IsReplay || f.IsCompactSummary || f.IsVisibleInTranscriptOnly {
		return
	}
	parent := providerutil.Deref(f.ParentToolUseID)
	sub := t.subagentOf(parent, f.AgentID)
	var blocks []contentBlock
	if json.Unmarshal(f.Message.Content, &blocks) != nil {
		// Plain string content: a prompt the CLI added itself.
		var text string
		if json.Unmarshal(f.Message.Content, &text) == nil && text != "" && !isTranscriptNoise(text) && !isCommandEcho(text) {
			synthetic := f.IsSynthetic || isInjectedPrompt(text)
			t.completeItem(agent.Item{ID: f.UUID, Kind: agent.ItemUserMessage, Status: agent.ItemDone, ParentID: parent, SubagentID: sub, Text: text, Synthetic: synthetic}, meta)
		}
		return
	}
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			kind := ""
			for _, m := range f.ToolResultMeta {
				if m.ID == b.ToolUseID {
					kind = m.NonExecutionKind
				}
			}
			if t.hidden[b.ToolUseID] {
				delete(t.hidden, b.ToolUseID)
				continue
			}
			t.remember(f.UUID, b.ToolUseID)
			t.completeTool(b.ToolUseID, b.Content, b.IsError, f.ToolUseResult, kind, meta)
		case "text":
			texts = append(texts, b.Text)
		}
	}
	if len(texts) == 0 {
		return
	}
	text := strings.Join(texts, "\n")
	switch {
	case parent != "" && !f.IsSynthetic:
		t.completeItem(agent.Item{ID: f.UUID, Kind: agent.ItemUserMessage, Status: agent.ItemDone, ParentID: parent, SubagentID: sub, Text: text}, meta)
	case parent == "" && (f.IsSynthetic || isInjectedPrompt(text)) && !isTranscriptNoise(text) && !isCommandEcho(text):
		// The host's own prompts are not echoed; these the CLI added.
		t.emit(&agent.ItemCompleted{EventMeta: meta, Item: agent.Item{ID: f.UUID, Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: text, Synthetic: true}})
	}
}

// isInjectedPrompt reports prompt text the CLI writes itself, such as
// background task notifications.
func isInjectedPrompt(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "<task-notification>") || strings.HasPrefix(text, "<local-command-")
}

// isCommandEcho reports the CLI's wrapper around a slash command the host
// sent; the host already has that message, so it is not emitted again.
func isCommandEcho(text string) bool {
	return strings.Contains(text, "<command-name>") || strings.Contains(text, "<command-message>")
}

// completeTool finishes a tool call with its result; see applyToolResult.
func (t *translator) completeTool(id string, content json.RawMessage, isError bool, structured json.RawMessage, nonExecution string, meta agent.EventMeta) {
	item, ok := t.tools[id]
	if !ok {
		return
	}
	t.stopTail(id)
	applyToolResult(item, content, isError, structured, nonExecution)
	if item.Status == agent.ItemRunning {
		// The subagent runs on; its task notification completes the call.
		t.emit(&agent.ItemUpdated{EventMeta: meta, Item: item.Clone()})
		t.toolEffects(item, structured, meta)
		return
	}
	delete(t.tools, id)
	delete(t.toolTurn, id)
	t.forgetThread(id)
	t.completeItem(*item, meta)
	if !isError {
		t.toolEffects(item, structured, meta)
	}
}

// applyToolResult fills in a tool call's result, shared by the live stream
// and history: the output, the file changes and shell details the
// structured result (toolUseResult) adds, and the final status. A
// background Agent launch stays running. nonExecution is the CLI's reason
// a call did not run, such as "cancelled".
func applyToolResult(item *agent.Item, content json.RawMessage, isError bool, structured json.RawMessage, nonExecution string) {
	item.Tool.Output, item.Tool.Images = toolResultContent(content)
	item.Tool.IsError = isError
	if len(structured) > 0 && string(structured) != "null" {
		item.Tool.OutputData = structured
		switch item.Tool.Kind {
		case agent.ToolEdit:
			item.Tool.Changes = editChanges(item.Tool.Name, item.Tool.Input, structured)
		case agent.ToolShell:
			var sh struct {
				Stdout      *string `json:"stdout"`
				Stderr      *string `json:"stderr"`
				Interrupted bool    `json:"interrupted"`
				ExitCode    *int    `json:"exitCode"`
			}
			if json.Unmarshal(structured, &sh) == nil {
				if sh.Interrupted {
					item.Status = agent.ItemCancelled
				}
				if out := shellOutput(providerutil.Deref(sh.Stdout), providerutil.Deref(sh.Stderr)); out != "" {
					item.Tool.Output = out
				}
				item.Tool.ExitCode = sh.ExitCode
			}
		case agent.ToolSubagent:
			if !isError && isAsyncLaunch(structured) {
				return
			}
		}
	}
	if nonExecution == "cancelled" {
		item.Status = agent.ItemCancelled
	}
	if item.Status == agent.ItemRunning {
		item.Status = agent.ItemDone
		if isError {
			item.Status = agent.ItemFailed
		}
	}
}

// shellOutput joins a shell call's streams. Claude trims each stream's
// trailing newline.
func shellOutput(stdout, stderr string) string {
	if strings.TrimSpace(stdout) == "" {
		stdout = ""
	}
	if strings.TrimSpace(stderr) == "" {
		stderr = ""
	}
	if stdout != "" && stderr != "" && !strings.HasSuffix(stdout, "\n") {
		return stdout + "\n" + stderr
	}
	return stdout + stderr
}

// isAsyncLaunch reports an Agent result that only acknowledges a
// background launch.
func isAsyncLaunch(structured json.RawMessage) bool {
	var r struct {
		IsAsync bool   `json:"isAsync"`
		Status  string `json:"status"`
	}
	return json.Unmarshal(structured, &r) == nil && (r.IsAsync || r.Status == "async_launched")
}

// toolEffects updates state that tool results reveal: the task list kept by
// TaskCreate, TaskUpdate, and TaskList, and the model a subagent runs on.
func (t *translator) toolEffects(item *agent.Item, structured json.RawMessage, meta agent.EventMeta) {
	switch item.Tool.Name {
	case "Agent", "Task":
		var out struct {
			AgentID       string `json:"agentId"`
			ResolvedModel string `json:"resolvedModel"`
		}
		if json.Unmarshal(structured, &out) != nil || out.AgentID == "" {
			return
		}
		if item.Status == agent.ItemRunning {
			// Only a background subagent's frames follow its call's result.
			t.agents[item.ID] = out.AgentID
		}
		t.taskMu.Lock()
		defer t.taskMu.Unlock()
		if task, ok := t.tasks[out.AgentID]; ok && task.Subagent != nil && out.ResolvedModel != "" && task.Subagent.Model != out.ResolvedModel {
			task.Subagent.Model = out.ResolvedModel
			t.emit(&agent.TaskUpdated{EventMeta: meta, Task: task.Clone()})
		}
		return
	}
	if item.ParentID != "" {
		// A subagent's task list calls do not change the session plan.
		return
	}
	if plan, ok := applyPlanTool(t.plan, item.Tool.Name, item.Tool.Input, structured); ok {
		t.plan = plan
		t.emit(&agent.PlanUpdated{EventMeta: meta, Steps: slices.Clone(t.plan), ItemID: item.ID})
	}
}

// applyPlanTool returns the task list after a successful TaskCreate,
// TaskUpdate, or TaskList call, and false for other calls or results that
// change nothing.
func applyPlanTool(plan []agent.PlanStep, tool string, input, structured json.RawMessage) ([]agent.PlanStep, bool) {
	plan = slices.Clone(plan)
	switch tool {
	case "TaskCreate":
		var in struct {
			Subject string `json:"subject"`
		}
		var out struct {
			Task struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
			} `json:"task"`
		}
		_ = json.Unmarshal(input, &in)
		if json.Unmarshal(structured, &out) != nil || out.Task.ID == "" {
			return nil, false
		}
		return append(plan, agent.PlanStep{ID: out.Task.ID, Text: cmp.Or(out.Task.Subject, in.Subject), Status: agent.StepPending}), true
	case "TaskUpdate":
		var in struct {
			TaskID  string `json:"taskId"`
			Subject string `json:"subject"`
			Status  string `json:"status"`
		}
		if json.Unmarshal(input, &in) != nil {
			return nil, false
		}
		i := slices.IndexFunc(plan, func(st agent.PlanStep) bool { return st.ID == in.TaskID })
		if i < 0 {
			return nil, false
		}
		if in.Status == "deleted" {
			return slices.Delete(plan, i, i+1), true
		}
		if in.Subject != "" {
			plan[i].Text = in.Subject
		}
		if in.Status != "" {
			plan[i].Status = planStatus(in.Status)
		}
		return plan, true
	case "TaskList":
		var out struct {
			Tasks []struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
				Status  string `json:"status"`
			} `json:"tasks"`
		}
		if json.Unmarshal(structured, &out) != nil {
			return nil, false
		}
		plan = plan[:0]
		for _, tk := range out.Tasks {
			plan = append(plan, agent.PlanStep{ID: tk.ID, Text: tk.Subject, Status: planStatus(tk.Status)})
		}
		return plan, true
	}
	return nil, false
}

func planStatus(s string) agent.PlanStepStatus {
	switch s {
	case "in_progress":
		return agent.StepInProgress
	case "completed":
		return agent.StepCompleted
	}
	return agent.StepPending
}

func (t *translator) result(f resultFrame, line []byte) {
	meta := agent.EventMeta{Raw: line}
	if f.Subtype != "success" && slices.ContainsFunc(append([]string{f.Result}, f.Errors...), func(e string) bool { return strings.Contains(e, dropsTurnRejectedText) }) {
		// A startup refusal of the resume Rewind asked for; Rewind
		// recovers, and no turn ran.
		t.dropsTurnRejected.Store(true)
		return
	}
	t.mu.Lock()
	cost := f.TotalCostUSD - t.lastCost
	if cost < 0 {
		cost = f.TotalCostUSD
	}
	t.lastCost = f.TotalCostUSD
	t.mu.Unlock()

	usage := usageOf(f.Usage)
	usage.CostUSD = cost
	// modelUsage accumulates every model the session used, subagents
	// included; the active model's entry describes the context.
	if mu, ok := f.ModelUsage[cmp.Or(t.mainModel, t.s.Info().Model)]; ok && mu.ContextWindow > 0 {
		t.contextWindow = mu.ContextWindow
	} else if len(f.ModelUsage) > 0 {
		var widest int64
		for _, mu := range f.ModelUsage {
			widest = max(widest, mu.ContextWindow)
		}
		if widest > 0 {
			t.contextWindow = widest
		}
	}
	usage.ContextWindow = t.contextWindow
	usage.ContextTokens = t.contextTokens
	t.turnUsage = agent.Usage{}

	res := agent.TurnResult{
		Status:     agent.TurnSucceeded,
		Text:       f.Result,
		Usage:      usage,
		Duration:   time.Duration(f.DurationMS) * time.Millisecond,
		Steps:      f.NumTurns,
		StopReason: cmp.Or(providerutil.Deref(f.StopReason), f.TerminalReason),
		Raw:        line,
	}
	if f.Origin != nil {
		res.Origin = f.Origin.Kind
	}
	if len(f.StructuredOutput) > 0 && string(f.StructuredOutput) != "null" {
		res.StructuredOutput = f.StructuredOutput
	}
	if d := f.DeferredToolUse; d != nil {
		t.emit(&DeferredToolUse{EventMeta: meta, ToolUseID: d.ID, ToolName: d.Name, Input: d.Input})
	}
	status := providerutil.Deref(f.APIErrorStatus)
	// The terminal reason is authoritative; the pending flag covers CLIs
	// that predate it, and only for a turn that did not succeed.
	requested := t.s.interruptPending.Swap(false)
	interrupted := strings.HasPrefix(f.TerminalReason, "aborted") ||
		(requested && f.TerminalReason == "" && (f.IsError || f.Subtype != "success"))
	switch {
	case interrupted:
		res.Status = agent.TurnInterrupted
		res.Err = &agent.TurnError{Code: cmp.Or(f.TerminalReason, "interrupted"), Err: agent.ErrInterrupted}
	case f.IsError || f.Subtype != "success" || status == 429 || status == 529:
		res.Status = agent.TurnFailed
		errs := slices.DeleteFunc(slices.Clone(f.Errors), func(e string) bool { return strings.HasPrefix(e, "[ede_diagnostic]") })
		msg := f.Result
		if f.Subtype != "success" || msg == "" {
			msg = strings.Join(errs, "; ")
		}
		code := f.Subtype
		if code == "success" {
			code = cmp.Or(f.StartupFailure, f.TerminalReason, t.turnError, "error")
		}
		te := &agent.TurnError{Code: code, Message: msg, HTTPStatus: status}
		usageLimit := status == 429 || f.TerminalReason == "blocking_limit" || t.turnError == "rate_limit"
		lower := strings.ToLower(msg + " " + t.turnError)
		te.Retryable = usageLimit || status == 529 || status == 503 ||
			strings.Contains(lower, "overloaded") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate_limit")
		switch texts := []string{msg, t.turnErrorText}; {
		case hasAnyPrefix(texts, orgPolicyLimitPrefixes):
			// The organization turned the service off; waiting does not help.
			te.Code, te.Retryable, usageLimit = "org_policy_limit", false, false
		case hasAnyPrefix(texts, usageLimitErrorPrefixes):
			te.Code, te.Retryable, usageLimit = "usage_limit", true, true
		}
		if usageLimit {
			te.ResetsAt = t.limitResetsAt
		}
		res.Err = te
		if strings.Contains(msg, noConversationText) {
			t.sessionMissing.Store(true)
			te.Err = agent.ErrSessionNotFound
		}
	}
	t.turnError, t.turnErrorText = "", ""

	blockStatus := agent.ItemCancelled
	if res.Status == agent.TurnSucceeded {
		blockStatus = agent.ItemDone
	}
	deferred := ""
	if f.DeferredToolUse != nil {
		deferred = f.DeferredToolUse.ID
	}
	t.endOpenItems(blockStatus, true, deferred)

	ids := f.UserMessageUUIDs
	if len(ids) == 0 && f.UserMessageUUID != "" {
		ids = []string{f.UserMessageUUID}
	}
	tracked := t.s.rt.TurnIDs()
	var finish []string
	for _, id := range ids {
		if slices.Contains(tracked, id) {
			finish = append(finish, id)
		}
	}
	switch {
	case len(finish) > 0:
	case len(ids) > 0:
		// A turn the host did not start, such as an automatic follow-up.
		finish = []string{ids[len(ids)-1]}
	case len(tracked) > 0:
		finish = []string{tracked[0]}
	default:
		finish = []string{""}
	}
	for _, id := range finish {
		r := res
		r.TurnID = id
		delete(t.startedTurns, id)
		t.s.rt.FinishTurn(id, &r)
	}
	if !t.s.rt.Busy() {
		t.emit(&agent.StatusChanged{EventMeta: meta, Status: agent.StatusIdle})
	}
}

// noConversationText starts the CLI's error for a Resume target it does
// not have.
const noConversationText = "No conversation found with session ID"

// dropsTurnRejectedText starts the CLI's refusal of a truncating resume
// whose dropped range holds more than the declared turn.
const dropsTurnRejectedText = "Resume rejected by --resume-drops-turn:"

// usageLimitErrorPrefixes and orgPolicyLimitPrefixes start the CLI's
// error texts for an exhausted usage allowance and for a service the
// organization disabled, from the Agent SDK's USAGE_LIMIT_ERROR_PREFIXES
// and ORG_POLICY_LIMIT_PREFIXES.
var (
	usageLimitErrorPrefixes = []string{"You've hit your", "You've reached your", "You're out of usage credits",
		"Your org is out of usage · add funds to continue", "Your org is out of usage · contact your admin",
		"Your seat type doesn't include usage credits", "Your seat type doesn't include usage",
		"Your usage allocation has been disabled by your admin", "Your group's usage limit is set to $0",
		"Fable 5 requires usage credits", "You're out of extra usage", "Your seat type doesn't include extra usage"}
	orgPolicyLimitPrefixes = []string{"This service is disabled for your org"}
)

// hasAnyPrefix reports whether a text, ignoring leading space, starts with
// one of the prefixes.
func hasAnyPrefix(texts, prefixes []string) bool {
	for _, text := range texts {
		text = strings.TrimSpace(text)
		for _, p := range prefixes {
			if strings.HasPrefix(text, p) {
				return true
			}
		}
	}
	return false
}

func (t *translator) system(env envelope, line []byte) {
	meta := agent.EventMeta{Raw: line}
	switch env.Subtype {
	case "init":
		var f initFrame
		if json.Unmarshal(line, &f) != nil {
			return
		}
		for _, id := range f.UserMessageUUIDs {
			t.turnStarted(id, meta)
		}
		t.s.setCapabilities(f.Capabilities)
		t.applyInit(f, line)
		// The CLI repeats plugin errors in every init message.
		if key, _ := json.Marshal(f.PluginErrors); string(key) != t.pluginErrors {
			t.pluginErrors = string(key)
			for _, pe := range f.PluginErrors {
				t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeWarning, Code: "plugin_error", Message: pe.Plugin + ": " + cmp.Or(pe.Message, pe.Type)})
			}
		}
	case "status":
		var f struct {
			Status         *string `json:"status"`
			PermissionMode string  `json:"permissionMode"`
			CompactResult  string  `json:"compact_result"`
			CompactError   string  `json:"compact_error"`
		}
		_ = json.Unmarshal(line, &f)
		if f.PermissionMode != "" && agent.PermissionMode(f.PermissionMode) != t.s.Info().PermissionMode {
			t.s.updateInfo(func(i *agent.SessionInfo) { i.PermissionMode = agent.PermissionMode(f.PermissionMode) })
		}
		switch {
		case f.Status != nil && *f.Status == "compacting":
			// The CLI repeats the status while compaction runs.
			if !t.compacting {
				t.compacting = true
				t.emit(&agent.Compaction{EventMeta: meta})
			}
			t.emit(&agent.StatusChanged{EventMeta: meta, Status: agent.StatusCompacting})
		case f.CompactResult == "failed":
			t.compacting = false
			t.emit(&agent.Compaction{EventMeta: meta, Done: true, Error: cmp.Or(f.CompactError, "compaction failed")})
		case f.Status == nil || *f.Status != "compacting":
			t.compacting = false
		}
	case "compact_boundary":
		var f struct {
			Meta struct {
				Trigger    string `json:"trigger"`
				PreTokens  int64  `json:"pre_tokens"`
				PostTokens int64  `json:"post_tokens"`
			} `json:"compact_metadata"`
		}
		_ = json.Unmarshal(line, &f)
		t.compacting = false
		if f.Meta.PostTokens > 0 {
			t.contextTokens = f.Meta.PostTokens
		}
		t.emit(&agent.Compaction{EventMeta: meta, Done: true, Trigger: f.Meta.Trigger, TokensBefore: f.Meta.PreTokens, TokensAfter: f.Meta.PostTokens})
		t.emit(&agent.ItemCompleted{EventMeta: meta, Item: agent.Item{ID: env.UUID, Kind: agent.ItemCompaction, Status: agent.ItemDone, Text: f.Meta.Trigger, Raw: line}})
	case "api_retry":
		var f struct {
			Attempt    int    `json:"attempt"`
			MaxRetries int    `json:"max_retries"`
			DelayMS    int64  `json:"retry_delay_ms"`
			Error      string `json:"error"`
			Status     *int   `json:"error_status"`
		}
		_ = json.Unmarshal(line, &f)
		msg := f.Error
		ev := &agent.Retry{EventMeta: meta, Attempt: f.Attempt, MaxAttempts: f.MaxRetries, Delay: time.Duration(f.DelayMS) * time.Millisecond, Code: f.Error}
		if f.Status != nil {
			msg = fmt.Sprintf("%s (HTTP %d)", f.Error, *f.Status)
			ev.HTTPStatus = *f.Status
		}
		ev.Message = msg
		t.emit(ev)
	case "model_refusal_fallback":
		var f struct {
			Scope         string   `json:"scope"`
			FallbackModel string   `json:"fallback_model"`
			Content       string   `json:"content"`
			Retracted     []string `json:"retracted_message_uuids"`
		}
		_ = json.Unmarshal(line, &f)
		t.retract(f.Retracted, meta)
		t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeWarning, Code: "model_refusal_fallback", Message: f.Content})
		if (f.Scope == "" || f.Scope == "session") && f.FallbackModel != "" {
			t.s.updateInfo(func(i *agent.SessionInfo) { i.Model = f.FallbackModel })
		}
	case "model_refusal_no_fallback":
		var f struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(line, &f)
		t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: "model_refusal", Message: f.Content})
	case "informational":
		var f struct {
			Content string `json:"content"`
			Level   string `json:"level"`
			Tag     string `json:"tag"`
		}
		_ = json.Unmarshal(line, &f)
		level := agent.NoticeInfo
		if f.Level == "warning" || f.Level == "suggestion" {
			level = agent.NoticeWarning
		}
		t.emit(&agent.Notice{EventMeta: meta, Level: level, Code: f.Tag, Message: f.Content})
	case "elicitation_complete":
		var f struct {
			Server        string `json:"mcp_server_name"`
			ElicitationID string `json:"elicitation_id"`
		}
		_ = json.Unmarshal(line, &f)
		t.elicitationComplete(f.Server, f.ElicitationID, meta)
	case "plugin_install":
		var f struct {
			Status string `json:"status"`
			Name   string `json:"name"`
			Error  string `json:"error"`
		}
		_ = json.Unmarshal(line, &f)
		if f.Status == "failed" {
			t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: "plugin_install_failed", Message: strings.TrimPrefix(f.Name+": "+f.Error, ": ")})
		}
		t.raw(env, line)
	case "session_state_changed":
		var f struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(line, &f)
		switch f.State {
		case "idle":
			t.emit(&agent.StatusChanged{EventMeta: meta, Status: agent.StatusIdle})
		case "running":
			t.emit(&agent.StatusChanged{EventMeta: meta, Status: agent.StatusRunning})
		case "requires_action":
			t.emit(&agent.StatusChanged{EventMeta: meta, Status: agent.StatusWaiting})
		}
	case "notification":
		var f struct {
			Key      string `json:"key"`
			Text     string `json:"text"`
			Priority string `json:"priority"`
		}
		_ = json.Unmarshal(line, &f)
		level := agent.NoticeInfo
		if f.Priority == "high" || f.Priority == "immediate" {
			level = agent.NoticeWarning
		}
		t.emit(&agent.Notice{EventMeta: meta, Level: level, Code: f.Key, Message: f.Text})
	case "permission_denied":
		var f struct {
			ToolName  string `json:"tool_name"`
			ToolUseID string `json:"tool_use_id"`
			Message   string `json:"message"`
			Reason    string `json:"decision_reason"`
		}
		_ = json.Unmarshal(line, &f)
		if item, ok := t.tools[f.ToolUseID]; ok {
			item.Status = agent.ItemDeclined
		}
		t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeWarning, Code: "permission_denied", Message: cmp.Or(f.Message, f.Reason, f.ToolName+" was denied")})
	case "task_started", "task_progress", "task_updated", "task_notification":
		t.task(env.Subtype, line, meta)
	case "commands_changed":
		var f struct {
			Commands []slashCommandWire `json:"commands"`
		}
		if json.Unmarshal(line, &f) == nil {
			cmds := convertCommands(f.Commands)
			t.s.updateInfo(func(i *agent.SessionInfo) { i.Commands = cmds })
		}
	case "hook_started", "hook_progress", "hook_response":
		var f struct {
			HookID    string `json:"hook_id"`
			HookName  string `json:"hook_name"`
			HookEvent string `json:"hook_event"`
			Output    string `json:"output"`
			Stdout    string `json:"stdout"`
			Stderr    string `json:"stderr"`
			Outcome   string `json:"outcome"`
			ExitCode  *int   `json:"exit_code"`
		}
		_ = json.Unmarshal(line, &f)
		t.emit(&agent.HookRun{
			EventMeta: meta,
			ID:        f.HookID,
			Event:     f.HookEvent,
			Name:      f.HookName,
			Done:      env.Subtype == "hook_response",
			Status:    f.Outcome,
			Outcome:   hookOutcome(f.Outcome, f.ExitCode),
			Output:    cmp.Or(f.Output, f.Stdout, f.Stderr),
		})
	case "local_command_output":
		var f struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(line, &f)
		id := env.UUID
		t.emit(&agent.ItemCompleted{EventMeta: meta, Item: agent.Item{ID: id, Kind: agent.ItemAssistantMessage, Status: agent.ItemDone, Text: f.Content}})
	case "background_tasks_changed":
		var f struct {
			Tasks []BackgroundTask `json:"tasks"`
		}
		_ = json.Unmarshal(line, &f)
		t.emit(&BackgroundTasksChanged{EventMeta: meta, Tasks: f.Tasks})
	default:
		t.raw(env, line)
	}
}

// elicitationComplete settles the url-mode elicitation an MCP server
// reported complete: a pending request is accepted on the user's behalf,
// since the user finished it in the browser. Without one it is reported
// as a Notice.
func (t *translator) elicitationComplete(server, id string, meta agent.EventMeta) {
	for _, req := range t.s.rt.PendingRequests() {
		if e, ok := req.(*agent.ElicitationRequest); ok && id != "" && e.ElicitationID == id && (server == "" || e.Server == server) {
			// Answering writes to the CLI and never waits for its reply,
			// so it is safe on the reader goroutine.
			if e.Accept(t.s.rt.Context(), nil) == nil {
				return
			}
		}
	}
	t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeInfo, Code: "elicitation_complete", Message: strings.TrimPrefix(server+": elicitation "+id+" completed", ": ")})
}

func (t *translator) applyInit(f initFrame, line []byte) {
	tools := f.Tools
	servers := make([]agent.MCPServerStatus, 0, len(f.MCPServers))
	for _, m := range f.MCPServers {
		servers = append(servers, agent.MCPServerStatus{Name: m.Name, Status: m.Status})
	}
	key, _ := json.Marshal([]any{f.Model, f.PermissionMode, f.Cwd, tools, servers, f.Version, f.Effort})
	if string(key) == t.lastInit {
		return
	}
	t.lastInit = string(key)
	t.s.updateInfo(func(i *agent.SessionInfo) {
		if f.Model != "" {
			i.Model = f.Model
		}
		if f.PermissionMode != "" {
			i.PermissionMode = agent.PermissionMode(f.PermissionMode)
		}
		if f.Cwd != "" {
			i.Cwd = f.Cwd
		}
		if f.Version != "" {
			i.Version = f.Version
		}
		if f.Effort != nil {
			i.Effort = agent.Effort(*f.Effort)
		}
		i.Tools = tools
		i.MCPServers = servers
	})
}

// subagentOf returns the subagent producing a frame: the frame's own
// agent_id, or the agent started by its parent tool call.
func (t *translator) subagentOf(parent, agentID string) string {
	if agentID != "" {
		if parent != "" {
			t.agents[parent] = agentID
		}
		return agentID
	}
	if parent == "" {
		return ""
	}
	return t.agents[parent]
}

func taskKind(native string) agent.TaskKind {
	switch native {
	case "local_agent", "remote_agent", "in_process_teammate":
		return agent.TaskSubagent
	case "local_bash":
		return agent.TaskShell
	case "local_workflow":
		return agent.TaskWorkflow
	case "":
		return ""
	}
	return agent.TaskOther
}

type taskFrame struct {
	TaskID       string `json:"task_id"`
	ToolUseID    string `json:"tool_use_id"`
	Description  string `json:"description"`
	TaskType     string `json:"task_type"`
	SubagentType string `json:"subagent_type"`
	ParentTaskID string `json:"parent_task_id"`
	Prompt       string `json:"prompt"`
	WorkflowName string `json:"workflow_name"`
	Backgrounded *bool  `json:"is_backgrounded"`
	SpawnDepth   int    `json:"spawn_depth"`
	Status       string `json:"status"`
	Summary      string `json:"summary"`
	LastToolName string `json:"last_tool_name"`
	Usage        *struct {
		TotalTokens int64 `json:"total_tokens"`
		ToolUses    int   `json:"tool_uses"`
		DurationMS  int64 `json:"duration_ms"`
	} `json:"usage"`
	WorkflowProgress []workflowEntry `json:"workflow_progress"`
	SkipTranscript   bool            `json:"skip_transcript"`
	Ambient          bool            `json:"ambient"`
	OutputFile       string          `json:"output_file"`
	HandbackReport   *struct {
		Text    string `json:"text"`
		Warning string `json:"warning"`
	} `json:"handback_report"`
	Patch struct {
		Status         string `json:"status"`
		Description    string `json:"description"`
		Error          string `json:"error"`
		IsBackgrounded *bool  `json:"is_backgrounded"`
	} `json:"patch"`
}

func (t *translator) task(kind string, line []byte, meta agent.EventMeta) {
	var f taskFrame
	if json.Unmarshal(line, &f) != nil || f.TaskID == "" {
		return
	}
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	task, ok := t.tasks[f.TaskID]
	if !ok {
		task = &agent.Task{ID: f.TaskID, Status: agent.TaskRunning}
		t.tasks[f.TaskID] = task
	}
	if f.TaskType != "" {
		task.NativeKind = f.TaskType
		task.Kind = taskKind(f.TaskType)
	}
	if f.SubagentType != "" && task.Kind == "" {
		task.Kind = agent.TaskSubagent
	}
	if task.Kind == "" {
		task.Kind = agent.TaskOther
	}
	if f.SkipTranscript || f.Ambient {
		task.Hidden = true
	}
	if kind == "task_progress" {
		// A progress description is the latest step, not the title.
		if f.Summary == "" && f.Description != "" {
			task.Summary = f.Description
		}
		task.Description = cmp.Or(task.Description, f.Description)
	} else {
		task.Description = cmp.Or(f.Description, f.Patch.Description, task.Description)
	}
	newAgent := false
	if f.ToolUseID != "" {
		task.ToolCallID = f.ToolUseID
		if task.Kind == agent.TaskSubagent && t.agents[f.ToolUseID] != f.TaskID {
			t.agents[f.ToolUseID] = f.TaskID
			newAgent = true
		}
	}
	task.ParentID = cmp.Or(f.ParentTaskID, task.ParentID)
	if task.Kind == agent.TaskSubagent {
		if task.Subagent == nil {
			task.Subagent = &agent.Subagent{}
		}
		sa := task.Subagent
		sa.Type = cmp.Or(f.SubagentType, sa.Type)
		sa.Prompt = cmp.Or(f.Prompt, sa.Prompt)
		if f.SpawnDepth > 0 {
			sa.Depth = f.SpawnDepth
		}
		if f.Backgrounded != nil {
			sa.Background = *f.Backgrounded
		}
		if f.Patch.IsBackgrounded != nil {
			sa.Background = *f.Patch.IsBackgrounded
		}
		if e, ok := t.pendingEffort[f.TaskID]; ok {
			sa.Effort = e
			delete(t.pendingEffort, f.TaskID)
		}
		if newAgent || (kind == "task_started" && task.ToolCallID != "") {
			t.bindSubagent(task, meta)
		}
	}
	if len(f.WorkflowProgress) > 0 {
		task.Workflow = mergeWorkflow(task.Workflow, f.WorkflowProgress)
	}
	if f.WorkflowName != "" && task.Description == "" {
		task.Description = f.WorkflowName
	}
	if f.Usage != nil {
		task.Usage = &agent.Usage{TotalTokens: f.Usage.TotalTokens}
		task.ToolUses = f.Usage.ToolUses
		task.Duration = time.Duration(f.Usage.DurationMS) * time.Millisecond
	}
	if f.LastToolName != "" {
		task.LastTool = f.LastToolName
	}
	status := f.Status
	switch kind {
	case "task_updated":
		status = f.Patch.Status
		if f.Patch.Error != "" {
			task.Error = f.Patch.Error
		}
	case "task_progress":
		if f.Summary != "" {
			task.Summary = f.Summary
		}
	case "task_started":
		if task.Kind == agent.TaskShell && (f.Backgrounded == nil || !*f.Backgrounded) && f.ToolUseID != "" {
			t.startTail(f.ToolUseID, f.TaskID)
		}
	case "task_notification":
		// The notification's summary is the task's final report, unless a
		// hand-back report or a workflow result replaces it.
		switch {
		case f.HandbackReport != nil && f.HandbackReport.Text != "":
			task.Output = f.HandbackReport.Text
			if f.HandbackReport.Warning != "" {
				task.Output = f.HandbackReport.Warning + "\n\n" + task.Output
			}
		case f.Summary != "":
			task.Output = f.Summary
		}
		if task.Kind == agent.TaskWorkflow && f.OutputFile != "" {
			if out := readWorkflowOutput(f.OutputFile); out != "" {
				task.Output = out
			}
		}
	}
	switch status {
	case "completed":
		task.Status = agent.TaskCompleted
	case "failed":
		task.Status = agent.TaskFailed
	case "stopped", "killed":
		task.Status = agent.TaskStopped
	case "running", "pending", "paused":
		task.Status = agent.TaskRunning
	}
	t.emit(&agent.TaskUpdated{EventMeta: meta, Task: task.Clone()})
	if task.Status != agent.TaskRunning {
		t.completeAsyncCall(task, meta)
	}
	if task.Status != agent.TaskRunning && kind == "task_notification" {
		delete(t.tasks, f.TaskID)
		if t.agents[task.ToolCallID] == task.ID {
			delete(t.agents, task.ToolCallID)
		}
	}
}

// bindSubagent fills in what was learned about a subagent before its
// task_started: the model and effort from the spawning call's input or its
// first frame, and the SubagentID of items it already produced.
func (t *translator) bindSubagent(task *agent.Task, meta agent.EventMeta) {
	sa := task.Subagent
	if call, ok := t.tools[task.ToolCallID]; ok && call.Tool != nil {
		var in struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
			Name   string `json:"name"`
		}
		if json.Unmarshal(call.Tool.Input, &in) == nil {
			sa.Name = cmp.Or(sa.Name, in.Name)
			if sa.Model == "" && in.Model != "inherit" {
				sa.Model = in.Model
			}
			if sa.Effort == "" {
				sa.Effort = agent.Effort(in.Effort)
			}
		}
	}
	if m := t.subagentModels[task.ToolCallID]; m != "" {
		sa.Model = m
	}
	delete(t.subagentModels, task.ToolCallID)
	for _, it := range t.tools {
		if it.ParentID == task.ToolCallID && it.SubagentID == "" {
			it.SubagentID = task.ID
		}
	}
	// Held completions are announced now, with the subagent; running calls
	// were announced already and are updated.
	for _, h := range t.pendingSubagentItems[task.ToolCallID] {
		if h.done {
			h.item.SubagentID = task.ID
			t.emit(&agent.ItemCompleted{EventMeta: h.meta, Item: h.item})
		} else if call, ok := t.tools[h.item.ID]; ok {
			t.emit(&agent.ItemUpdated{EventMeta: meta, Item: call.Clone()})
		}
	}
	delete(t.pendingSubagentItems, task.ToolCallID)
}

// completeAsyncCall completes a background subagent's spawning call, which
// its launch acknowledgement left running.
func (t *translator) completeAsyncCall(task *agent.Task, meta agent.EventMeta) {
	call, ok := t.tools[task.ToolCallID]
	if !ok || task.Kind != agent.TaskSubagent || call.Tool == nil || len(call.Tool.OutputData) == 0 {
		return
	}
	delete(t.tools, task.ToolCallID)
	delete(t.agents, task.ToolCallID)
	t.forgetThread(task.ToolCallID)
	// The call belongs to the turn that made it, not the one running now.
	meta.TurnID = t.toolTurn[task.ToolCallID]
	delete(t.toolTurn, task.ToolCallID)
	call.Status = agent.ItemDone
	switch task.Status {
	case agent.TaskFailed:
		call.Status = agent.ItemFailed
		call.Tool.IsError = true
	case agent.TaskStopped:
		call.Status = agent.ItemCancelled
	}
	call.Tool.Output = cmp.Or(task.Output, task.Error, call.Tool.Output)
	t.emit(&agent.ItemCompleted{EventMeta: meta, Item: call.Clone()})
}

type workflowEntry struct {
	Type          string `json:"type"`
	Index         *int   `json:"index"`
	Title         string `json:"title"`
	Label         string `json:"label"`
	State         string `json:"state"`
	AgentID       string `json:"agentId"`
	PhaseIndex    int    `json:"phaseIndex"`
	Model         string `json:"model"`
	Attempt       int    `json:"attempt"`
	Tokens        int64  `json:"tokens"`
	ToolCalls     int    `json:"toolCalls"`
	DurationMS    int64  `json:"durationMs"`
	PromptPreview string `json:"promptPreview"`
	ResultPreview string `json:"resultPreview"`
}

// mergeWorkflow applies a workflow_progress snapshot to the known phases
// and agents, keyed by index. An agent's older attempt does not replace a
// newer one.
func mergeWorkflow(prev *agent.Workflow, entries []workflowEntry) *agent.Workflow {
	w := &agent.Workflow{}
	if prev != nil {
		w.Phases = slices.Clone(prev.Phases)
		w.Agents = slices.Clone(prev.Agents)
	}
	for _, e := range entries {
		if e.Index == nil {
			continue
		}
		switch e.Type {
		case "workflow_phase":
			ph := agent.WorkflowPhase{Index: *e.Index, Title: e.Title}
			if i := slices.IndexFunc(w.Phases, func(p agent.WorkflowPhase) bool { return p.Index == ph.Index }); i >= 0 {
				w.Phases[i] = ph
			} else {
				w.Phases = append(w.Phases, ph)
			}
		case "workflow_agent":
			a := agent.WorkflowAgent{
				Index: *e.Index, Phase: e.PhaseIndex, Label: e.Label, State: e.State, AgentID: e.AgentID, Model: e.Model,
				Attempt: e.Attempt, Tokens: e.Tokens, ToolCalls: e.ToolCalls, Duration: time.Duration(e.DurationMS) * time.Millisecond,
				Prompt: e.PromptPreview, Result: e.ResultPreview,
			}
			i := slices.IndexFunc(w.Agents, func(p agent.WorkflowAgent) bool { return p.Index == a.Index })
			switch {
			case i < 0:
				w.Agents = append(w.Agents, a)
			case a.Attempt >= w.Agents[i].Attempt:
				w.Agents[i] = a
			}
		}
	}
	slices.SortFunc(w.Phases, func(a, b agent.WorkflowPhase) int { return a.Index - b.Index })
	slices.SortFunc(w.Agents, func(a, b agent.WorkflowAgent) int { return a.Index - b.Index })
	return w
}

// rateLimitWindows are the lengths of the known rate-limit windows.
var rateLimitWindows = map[string]time.Duration{"five_hour": 5 * time.Hour, "seven_day": 7 * 24 * time.Hour,
	"seven_day_opus": 7 * 24 * time.Hour, "seven_day_sonnet": 7 * 24 * time.Hour, "seven_day_oauth_apps": 7 * 24 * time.Hour,
	"seven_day_overage_included": 7 * 24 * time.Hour}

// rateLimit maps a rate_limit_event to the uniform windows. The headline
// window carries the event's status and overage state; the others are
// graded by use. A rejection is not an error while overage is allowed.
func (t *translator) rateLimit(line []byte, meta agent.EventMeta) {
	var f struct {
		Info struct {
			Status         string  `json:"status"`
			ResetsAt       int64   `json:"resetsAt"`
			RateLimitType  string  `json:"rateLimitType"`
			Utilization    float64 `json:"utilization"`
			OverageStatus  string  `json:"overageStatus"`
			IsUsingOverage bool    `json:"isUsingOverage"`
			OverageInUse   bool    `json:"overageInUse"`
			UnifiedWindows map[string]struct {
				Utilization float64 `json:"utilization"`
				ResetsAt    int64   `json:"resetsAt"`
			} `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal(line, &f) != nil {
		return
	}
	info := f.Info
	status := agent.RateLimitOK
	switch info.Status {
	case "allowed_warning":
		status = agent.RateLimitWarning
	case "rejected":
		status = agent.RateLimitExceeded
	}
	overageAllowed := info.OverageStatus == "allowed" || info.OverageStatus == "allowed_warning"
	t.mu.Lock()
	overageLimit := t.overageLimit
	t.mu.Unlock()
	t.limitResetsAt = time.Time{}
	var limits []agent.RateLimit
	add := func(name string, utilization float64, resets int64) {
		l := agent.RateLimit{Name: name, UsedPercent: utilization * 100, Window: rateLimitWindows[name], Status: agent.RateLimitOK}
		if resets > 0 {
			l.ResetsAt = time.Unix(resets, 0)
		}
		if name == "seven_day_overage_included" {
			l.LimitID = overageLimit
		}
		switch {
		case name == info.RateLimitType:
			l.Status = status
			l.Overage = info.IsUsingOverage || info.OverageInUse
			if status == agent.RateLimitExceeded {
				t.limitResetsAt = l.ResetsAt
			}
		case utilization >= 1:
			l.Status = agent.RateLimitExceeded
		}
		limits = append(limits, l)
	}
	for _, name := range slices.Sorted(maps.Keys(info.UnifiedWindows)) {
		w := info.UnifiedWindows[name]
		add(name, w.Utilization, w.ResetsAt)
	}
	if _, ok := info.UnifiedWindows[info.RateLimitType]; !ok && info.RateLimitType != "" {
		add(info.RateLimitType, info.Utilization, info.ResetsAt)
	}
	if status == agent.RateLimitExceeded && t.limitResetsAt.IsZero() && info.ResetsAt > 0 {
		t.limitResetsAt = time.Unix(info.ResetsAt, 0)
	}
	t.emit(&agent.RateLimitUpdated{EventMeta: meta, Limits: limits})
	switch {
	case status == agent.RateLimitExceeded && overageAllowed:
		t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeWarning, Code: "rate_limit_overage", Message: "rate limit reached (" + info.RateLimitType + "); using overage"})
	case status == agent.RateLimitExceeded:
		t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: "rate_limit", Message: "rate limit reached (" + info.RateLimitType + ")"})
	}
}
