package opencode

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// translator holds the state of mapping OpenCode events to agent events.
// It is guarded by Session.mu; events are handled on the stream goroutine.
type translator struct {
	// steers maps steers OpenCode has not delivered to their turn.
	steers map[string]string
	// delivered remembers delivered steers, for a steer whose answer
	// arrives after its delivery.
	delivered map[string]bool
	// held is a turn whose execution succeeded while steers sent into it
	// were undelivered; OpenCode runs another execution for them.
	held *heldEnd
	acc  *turnAcc
	// inbox holds enqueued items until OpenCode delivers them.
	inbox map[string]inboxItem
	// turnMsg maps a turn to the user message that started it, for turns
	// whose ID is not a message ID, such as slash commands.
	turnMsg map[string]string
	// tools holds running tool items by item ID.
	tools map[string]*agent.Item
	// texts holds the text of open message and reasoning items.
	texts map[string]*openText
	// children are subagent sessions by session ID.
	children map[string]*child
	// calls holds subagent tool calls by item ID until their session is
	// known.
	calls map[string]*subagentCall
	// inflight is the steer being sent, at most one under steerMu.
	// inflightCancelled marks that its turn was interrupted or failed
	// meanwhile, for the steer to take back once its request returns;
	// inflightAdopted that OpenCode ran it as a turn of its own.
	inflight                           string
	inflightCancelled, inflightAdopted bool
	// compactTurns marks the turns that run a compaction.
	compactTurns map[string]bool
	// reqSession maps pending requests to the session that asked.
	reqSession map[string]string
	// items lists the IDs of emitted items in order, for ItemsRemoved
	// when a revert drops them.
	items    []string
	itemSeen map[string]bool
	// execSeq counts every start and end of the session's executions, so
	// reconcile can tell that one happened while it read /active.
	execSeq int
	// awaiting is a steer waiting for the execution OpenCode starts for
	// it; pendingInterrupt is one Interrupt asked to stop once it starts.
	awaiting, pendingInterrupt string
	// skipExec marks the empty execution a revert clear runs, which is no
	// turn; skipping is set while it runs.
	skipExec, skipping bool
	// compaction is the open compaction item.
	compaction string
	// after holds work to run once Session.mu is released, such as
	// withdrawing requests: an answer in flight holds a request's lock
	// and may need Session.mu.
	after []func()
}

type heldEnd struct {
	turn string
	res  *agent.TurnResult
}

type turnAcc struct {
	turn       string
	compactErr string
	usage      agent.Usage
	steps      int
	finish     string
	lastMsg    string
	texts      []string
	origin     string
	sawInput   bool
}

type inboxItem struct {
	Type    string `json:"type"`
	Payload struct {
		Text     string          `json:"text"`
		Metadata json.RawMessage `json:"metadata"`
	} `json:"payload"`
}

func (i inboxItem) meta(key string) string {
	var m map[string]any
	_ = json.Unmarshal(i.Payload.Metadata, &m)
	v, _ := m[key].(string)
	return v
}

type openText struct {
	kind agent.ItemKind
	b    strings.Builder
	sc   scope
}

type child struct {
	id     string
	parent string
	depth  int
	// toolItem is the parent's subagent tool item.
	toolItem string
	task     *agent.Task
}

// newChild returns subagent session id of session parent with its running
// task.
func newChild(id, parent, title, agentName string, model *modelRef, depth int) *child {
	return &child{id: id, parent: parent, depth: depth, task: &agent.Task{ID: id, Kind: agent.TaskSubagent, NativeKind: "subagent", Status: agent.TaskRunning, Description: title,
		Subagent: &agent.Subagent{Type: agentName, Model: model.slug(), Effort: model.effort(), Depth: depth}}}
}

type subagentCall struct {
	prompt, description, agent, model string
	background                        bool
	child                             string
}

// scope says whose events are handled: the main session or a subagent.
type scope struct {
	prefix   string
	subagent string
	parent   string
}

func (t *translator) init() {
	t.steers = map[string]string{}
	t.delivered = map[string]bool{}
	t.inbox = map[string]inboxItem{}
	t.turnMsg = map[string]string{}
	t.tools = map[string]*agent.Item{}
	t.texts = map[string]*openText{}
	t.children = map[string]*child{}
	t.calls = map[string]*subagentCall{}
	t.compactTurns = map[string]bool{}
	t.reqSession = map[string]string{}
	t.itemSeen = map[string]bool{}
}

// maxItemLog bounds translator.items.
const maxItemLog = 4096

// emit queues ev and records the items it reports. s.mu must be held.
func (s *Session) emit(ev agent.Event) {
	var id string
	switch ev := ev.(type) {
	case *agent.ItemStarted:
		id = ev.Item.ID
	case *agent.ItemCompleted:
		id = ev.Item.ID
	}
	if t := &s.tr; id != "" && !t.itemSeen[id] {
		if len(t.items) >= maxItemLog {
			for _, old := range t.items[:maxItemLog/2] {
				delete(t.itemSeen, old)
			}
			t.items = slices.Delete(t.items, 0, maxItemLog/2)
		}
		t.items = append(t.items, id)
		t.itemSeen[id] = true
	}
	s.rt.Emit(ev)
}

// isBackgroundLocked reports whether session sid is a subagent running in
// the background, or nested in one.
func (s *Session) isBackgroundLocked(sid string) bool {
	for range 64 {
		ch := s.tr.children[sid]
		if ch == nil {
			return false
		}
		if ch.task != nil && ch.task.Subagent != nil && ch.task.Subagent.Background {
			return ch.task.Status == agent.TaskRunning
		}
		sid = ch.parent
	}
	return false
}

// inSubagentLocked reports whether session sid is subagent session root or
// nested in it.
func (s *Session) inSubagentLocked(sid, root string) bool {
	for range 64 {
		if sid == root {
			return true
		}
		ch := s.tr.children[sid]
		if ch == nil {
			return false
		}
		sid = ch.parent
	}
	return false
}

// closeSubagentItemsLocked completes the open items of subagent session
// root and of those nested in it once root stopped: it sends no more
// events for them. Tool calls that never ended are cancelled; text ends
// done when the subagent completed, else cancelled.
func (s *Session) closeSubagentItemsLocked(root string, meta agent.EventMeta) {
	t := &s.tr
	textStatus := agent.ItemCancelled
	if ch := t.children[root]; ch != nil && ch.task != nil && ch.task.Status == agent.TaskCompleted {
		textStatus = agent.ItemDone
	}
	for _, itemID := range slices.Sorted(maps.Keys(t.texts)) {
		ot := t.texts[itemID]
		if !s.inSubagentLocked(ot.sc.subagent, root) {
			continue
		}
		s.emit(&agent.ItemCompleted{EventMeta: meta, Item: agent.Item{ID: itemID, Kind: ot.kind, Status: textStatus, Text: ot.b.String(), ParentID: ot.sc.parent, SubagentID: ot.sc.subagent}})
		delete(t.texts, itemID)
	}
	for _, itemID := range slices.Sorted(maps.Keys(t.tools)) {
		item := t.tools[itemID]
		if !s.inSubagentLocked(item.SubagentID, root) {
			continue
		}
		item.Status = agent.ItemCancelled
		s.emit(&agent.ItemCompleted{EventMeta: meta, Item: item.Clone()})
		delete(t.tools, itemID)
		delete(t.calls, itemID)
	}
}

// releaseHeldLocked returns and clears the held end of turn, or nil.
func (t *translator) releaseHeldLocked(turn string) *agent.TurnResult {
	if t.held == nil || t.held.turn != turn {
		return nil
	}
	res := t.held.res
	t.held = nil
	return res
}

// steersOfLocked returns the undelivered steers of turn.
func (t *translator) steersOfLocked(turn string) []string {
	var ids []string
	for id, tid := range t.steers {
		if tid == turn {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// afterUnlock runs fn once s.mu is released by unlock. s.mu must be held.
func (s *Session) afterUnlock(fn func()) { s.tr.after = append(s.tr.after, fn) }

// unlock releases s.mu and runs the work queued with afterUnlock.
func (s *Session) unlock() {
	fns := s.tr.after
	s.tr.after = nil
	s.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func (s *Session) childIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.tr.children))
}

// remember bounds the bookkeeping maps that only ever grow.
func remember[V any](m map[string]V, key string, v V) {
	if len(m) >= 512 {
		clear(m)
	}
	m[key] = v
}

type envelope struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
	Location *struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

// handleRaw handles one event from the stream.
func (s *Session) handleRaw(raw []byte) {
	var ev envelope
	if err := json.Unmarshal(raw, &ev); err != nil {
		s.rt.Logger().Warn("opencode: ignoring malformed event", "error", err)
		return
	}
	if ev.Type == "server.connected" {
		s.connOnce.Do(func() { close(s.connected) })
		return
	}
	var head struct {
		SessionID string `json:"sessionID"`
		ParentID  string `json:"parentID"`
		Form      *struct {
			SessionID string `json:"sessionID"`
		} `json:"form"`
	}
	_ = json.Unmarshal(ev.Data, &head)
	sid := head.SessionID
	if head.Form != nil {
		sid = head.Form.SessionID
	}
	meta := agent.EventMeta{Raw: json.RawMessage(raw)}
	s.mu.Lock()
	defer s.unlock()
	own := s.rt.SessionID()
	if own == "" {
		return
	}
	switch {
	case sid == own:
		s.sessionEvent(scope{}, ev.Type, ev.Data, meta)
	case sid != "" && s.tr.children[sid] != nil:
		ch := s.tr.children[sid]
		s.childEvent(ch, ev.Type, ev.Data, meta)
	case ev.Type == "session.created" && head.ParentID != "" && (head.ParentID == own || s.tr.children[head.ParentID] != nil):
		s.childCreated(ev.Data, meta)
	case ev.Type == "session.agent.selected" && sid != "" && s.subagentRules[sid] != nil:
		// A finished subagent session resumed with another agent.
		s.subagentAgentSelectedLocked(sid, ev.Data)
		s.raw(ev.Type, meta)
	case sid == "":
		if ev.Location == nil || ev.Location.Directory == s.cwd {
			if ev.Type == "agent.updated" {
				// A config reload can change the agents' rules and the
				// default agent.
				s.afterUnlock(s.rewriteRules)
			}
			s.raw(ev.Type, meta)
		}
	}
}

func (s *Session) raw(typ string, meta agent.EventMeta) {
	s.rt.Emit(&agent.RawEvent{EventMeta: meta, Type: typ})
}

// sessionEvent handles an event of the main session.
func (s *Session) sessionEvent(sc scope, typ string, data json.RawMessage, meta agent.EventMeta) {
	t := &s.tr
	switch typ {
	case "session.execution.started":
		t.execSeq++
		if t.skipExec {
			if running, starting := s.rt.Running(); running == "" && !starting {
				// The empty execution a revert clear runs.
				t.skipExec, t.skipping = false, true
				s.raw(typ, meta)
				return
			}
		}
		running, _ := s.rt.RunStarted("")
		if running == "" {
			return
		}
		if t.held != nil && t.held.turn == running {
			// Another execution for the steers of a held turn.
			t.held = nil
		}
		if running == t.inflight {
			t.inflightAdopted = true
		}
		if running == t.awaiting {
			t.awaiting = ""
			if t.pendingInterrupt == running {
				t.pendingInterrupt = ""
				s.rt.Interrupt(running)
				// Off the event goroutine: the interrupt waits for OpenCode.
				go func() { _ = s.sendInterrupt(context.Background(), running) }()
			}
		}
		if t.acc == nil || t.acc.turn != running {
			t.acc = &turnAcc{turn: running}
			s.rt.Emit(&agent.TurnStarted{EventMeta: withTurn(meta, running)})
		}
		s.rt.Emit(&agent.StatusChanged{EventMeta: agent.EventMeta{TurnID: running}, Status: agent.StatusRunning})
	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted":
		t.execSeq++
		if t.skipping {
			t.skipping = false
			s.raw(typ, meta)
			return
		}
		running, _ := s.rt.Running()
		if running == "" {
			// An execution no turn waits for, such as the one clearing a
			// staged revert runs.
			s.raw(typ, meta)
			return
		}
		res := executionResult(typ, data, meta)
		if typ == "session.execution.succeeded" {
			if id, _ := s.rt.Interrupting(); id == running {
				res.Status, res.StopReason = agent.TurnInterrupted, "interrupted"
			} else if len(t.steersOfLocked(running)) > 0 {
				t.held = &heldEnd{turn: running, res: res}
				return
			}
		}
		s.finishTurnLocked(running, res)
	case "session.inbox.enqueued":
		var d struct {
			InboxID string    `json:"inboxID"`
			Item    inboxItem `json:"item"`
		}
		_ = json.Unmarshal(data, &d)
		t.inbox[d.InboxID] = d.Item
		if d.Item.Type == "synthetic" && d.Item.meta("source") == "subagent" {
			if s.taskReport(d.Item, meta) {
				return
			}
		}
		s.raw(typ, meta)
	case "session.inbox.delivered":
		var d struct {
			InboxID string `json:"inboxID"`
		}
		_ = json.Unmarshal(data, &d)
		item, ok := t.inbox[d.InboxID]
		delete(t.inbox, d.InboxID)
		if _, steer := t.steers[d.InboxID]; steer {
			delete(t.steers, d.InboxID)
			remember(t.delivered, d.InboxID, true)
		}
		if !ok || item.Type != "user" && item.Type != "synthetic" {
			s.raw(typ, meta)
			return
		}
		running, _ := s.rt.Running()
		if t.acc != nil && !t.acc.sawInput {
			t.acc.sawInput = true
			if item.Type == "synthetic" && item.meta("source") == "subagent" {
				t.acc.origin = "task-notification"
			}
		}
		if running != "" && item.Type == "user" && t.turnMsg[running] == "" {
			remember(t.turnMsg, running, d.InboxID)
		}
		s.emit(&agent.ItemCompleted{EventMeta: withTurn(meta, running), Item: agent.Item{
			ID: d.InboxID, Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: item.Payload.Text,
			TurnID: running, Time: time.Now(), ClientID: item.meta("clientID"), Synthetic: item.Type == "synthetic",
		}})
	case "session.inbox.cancelled":
		var d struct {
			InboxID string `json:"inboxID"`
		}
		_ = json.Unmarshal(data, &d)
		delete(t.inbox, d.InboxID)
		if d.InboxID == t.awaiting {
			// The steer's execution will not come.
			t.awaiting, t.pendingInterrupt = "", ""
			s.afterUnlock(func() {
				s.rt.EndSteer(d.InboxID)
				s.rt.AbandonTurn(d.InboxID, agent.ErrCancelled)
			})
		}
		if turn, ok := t.steers[d.InboxID]; ok {
			delete(t.steers, d.InboxID)
			if t.held != nil && t.held.turn == turn && len(t.steersOfLocked(turn)) == 0 {
				s.finishTurnLocked(turn, t.releaseHeldLocked(turn))
			}
		}
		s.raw(typ, meta)
	case "session.step.started", "session.model.selected":
		var d struct {
			Model *modelRef `json:"model"`
		}
		_ = json.Unmarshal(data, &d)
		if !s.modelChangedLocked(d.Model, meta) {
			s.raw(typ, meta)
		}
	case "session.step.ended", "session.step.failed":
		var d struct {
			Finish string   `json:"finish"`
			Cost   *float64 `json:"cost"`
			Tokens *tokens  `json:"tokens"`
			Error  *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &d)
		if acc := t.acc; acc != nil {
			acc.steps++
			if d.Finish != "" {
				acc.finish = d.Finish
			}
			if d.Cost != nil {
				acc.usage.CostUSD += *d.Cost
			}
			if d.Tokens != nil {
				d.Tokens.addTo(&acc.usage)
				acc.usage.ContextTokens = d.Tokens.Input + d.Tokens.Cache.Read + d.Tokens.Cache.Write + d.Tokens.Output + d.Tokens.Reasoning
				acc.usage.ContextWindow = s.windowLocked()
				s.rt.Emit(&agent.UsageUpdated{EventMeta: meta, Usage: acc.usage})
			}
		}
		if d.Error != nil && d.Error.Type != "aborted" {
			s.rt.Emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: d.Error.Type, Message: d.Error.Message})
		} else if d.Tokens == nil || t.acc == nil {
			s.raw(typ, meta)
		}
	case "session.compaction.started":
		var d struct {
			Reason  string `json:"reason"`
			InputID string `json:"inputID"`
		}
		_ = json.Unmarshal(data, &d)
		t.compaction = cmp.Or(d.InputID, messageIDOfEvent(eventID(meta)))
		s.rt.Emit(&agent.StatusChanged{Status: agent.StatusCompacting})
		s.rt.Emit(&agent.Compaction{EventMeta: meta, Trigger: d.Reason})
		s.emit(&agent.ItemStarted{EventMeta: meta, Item: agent.Item{ID: t.compaction, Kind: agent.ItemCompaction, Status: agent.ItemRunning}})
	case "session.compaction.ended", "session.compaction.failed":
		var d struct {
			Reason string  `json:"reason"`
			Text   string  `json:"text"`
			Tokens *tokens `json:"tokens"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &d)
		id := cmp.Or(t.compaction, messageIDOfEvent(eventID(meta)))
		t.compaction = ""
		ev := &agent.Compaction{EventMeta: meta, Done: true, Trigger: d.Reason, Summary: d.Text}
		item := agent.Item{ID: id, Kind: agent.ItemCompaction, Status: agent.ItemDone, Text: d.Text}
		if d.Error != nil {
			ev.Error = d.Error.Message
			item.Status, item.Text = agent.ItemFailed, d.Error.Message
			if t.acc != nil {
				t.acc.compactErr = cmp.Or(d.Error.Message, "compaction failed")
			}
		}
		if t.acc != nil && d.Tokens != nil {
			d.Tokens.addTo(&t.acc.usage)
		}
		s.rt.Emit(ev)
		s.emit(&agent.ItemCompleted{EventMeta: meta, Item: item})
		if t.acc != nil {
			s.rt.Emit(&agent.StatusChanged{Status: agent.StatusRunning})
		}
	case "session.retry.scheduled":
		var d struct {
			Attempt int   `json:"attempt"`
			At      int64 `json:"at"`
			Error   struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Status  int    `json:"status"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &d)
		delay := max(time.Until(time.UnixMilli(d.At)), 0)
		s.rt.Emit(&agent.Retry{EventMeta: meta, Attempt: d.Attempt, Delay: delay, Message: d.Error.Message, Code: d.Error.Type, HTTPStatus: d.Error.Status})
	case "session.renamed":
		var d struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(data, &d)
		s.info.Title = d.Title
		s.rt.Emit(&agent.SessionUpdated{EventMeta: meta, Info: s.infoLocked()})
	case "session.agent.selected":
		var d struct {
			Agent string `json:"agent"`
		}
		_ = json.Unmarshal(data, &d)
		// The mode stays: it follows the rules this library writes. An agent
		// switch by another client or a command gets the rules of the mode
		// for the new agent; SetAgent and SetPermissionMode write their own.
		if d.Agent != "" {
			// Compared with the agent the rules were built for, which a
			// following session's write in flight can have moved away from
			// this one; that write checks again when it ends.
			changed := d.Agent != s.agentName || d.Agent != s.rulesAgent
			s.agentName, s.agentFollowsDefault = d.Agent, false
			s.agentChanges++
			if changed {
				s.afterUnlock(s.rewriteRules)
			}
		}
		s.rt.Emit(&agent.SessionUpdated{EventMeta: meta, Info: s.infoLocked()})
	case "session.revert.committed":
		var d struct {
			To string `json:"to"`
		}
		_ = json.Unmarshal(data, &d)
		if i := slices.Index(t.items, d.To); i >= 0 {
			removed := slices.Clone(t.items[i:])
			for _, id := range removed {
				delete(t.itemSeen, id)
			}
			t.items = t.items[:i]
			s.rt.Emit(&agent.ItemsRemoved{EventMeta: meta, IDs: removed})
			return
		}
		s.raw(typ, meta)
	case "session.deleted":
		s.rt.Emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeWarning, Code: "session_deleted", Message: "the OpenCode session was deleted"})
	default:
		if !s.sharedEventLocked(sc, typ, data, meta) {
			s.raw(typ, meta)
		}
	}
}

// sharedEventLocked handles the events whose meaning is the same in the
// main session and in subagent sessions. It reports whether it handled typ.
func (s *Session) sharedEventLocked(sc scope, typ string, data json.RawMessage, meta agent.EventMeta) bool {
	t := &s.tr
	switch typ {
	case "session.text.started", "session.reasoning.started":
		var d textBlock
		_ = json.Unmarshal(data, &d)
		id, kind := blockID(typ, d)
		ot := &openText{kind: kind, sc: sc}
		t.texts[id] = ot
		s.emit(&agent.ItemStarted{EventMeta: meta, Item: agent.Item{ID: id, Kind: kind, Status: agent.ItemRunning, ParentID: sc.parent, SubagentID: sc.subagent}})
	case "session.text.delta", "session.reasoning.delta":
		var d textBlock
		_ = json.Unmarshal(data, &d)
		id, kind := blockID(typ, d)
		ot := t.texts[id]
		if ot == nil {
			ot = &openText{kind: kind, sc: sc}
			t.texts[id] = ot
		}
		ot.b.WriteString(d.Delta)
		if kind == agent.ItemReasoning {
			s.rt.Emit(&agent.ReasoningDelta{EventMeta: meta, ItemID: id, ParentID: sc.parent, SubagentID: sc.subagent, Delta: d.Delta})
		} else {
			s.rt.Emit(&agent.TextDelta{EventMeta: meta, ItemID: id, ParentID: sc.parent, SubagentID: sc.subagent, Delta: d.Delta})
		}
	case "session.text.ended", "session.reasoning.ended":
		var d textBlock
		_ = json.Unmarshal(data, &d)
		id, kind := blockID(typ, d)
		delete(t.texts, id)
		if kind == agent.ItemAssistantMessage && sc.subagent == "" && t.acc != nil {
			if d.AssistantMessageID != t.acc.lastMsg {
				t.acc.lastMsg, t.acc.texts = d.AssistantMessageID, nil
			}
			t.acc.texts = append(t.acc.texts, d.Text)
		}
		s.emit(&agent.ItemCompleted{EventMeta: meta, Item: agent.Item{ID: id, Kind: kind, Status: agent.ItemDone, Text: d.Text, ParentID: sc.parent, SubagentID: sc.subagent}})
	case "session.tool.input.started":
		var d struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(data, &d)
		id := sc.prefix + d.ID
		item := &agent.Item{ID: id, Kind: agent.ItemToolCall, Status: agent.ItemRunning, ParentID: sc.parent, SubagentID: sc.subagent,
			Tool: &agent.ToolCall{Name: d.Name, Kind: s.toolKindLocked(d.Name)}}
		s.fillServerLocked(item.Tool)
		t.tools[id] = item
		if item.Tool.Kind == agent.ToolSubagent {
			t.calls[id] = &subagentCall{}
		}
		if ch := t.children[sc.subagent]; ch != nil && ch.task != nil {
			ch.task.ToolUses++
			ch.task.LastTool = d.Name
			s.rt.Emit(&agent.TaskUpdated{Task: ch.task.Clone()})
		}
		s.emit(&agent.ItemStarted{EventMeta: meta, Item: item.Clone()})
	case "session.tool.input.delta":
		var d struct {
			ID    string `json:"id"`
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(data, &d)
		s.rt.Emit(&agent.ToolInputDelta{EventMeta: meta, ItemID: sc.prefix + d.ID, ParentID: sc.parent, SubagentID: sc.subagent, Delta: d.Delta})
	case "session.tool.input.ended", "session.tool.called":
		var d struct {
			ID    string          `json:"id"`
			Text  string          `json:"text"`
			Input json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(data, &d)
		input := d.Input
		if typ == "session.tool.input.ended" {
			input = json.RawMessage(d.Text)
		}
		if !json.Valid(input) {
			s.raw(typ, meta)
			return true
		}
		item := t.tools[sc.prefix+d.ID]
		if item == nil {
			s.raw(typ, meta)
			return true
		}
		item.Tool.Input = bytes.Clone(input)
		fillToolDetails(item.Tool, input)
		if call := t.calls[item.ID]; call != nil {
			call.read(input)
			if ch := t.children[call.child]; ch != nil {
				s.updateTaskLocked(ch, call)
			}
		}
		s.rt.Emit(&agent.ItemUpdated{EventMeta: meta, Item: item.Clone()})
	case "session.tool.progress":
		var d struct {
			ID       string `json:"id"`
			Metadata struct {
				SessionID string `json:"sessionID"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(data, &d)
		id := sc.prefix + d.ID
		if call := t.calls[id]; call != nil && d.Metadata.SessionID != "" {
			s.linkChildLocked(cmp.Or(sc.subagent, s.rt.SessionID()), id, call, d.Metadata.SessionID)
			return true
		}
		s.raw(typ, meta)
	case "session.tool.success", "session.tool.failed":
		s.toolEndLocked(sc, typ, data, meta)
	case "permission.asked":
		s.permissionAskedLocked(data, meta.Raw)
	case "form.created":
		var d struct {
			Form json.RawMessage `json:"form"`
		}
		_ = json.Unmarshal(data, &d)
		s.formCreatedLocked(d.Form, meta.Raw)
	case "permission.replied":
		var d struct {
			RequestID string `json:"requestID"`
		}
		_ = json.Unmarshal(data, &d)
		s.requestSettledLocked(d.RequestID)
		s.raw(typ, meta)
	case "form.replied", "form.cancelled":
		var d struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(data, &d)
		s.requestSettledLocked(d.ID)
		s.raw(typ, meta)
	default:
		return false
	}
	return true
}

// childEvent handles an event of a subagent session.
func (s *Session) childEvent(ch *child, typ string, data json.RawMessage, meta agent.EventMeta) {
	sc := scope{prefix: ch.id + "/", subagent: ch.id, parent: ch.toolItem}
	switch typ {
	case "session.execution.started":
		if ch.task != nil && ch.task.Status != agent.TaskRunning {
			ch.task.Status = agent.TaskRunning
			s.rt.Emit(&agent.TaskUpdated{EventMeta: meta, Task: ch.task.Clone()})
			return
		}
	case "session.agent.selected":
		s.subagentAgentSelectedLocked(ch.id, data)
	case "session.step.ended", "session.step.failed":
		var d struct {
			Cost   *float64 `json:"cost"`
			Tokens *tokens  `json:"tokens"`
		}
		_ = json.Unmarshal(data, &d)
		if ch.task != nil && d.Tokens != nil {
			if ch.task.Usage == nil {
				ch.task.Usage = &agent.Usage{}
			}
			d.Tokens.addTo(ch.task.Usage)
			if d.Cost != nil {
				ch.task.Usage.CostUSD += *d.Cost
			}
		}
	default:
		if s.sharedEventLocked(sc, typ, data, meta) {
			return
		}
	}
	s.raw(typ, meta)
}

// subagentAgentSelectedLocked records the agent a subagent session
// switched to and writes its rules for it. The caller holds s.mu.
func (s *Session) subagentAgentSelectedLocked(id string, data json.RawMessage) {
	var d struct {
		Agent string `json:"agent"`
	}
	_ = json.Unmarshal(data, &d)
	if s.setSubagentAgentLocked(id, d.Agent) {
		s.afterUnlock(func() { s.writeSubagent(id) })
	}
}

// childCreated registers a subagent session and reports its task.
func (s *Session) childCreated(data json.RawMessage, meta agent.EventMeta) {
	var d struct {
		SessionID string    `json:"sessionID"`
		ParentID  string    `json:"parentID"`
		Title     string    `json:"title"`
		Agent     string    `json:"agent"`
		Model     *modelRef `json:"model"`
		// Permissions are the rules OpenCode copied from the parent.
		Permissions *[]PermissionRule `json:"permissions"`
	}
	_ = json.Unmarshal(data, &d)
	depth := 1
	parent := s.tr.children[d.ParentID]
	if parent != nil {
		depth = parent.depth + 1
	}
	ch := newChild(d.SessionID, d.ParentID, d.Title, d.Agent, d.Model, depth)
	task := ch.task
	if parent != nil {
		task.ParentID = parent.id
	}
	s.tr.children[d.SessionID] = ch
	if s.addSubagentLocked(d.SessionID, d.Agent, d.Permissions) {
		s.afterUnlock(func() { s.writeSubagent(d.SessionID) })
	}
	s.rt.Emit(&agent.TaskUpdated{EventMeta: meta, Task: task.Clone()})
}

func (c *subagentCall) read(input json.RawMessage) {
	var in struct {
		Prompt      string `json:"prompt"`
		Description string `json:"description"`
		Agent       string `json:"agent"`
		Model       string `json:"model"`
		Background  bool   `json:"background"`
	}
	if json.Unmarshal(input, &in) != nil {
		return
	}
	c.prompt = cmp.Or(in.Prompt, c.prompt)
	c.description = cmp.Or(in.Description, c.description)
	c.agent = cmp.Or(in.Agent, c.agent)
	c.model = cmp.Or(in.Model, c.model)
	c.background = c.background || in.Background
}

// linkChildLocked connects a subagent tool call to the session it runs in.
func (s *Session) linkChildLocked(caller, toolItem string, call *subagentCall, childID string) {
	call.child = childID
	ch := s.tr.children[childID]
	if ch == nil {
		// The session was created before this session saw it.
		ch = newChild(childID, caller, "", "", nil, 1)
		s.tr.children[childID] = ch
	}
	switch {
	case s.addSubagentLocked(childID, call.agent, nil), s.setSubagentAgentLocked(childID, call.agent):
		s.afterUnlock(func() { s.writeSubagent(childID) })
	}
	ch.toolItem = toolItem
	s.updateTaskLocked(ch, call)
}

func (s *Session) updateTaskLocked(ch *child, call *subagentCall) {
	task := ch.task
	task.ToolCallID = ch.toolItem
	task.Description = cmp.Or(call.description, task.Description)
	task.Subagent.Prompt = cmp.Or(call.prompt, task.Subagent.Prompt)
	task.Subagent.Type = cmp.Or(call.agent, task.Subagent.Type)
	task.Subagent.Model = cmp.Or(call.model, task.Subagent.Model)
	task.Subagent.Background = call.background
	s.rt.Emit(&agent.TaskUpdated{Task: task.Clone()})
}

var subagentResult = regexp.MustCompile(`(?s)^<subagent\b[^>]*>\n?(.*?)\n?</subagent>$`)

// subagentOutput unwraps the <subagent> envelope of a subagent result.
func subagentOutput(text string) string {
	if m := subagentResult.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
		return m[1]
	}
	return text
}

// taskReport settles a background subagent from the report OpenCode
// queues for its parent. It reports whether the report named a known task.
func (s *Session) taskReport(item inboxItem, meta agent.EventMeta) bool {
	ch := s.tr.children[item.meta("childID")]
	if ch == nil || ch.task == nil {
		return false
	}
	switch item.meta("state") {
	case "error":
		ch.task.Status = agent.TaskFailed
		ch.task.Error = subagentOutput(item.Payload.Text)
	case "cancelled":
		ch.task.Status = agent.TaskStopped
	default:
		ch.task.Status = agent.TaskCompleted
		ch.task.Output = subagentOutput(item.Payload.Text)
	}
	s.closeSubagentItemsLocked(ch.id, meta)
	s.rt.Emit(&agent.TaskUpdated{EventMeta: meta, Task: ch.task.Clone()})
	return true
}

func (s *Session) toolEndLocked(sc scope, typ string, data json.RawMessage, meta agent.EventMeta) {
	t := &s.tr
	var d struct {
		ID      string           `json:"id"`
		Content []toolContent    `json:"content"`
		Meta    json.RawMessage  `json:"metadata"`
		Error   *structuredError `json:"error"`
	}
	_ = json.Unmarshal(data, &d)
	id := sc.prefix + d.ID
	item := t.tools[id]
	if item == nil && t.itemSeen[id] {
		// The item already completed, such as one a turn's end or its
		// subagent's stop cancelled.
		s.raw(typ, meta)
		return
	}
	delete(t.tools, id)
	if item == nil {
		item = &agent.Item{ID: id, Kind: agent.ItemToolCall, ParentID: sc.parent, SubagentID: sc.subagent, Tool: &agent.ToolCall{Kind: agent.ToolOther}}
	}
	call := item.Tool
	call.Output, call.Images = contentText(d.Content)
	if len(d.Meta) > 0 && string(d.Meta) != "null" {
		call.OutputData = bytes.Clone(d.Meta)
		var m struct {
			Exit *int `json:"exit"`
		}
		if json.Unmarshal(d.Meta, &m) == nil && m.Exit != nil {
			call.ExitCode = m.Exit
		}
	}
	item.Status = agent.ItemDone
	if typ == "session.tool.failed" {
		call.IsError = true
		item.Status = agent.ItemFailed
		if d.Error != nil {
			call.Output = cmp.Or(call.Output, d.Error.Message)
			switch d.Error.Type {
			case "aborted":
				item.Status = agent.ItemCancelled
			case "permission.rejected":
				item.Status = agent.ItemDeclined
			}
		}
	}
	if sub := t.calls[id]; sub != nil {
		delete(t.calls, id)
		var m struct {
			SessionID string `json:"sessionID"`
			Status    string `json:"status"`
		}
		_ = json.Unmarshal(d.Meta, &m)
		if m.SessionID != "" && sub.child == "" {
			s.linkChildLocked(cmp.Or(sc.subagent, s.rt.SessionID()), id, sub, m.SessionID)
		}
		if ch := t.children[sub.child]; ch != nil && ch.task != nil {
			switch {
			case m.Status == "running":
				// A background subagent returns at launch; its report
				// settles it.
				ch.task.Subagent.Background = true
			case item.Status == agent.ItemDone:
				ch.task.Status = agent.TaskCompleted
				ch.task.Output = subagentOutput(call.Output)
			case item.Status == agent.ItemCancelled:
				ch.task.Status = agent.TaskStopped
			default:
				ch.task.Status = agent.TaskFailed
				ch.task.Error = call.Output
			}
			if ch.task.Status != agent.TaskRunning {
				s.closeSubagentItemsLocked(ch.id, meta)
			}
			s.rt.Emit(&agent.TaskUpdated{EventMeta: meta, Task: ch.task.Clone()})
		}
	}
	s.emit(&agent.ItemCompleted{EventMeta: meta, Item: item.Clone()})
}

type structuredError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
	URI  string `json:"uri"`
	Mime string `json:"mime"`
}

// contentText joins the text of tool content and returns its images.
func contentText(content []toolContent) (string, []agent.Image) {
	var texts []string
	var images []agent.Image
	for _, c := range content {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
		case "file":
			if !strings.HasPrefix(c.Mime, "image/") {
				continue
			}
			img := agent.Image{MediaType: c.Mime}
			if rest, ok := strings.CutPrefix(c.URI, "data:"); ok {
				if _, b64, ok := strings.Cut(rest, ";base64,"); ok {
					img.Data, _ = base64.StdEncoding.DecodeString(b64)
				}
			} else if path, ok := fileURIPath(c.URI, runtime.GOOS == "windows"); ok {
				img.Path = filepath.FromSlash(path)
			} else {
				img.URL = c.URI
			}
			images = append(images, img)
		}
	}
	return strings.Join(texts, "\n"), images
}

// textBlock is the data of a text or reasoning event.
type textBlock struct {
	AssistantMessageID string `json:"assistantMessageID"`
	Ordinal            int    `json:"ordinal"`
	Delta              string `json:"delta"`
	Text               string `json:"text"`
}

func blockID(typ string, d textBlock) (string, agent.ItemKind) {
	if strings.HasPrefix(typ, "session.reasoning.") {
		return d.AssistantMessageID + ":reasoning:" + strconv.Itoa(d.Ordinal), agent.ItemReasoning
	}
	return d.AssistantMessageID + ":text:" + strconv.Itoa(d.Ordinal), agent.ItemAssistantMessage
}

func eventID(meta agent.EventMeta) string {
	var e struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meta.Raw, &e)
	return e.ID
}

// messageIDOfEvent is the ID of the message OpenCode records for event id,
// such as a compaction started without an input.
func messageIDOfEvent(id string) string {
	if rest, ok := strings.CutPrefix(id, "evt_"); ok {
		return "msg_" + rest
	}
	return id
}

func withTurn(meta agent.EventMeta, turn string) agent.EventMeta {
	meta.TurnID = turn
	return meta
}

// executionResult builds the outcome of an execution end.
func executionResult(typ string, data json.RawMessage, meta agent.EventMeta) *agent.TurnResult {
	res := &agent.TurnResult{Status: agent.TurnSucceeded, Raw: meta.Raw}
	switch typ {
	case "session.execution.interrupted":
		var d struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(data, &d)
		res.Status = agent.TurnInterrupted
		res.StopReason = cmp.Or(d.Reason, "interrupted")
		res.Err = &agent.TurnError{Code: "interrupted", Message: "the execution was interrupted (" + res.StopReason + ")"}
	case "session.execution.failed":
		var d struct {
			Error structuredError `json:"error"`
		}
		_ = json.Unmarshal(data, &d)
		res.Status = agent.TurnFailed
		res.StopReason = "error"
		res.Err = turnError(d.Error)
	}
	return res
}

func turnError(e structuredError) *agent.TurnError {
	te := &agent.TurnError{Code: e.Type, Message: e.Message, HTTPStatus: e.Status}
	te.Retryable = e.Status == 429 || e.Status >= 500 ||
		strings.Contains(e.Type, "rate") || strings.Contains(e.Type, "overload")
	return te
}

// finishTurnLocked ends turn id: open items complete, pending requests are
// withdrawn, undelivered steers of a turn that did not succeed are taken
// back, and the runtime reports the result.
func (s *Session) finishTurnLocked(id string, res *agent.TurnResult) {
	t := &s.tr
	compactErr := ""
	if acc := t.acc; acc != nil && acc.turn == id {
		compactErr = acc.compactErr
		res.Usage = acc.usage
		res.Steps = acc.steps
		res.Text = strings.Join(acc.texts, "\n\n")
		res.Origin = acc.origin
		if res.StopReason == "" {
			res.StopReason = acc.finish
		}
		t.acc = nil
	}
	if t.compactTurns[id] {
		delete(t.compactTurns, id)
		if compactErr != "" && res.Status == agent.TurnSucceeded {
			res.Status, res.StopReason = agent.TurnFailed, "compaction_failed"
			res.Err = &agent.TurnError{Code: "compaction_failed", Message: compactErr}
		}
	}
	t.releaseHeldLocked(id)
	// Background subagents outlive the turn: their items stay open and
	// their requests pending. Text an interrupt cut off is cancelled, and
	// tool calls that never ended are, as with Pi.
	textStatus := agent.ItemDone
	if res.Status == agent.TurnInterrupted {
		textStatus = agent.ItemCancelled
	}
	for _, itemID := range slices.Sorted(maps.Keys(t.texts)) {
		ot := t.texts[itemID]
		if s.isBackgroundLocked(ot.sc.subagent) {
			continue
		}
		s.emit(&agent.ItemCompleted{EventMeta: agent.EventMeta{TurnID: id}, Item: agent.Item{ID: itemID, Kind: ot.kind, Status: textStatus, Text: ot.b.String(), ParentID: ot.sc.parent, SubagentID: ot.sc.subagent}})
		delete(t.texts, itemID)
	}
	for _, itemID := range slices.Sorted(maps.Keys(t.tools)) {
		item := t.tools[itemID]
		if s.isBackgroundLocked(item.SubagentID) {
			continue
		}
		item.Status = agent.ItemCancelled
		s.emit(&agent.ItemCompleted{EventMeta: agent.EventMeta{TurnID: id}, Item: item.Clone()})
		delete(t.tools, itemID)
		delete(t.calls, itemID)
	}
	for _, req := range s.rt.PendingRequests() {
		if s.isBackgroundLocked(t.reqSession[req.RequestID()]) {
			continue
		}
		s.afterUnlock(func() { s.rt.WithdrawRequest(req.RequestID()) })
	}
	var takeBack []string
	for _, sid := range t.steersOfLocked(id) {
		delete(t.steers, sid)
		switch {
		case res.Status == agent.TurnSucceeded:
			// OpenCode delivers it in an execution of its own.
		case t.inflight == sid:
			// The steer takes itself back once its request returns.
			t.inflightCancelled = true
		default:
			takeBack = append(takeBack, sid)
		}
	}
	if len(takeBack) > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s.cancelSteers(ctx, takeBack)
		}()
	}
	// Requests settle by answer, withdrawal, or a turn's end; the owners of
	// those no longer pending are forgotten.
	pending := map[string]bool{}
	for _, req := range s.rt.PendingRequests() {
		pending[req.RequestID()] = true
	}
	for rid := range t.reqSession {
		if !pending[rid] {
			delete(t.reqSession, rid)
		}
	}
	// Finished subagents send no more events.
	for cid, ch := range t.children {
		if ch.task != nil && ch.task.Status != agent.TaskRunning {
			delete(t.children, cid)
		}
	}
	s.rt.FinishTurn(id, res)
	// Ending the turn may start queued input, which is not idle.
	if s.rt.Idle() {
		s.rt.Emit(&agent.StatusChanged{EventMeta: agent.EventMeta{TurnID: id}, Status: agent.StatusIdle})
	}
}

// finishTurn is finishTurnLocked for callers that do not hold s.mu.
func (s *Session) finishTurn(id string, res *agent.TurnResult) {
	s.mu.Lock()
	defer s.unlock()
	s.finishTurnLocked(id, res)
}

// endTurn ends turn id unless it already ended.
func (s *Session) endTurn(id string, res *agent.TurnResult) {
	s.mu.Lock()
	defer s.unlock()
	if running, _ := s.rt.Running(); running == id {
		s.finishTurnLocked(id, res)
	}
}

func (s *Session) infoLocked() agent.SessionInfo {
	info := s.info
	info.ID = s.rt.SessionID()
	info.Commands = slices.Clone(info.Commands)
	info.MCPServers = slices.Clone(info.MCPServers)
	return info
}

// modelChangedLocked records a model the server reports and emits
// SessionUpdated when it differs. It reports whether it emitted.
func (s *Session) modelChangedLocked(m *modelRef, meta agent.EventMeta) bool {
	if m == nil || m.slug() == "" {
		return false
	}
	if s.model != nil && *s.model == *m {
		return false
	}
	if s.model != nil && s.model.slug() == m.slug() && s.model.effort() == m.effort() {
		s.model = m
		return false
	}
	s.model = m
	s.info.Model, s.info.Effort = m.slug(), m.effort()
	s.rt.Emit(&agent.SessionUpdated{EventMeta: meta, Info: s.infoLocked()})
	return true
}

func (s *Session) windowLocked() int64 {
	if m, ok := s.models[s.model.slug()]; ok {
		return m.window()
	}
	return 0
}

// toolKindLocked classifies an OpenCode tool name.
func (s *Session) toolKindLocked(name string) agent.ToolKind {
	if s.host != nil && strings.HasPrefix(name, s.host.name+"_") {
		return agent.ToolHost
	}
	if kind := builtinToolKind(name); kind != agent.ToolOther {
		return kind
	}
	if s.mcpServerOfLocked(name) != "" {
		return agent.ToolMCP
	}
	return agent.ToolOther
}

// builtinToolKind classifies the names of OpenCode's built-in tools.
func builtinToolKind(name string) agent.ToolKind {
	switch strings.ToLower(name) {
	case "shell", "bash":
		return agent.ToolShell
	case "read":
		return agent.ToolRead
	case "edit", "write", "patch", "apply_patch", "multiedit":
		return agent.ToolEdit
	case "grep", "glob", "list", "ls", "lsp", "codesearch", "code_search":
		return agent.ToolSearch
	case "webfetch", "websearch", "browser":
		return agent.ToolWeb
	case "subagent", "task":
		return agent.ToolSubagent
	case "question":
		return agent.ToolQuestion
	case "todowrite", "todoread", "todo":
		return agent.ToolTodo
	}
	return agent.ToolOther
}

// mcpServerOfLocked returns the MCP server whose tool name is, or "".
func (s *Session) mcpServerOfLocked(name string) string {
	best := ""
	consider := func(server string) {
		if strings.HasPrefix(name, server+"_") && len(server) > len(best) {
			best = server
		}
	}
	for server := range s.cfg.MCPServers {
		consider(server)
	}
	for _, st := range s.info.MCPServers {
		consider(st.Name)
	}
	return best
}

func (s *Session) fillServerLocked(call *agent.ToolCall) {
	if call.Kind == agent.ToolMCP {
		call.Server = s.mcpServerOfLocked(call.Name)
	}
	if call.Kind == agent.ToolHost && s.host != nil {
		call.Server = s.host.name
		call.DisplayName = strings.TrimPrefix(call.Name, s.host.name+"_")
	}
}

// fillToolDetails copies well-known arguments into the uniform fields.
func fillToolDetails(call *agent.ToolCall, input json.RawMessage) {
	var in struct {
		Command   string `json:"command"`
		Workdir   string `json:"workdir"`
		Cwd       string `json:"cwd"`
		FilePath  string `json:"filePath"`
		Path      string `json:"path"`
		File      string `json:"file"`
		Pattern   string `json:"pattern"`
		Query     string `json:"query"`
		URL       string `json:"url"`
		OldString string `json:"oldString"`
		Content   string `json:"content"`
	}
	if json.Unmarshal(input, &in) != nil {
		return
	}
	switch call.Kind {
	case agent.ToolShell:
		call.Command = in.Command
		call.Cwd = cmp.Or(in.Workdir, in.Cwd)
	case agent.ToolRead:
		call.Path = cmp.Or(in.FilePath, in.Path, in.File)
	case agent.ToolEdit:
		call.Path = cmp.Or(in.FilePath, in.Path, in.File)
		if call.Path != "" {
			kind := agent.FileModified
			if call.Name == "write" && in.OldString == "" {
				kind = agent.FileAdded
			}
			call.Changes = []agent.FileChange{{Path: call.Path, Kind: kind}}
		}
	case agent.ToolSearch:
		call.Query = cmp.Or(in.Pattern, in.Query)
		call.Path = cmp.Or(in.Path, in.FilePath)
	case agent.ToolWeb:
		call.URL = in.URL
		call.Query = in.Query
	}
}

// permissionKind classifies a permission action.
func permissionKind(action string) agent.PermissionKind {
	switch action {
	case "shell", "bash":
		return agent.PermissionCommand
	case "edit", "write", "patch", "apply_patch":
		return agent.PermissionFileChange
	case "external_directory":
		return agent.PermissionGrant
	}
	return agent.PermissionTool
}

// scopeOfLocked returns the scope of a session this one tracks.
func (s *Session) scopeOfLocked(sid string) (scope, bool) {
	if sid == s.rt.SessionID() {
		return scope{}, true
	}
	if ch := s.tr.children[sid]; ch != nil {
		return scope{prefix: sid + "/", subagent: sid, parent: ch.toolItem}, true
	}
	return scope{}, false
}

func (s *Session) pendingLocked(id string) bool {
	return slices.ContainsFunc(s.rt.PendingRequests(), func(r agent.Request) bool { return r.RequestID() == id })
}

func (s *Session) requestSettledLocked(id string) {
	s.afterUnlock(func() { s.rt.WithdrawRequest(id) })
	if s.tr.acc != nil && len(s.rt.PendingRequests()) <= 1 {
		s.rt.Emit(&agent.StatusChanged{Status: agent.StatusRunning})
	}
}

// permissionAsked registers a permission request; see
// permissionAskedLocked.
func (s *Session) permissionAsked(data, raw json.RawMessage) {
	s.mu.Lock()
	defer s.unlock()
	s.permissionAskedLocked(data, raw)
}

func (s *Session) permissionAskedLocked(data, raw json.RawMessage) {
	var d struct {
		ID        string   `json:"id"`
		SessionID string   `json:"sessionID"`
		Action    string   `json:"action"`
		Resources []string `json:"resources"`
		Save      []string `json:"save"`
		Source    *struct {
			ID string `json:"id"`
		} `json:"source"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &d) != nil || d.ID == "" || s.pendingLocked(d.ID) {
		return
	}
	sc, ok := s.scopeOfLocked(d.SessionID)
	if !ok {
		return
	}
	req := agent.PermissionRequest{
		EventMeta:  agent.EventMeta{Raw: raw},
		ID:         d.ID,
		Kind:       permissionKind(d.Action),
		ToolName:   d.Action,
		Reason:     d.Message,
		Paths:      d.Resources,
		SubagentID: sc.subagent,
		Decisions:  []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionAllowSession, agent.DecisionAllowAlways, agent.DecisionDeny, agent.DecisionCancel},
	}
	if d.Source != nil {
		req.ToolCallID = sc.prefix + d.Source.ID
		if item := s.tr.tools[req.ToolCallID]; item != nil {
			req.ToolName = item.Tool.Name
			req.Input = item.Tool.Input
		}
	}
	target := strings.Join(d.Resources, ", ")
	switch req.Kind {
	case agent.PermissionCommand:
		req.Command = strings.Join(d.Resources, "\n")
		req.Cwd = s.cwd
		req.Title = "OpenCode wants to run " + target
	case agent.PermissionFileChange:
		req.Title = "OpenCode wants to edit " + target
		for _, p := range d.Resources {
			req.Changes = append(req.Changes, agent.FileChange{Path: p, Kind: agent.FileModified})
		}
	case agent.PermissionGrant:
		req.Title = "OpenCode wants to access " + target
	default:
		req.Title = fmt.Sprintf("OpenCode wants to use %s", req.ToolName)
		if target != "" && target != "*" {
			req.Title += " on " + target
		}
	}
	if len(d.Save) > 0 {
		req.Description = "Allowing for the session or always covers " + strings.Join(d.Save, ", ")
	}
	sid, action, save, asked := d.SessionID, d.Action, d.Save, d.Resources
	s.tr.reqSession[d.ID] = sid
	s.rt.Request(providerkit.NewPermissionRequest(req, func(ctx context.Context, resp agent.PermissionResponse) error {
		if err := providerkit.CheckUpdatedInput(Name, resp); err != nil {
			return err
		}
		return s.replyPermission(ctx, sid, req.ID, action, save, asked, resp)
	}))
	s.rt.Emit(&agent.StatusChanged{Status: agent.StatusWaiting})
}

// declined is the message of a Deny without one: OpenCode treats a reject
// without a message as a stop of the whole execution.
const declined = "The user declined this request."

func (s *Session) replyPermission(ctx context.Context, sid, id, action string, save, asked []string, resp agent.PermissionResponse) error {
	var reply PermissionReply
	switch v := resp.ProviderResponse.(type) {
	case PermissionReply:
		reply = v
	case *PermissionReply:
		reply = *v
	default:
		//exhaustive:enforce
		switch resp.Decision {
		case agent.DecisionAllow:
			reply.Decision = "once"
		case agent.DecisionAllowSession:
			if err := s.grant(ctx, sid, action, save, asked); err != nil {
				return err
			}
			reply.Decision = "once"
		case agent.DecisionAllowAlways:
			reply.Decision = "always"
		case agent.DecisionDeny:
			reply = PermissionReply{Decision: "reject", Message: cmp.Or(resp.Message, declined)}
		case agent.DecisionCancel:
			reply.Decision = "reject"
		default:
			return fmt.Errorf("opencode: unknown permission decision %q", resp.Decision)
		}
	}
	path := sessionPath(sid, "/permission/") + url.PathEscape(id) + "/reply"
	err := s.c.Call(ctx, "POST", path, reply, nil)
	if httpStatus(err) == 404 {
		// The request is gone, such as after an interrupt.
		return nil
	}
	return err
}

type formField struct {
	Key         string          `json:"key"`
	Type        string          `json:"type"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Placeholder string          `json:"placeholder"`
	Default     json.RawMessage `json:"default"`
	Custom      bool            `json:"custom"`
	Hidden      bool            `json:"hidden"`
	Required    bool            `json:"required"`
	URL         string          `json:"url"`
	When        []formWhen      `json:"when"`
	Options     []struct {
		Value       string `json:"value"`
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

// formWhen makes a field apply only when another field's value compares
// equal (op "eq") or unequal ("neq") to Value.
type formWhen struct {
	Key   string          `json:"key"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`
}

// defaultText renders a field default of any JSON type as an answer.
func defaultText(raw json.RawMessage, boolean bool) string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return ""
	}
	switch v := v.(type) {
	case string:
		return v
	case bool:
		if !boolean {
			return strconv.FormatBool(v)
		}
		if v {
			return "Yes"
		}
		return "No"
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case []any:
		var parts []string
		for _, p := range v {
			parts = append(parts, fmt.Sprint(p))
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

// formCreated registers a form; see formCreatedLocked.
func (s *Session) formCreated(form, raw json.RawMessage) {
	s.mu.Lock()
	defer s.unlock()
	s.formCreatedLocked(form, raw)
}

// formCreatedLocked turns an OpenCode form, such as the question tool's,
// into a UserInputRequest.
func (s *Session) formCreatedLocked(form, raw json.RawMessage) {
	var f struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		Title     string `json:"title"`
		Metadata  struct {
			Tool *struct {
				ID string `json:"id"`
			} `json:"tool"`
		} `json:"metadata"`
		Fields []formField `json:"fields"`
	}
	if json.Unmarshal(form, &f) != nil || f.ID == "" || s.pendingLocked(f.ID) {
		return
	}
	sc, ok := s.scopeOfLocked(f.SessionID)
	if !ok {
		return
	}
	req := agent.UserInputRequest{EventMeta: agent.EventMeta{Raw: raw}, ID: f.ID, Title: f.Title, SubagentID: sc.subagent}
	if f.Metadata.Tool != nil {
		req.ToolCallID = sc.prefix + f.Metadata.Tool.ID
	}
	for i, field := range f.Fields {
		if field.Hidden {
			continue
		}
		q := agent.Question{ID: field.Key, Header: cmp.Or(field.Title, fmt.Sprintf("Question %d", i+1)), Text: cmp.Or(field.Description, field.Title), Placeholder: field.Placeholder}
		q.Default = defaultText(field.Default, field.Type == "boolean")
		for _, w := range field.When {
			// The UI shows every question; the answer drops this one when
			// the condition does not hold.
			op := "is"
			if w.Op == "neq" {
				op = "is not"
			}
			q.Text = strings.TrimSpace(fmt.Sprintf("%s (only when %s %s %s)", q.Text, w.Key, op, strings.Trim(string(w.Value), `"`)))
		}
		switch field.Type {
		case "boolean":
			q.Options = []agent.QuestionOption{{Label: "Yes"}, {Label: "No"}}
		case "external":
			q.Text = strings.TrimSpace(q.Text + "\n" + field.URL)
		default:
			for _, o := range field.Options {
				q.Options = append(q.Options, agent.QuestionOption{Label: cmp.Or(o.Label, o.Value), Description: o.Description})
			}
			q.MultiSelect = field.Type == "multiselect"
			q.AllowOther = field.Custom || len(field.Options) == 0
		}
		req.Questions = append(req.Questions, q)
	}
	sid, fields := f.SessionID, f.Fields
	s.tr.reqSession[f.ID] = sid
	s.rt.Request(providerkit.NewUserInputRequest(req, func(ctx context.Context, resp agent.UserInputResponse) error {
		return s.replyForm(ctx, sid, req.ID, fields, resp)
	}))
	s.rt.Emit(&agent.StatusChanged{Status: agent.StatusWaiting})
}

func (s *Session) replyForm(ctx context.Context, sid, id string, fields []formField, resp agent.UserInputResponse) error {
	base := sessionPath(sid, "/form/") + url.PathEscape(id)
	var err error
	if resp.Cancelled {
		err = s.c.Call(ctx, "DELETE", base, nil, nil)
	} else {
		err = s.c.Call(ctx, "POST", base+"/reply", map[string]any{"answer": formAnswer(fields, resp.Answers)}, nil)
	}
	if t := errorTag(err); t == "FormNotFoundError" || t == "FormAlreadySettledError" || httpStatus(err) == 404 {
		return nil
	}
	return err
}

// formAnswer converts chosen labels and typed text to the field values
// OpenCode expects. Fields are kept in order, and a field whose conditions
// do not hold against the answers kept so far is dropped, as OpenCode
// evaluates them.
func formAnswer(fields []formField, answers map[string][]string) map[string]any {
	out := map[string]any{}
	// Hidden fields are never shown, and OpenCode's own interface fills in
	// required ones, so both take their defaults when unanswered;
	// conditions can refer to them.
	for _, f := range fields {
		if (f.Hidden || f.Required) && f.Type != "external" && len(answers[f.Key]) == 0 {
			if v, ok := defaultValue(f.Default); ok {
				out[f.Key] = v
			}
		}
	}
	for _, f := range fields {
		if f.Type == "external" {
			// An external field, such as an MCP URL elicitation, is
			// acknowledged by any answer that is not a cancellation;
			// OpenCode requires it whatever its conditions say.
			out[f.Key] = true
			continue
		}
		if !whenHolds(f.When, out) {
			delete(out, f.Key)
			continue
		}
		var values []string
		for _, v := range answers[f.Key] {
			if v = strings.TrimSpace(v); v == "" {
				continue
			}
			for _, o := range f.Options {
				if v == o.Label {
					v = o.Value
					break
				}
			}
			values = append(values, v)
		}
		if len(values) == 0 {
			continue
		}
		switch f.Type {
		case "multiselect":
			out[f.Key] = values
		case "boolean":
			switch strings.ToLower(values[0]) {
			case "yes", "true", "y", "1":
				out[f.Key] = true
			default:
				out[f.Key] = false
			}
		case "number", "integer":
			n, err := strconv.ParseFloat(values[0], 64)
			if err == nil && (f.Type == "number" || n == float64(int64(n))) {
				out[f.Key] = n
			}
		default:
			out[f.Key] = strings.Join(values, ", ")
		}
	}
	return out
}

// defaultValue decodes a field default into an answer value.
func defaultValue(raw json.RawMessage) (any, bool) {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v == nil {
		return nil, false
	}
	if list, ok := v.([]any); ok {
		out := make([]string, 0, len(list))
		for _, item := range list {
			out = append(out, jsString(item))
		}
		return out, true
	}
	return v, true
}

// whenHolds evaluates field conditions as OpenCode's server does: a
// missing answer fails both "eq" and "neq"; a list answer matches when it
// includes the value, compared strictly; any other answer matches only an
// equal value of the same type.
func whenHolds(conds []formWhen, kept map[string]any) bool {
	for _, w := range conds {
		got, ok := kept[w.Key]
		if !ok {
			return false
		}
		var want any
		_ = json.Unmarshal(w.Value, &want)
		var match bool
		switch got := got.(type) {
		case []string:
			// Strict, as OpenCode's server check: a number never matches.
			str, isString := want.(string)
			match = isString && slices.Contains(got, str)
		case string, bool, float64:
			match = got == want
		}
		if match != (w.Op != "neq") {
			return false
		}
	}
	return true
}

// jsString renders a JSON scalar as JavaScript's String does.
func jsString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return "null"
	}
	return fmt.Sprint(v)
}

// fileURI returns the RFC 8089 file URI of an absolute path written with
// forward slashes. On Windows a drive path gets a slash before its drive
// (file:///C:/x) and a UNC path names its server as the host
// (file://server/share/x).
func fileURI(slashPath string, windows bool) string {
	u := url.URL{Scheme: "file", Path: slashPath}
	if windows {
		if unc, ok := strings.CutPrefix(slashPath, "//"); ok {
			host, rest, _ := strings.Cut(unc, "/")
			u.Host, u.Path = host, "/"+rest
		} else if !strings.HasPrefix(slashPath, "/") {
			u.Path = "/" + slashPath
		}
	}
	return u.String()
}

// fileURIPath returns the path, with forward slashes, of a file URI, the
// inverse of fileURI. It reports false for another scheme, and for a URI
// naming another host where paths cannot (outside Windows) name one.
func fileURIPath(uri string, windows bool) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	path := u.Path
	switch {
	case u.Host != "" && u.Host != "localhost":
		if !windows {
			return "", false
		}
		return "//" + u.Host + path, true
	case windows && len(path) >= 3 && path[0] == '/' && path[2] == ':':
		return path[1:], true
	}
	return path, true
}
