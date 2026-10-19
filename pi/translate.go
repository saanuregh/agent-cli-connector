package pi

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

// translator turns Pi session events into uniform events. It runs on the
// reader goroutine.
type translator struct {
	s *Session

	// idPrefix makes the item IDs of assistant messages unique to this
	// translator, since Pi gives a message no ID until it ended.
	idPrefix string
	messages int
	msgKey   string
	blocks   map[int]string
	tools    map[string]*agent.Item
	partial  map[string]partialOutput
	users    int

	run           runState
	contextTokens int64
	liveTotal     int64
	// summaryAttempt is the latest retry of a compaction or branch
	// summary, until the retries finish.
	summaryAttempt int
	// pendingUsers wait for the bridge to report their session entry IDs.
	pendingUsers []pendingUser
	// subtasks holds the running subagent tasks of each tool call, to
	// close them if the call ends without reporting them finished.
	subtasks map[string]map[string]agent.Task
}

// runState is the state of one run, cleared when the run settles.
type runState struct {
	cycleStarted bool
	usage        agent.Usage
	steps        int
	text         string
	errMsg       string
	errDetail    string
	stopReason   string
	// output holds the arguments of the run's structured_output call.
	output json.RawMessage
}

type pendingUser struct {
	item  agent.Item
	stamp int64
	// first marks the turn's first user message, whose entry locates the
	// turn.
	first bool
	meta  agent.EventMeta
}

func newTranslator(s *Session) *translator {
	return &translator{
		s:        s,
		idPrefix: "msg-" + providerutil.NewUUID()[:8] + "-",
		subtasks: make(map[string]map[string]agent.Task),
		blocks:   make(map[int]string),
		tools:    make(map[string]*agent.Item),
		partial:  make(map[string]partialOutput),
	}
}

func (t *translator) emit(ev agent.Event) { t.s.rt.Emit(ev) }

type piUsage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	Reasoning   int64 `json:"reasoning"`
	TotalTokens int64 `json:"totalTokens"`
	Cost        struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// usage converts a message's usage; it leaves the context fields zero.
func (m *piUsage) usage() agent.Usage {
	return agent.Usage{
		InputTokens:      m.Input,
		OutputTokens:     m.Output,
		CacheReadTokens:  m.CacheRead,
		CacheWriteTokens: m.CacheWrite,
		ReasoningTokens:  m.Reasoning,
		TotalTokens:      m.TotalTokens,
		CostUSD:          m.Cost.Total,
	}
}

// contextTokens is the context a message filled: its input, cached input,
// and output.
func (m *piUsage) contextTokens() int64 {
	return m.Input + m.CacheRead + m.CacheWrite + m.Output
}

type piBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Data      string          `json:"data"`
	MimeType  string          `json:"mimeType"`
}

type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Usage        *piUsage        `json:"usage"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Model        string          `json:"model"`
	Provider     string          `json:"provider"`
	Command      string          `json:"command"`
	Output       string          `json:"output"`
	ExitCode     *int            `json:"exitCode"`
	Cancelled    bool            `json:"cancelled"`
	Summary      string          `json:"summary"`
	// Display is nil when absent, which Pi's terminal UI treats as hidden.
	Display       *bool  `json:"display"`
	Timestamp     int64  `json:"timestamp"`
	ResponseModel string `json:"responseModel"`
	ResponseID    string `json:"responseId"`
}

func (m piMessage) blocks() []piBlock {
	var blocks []piBlock
	if json.Unmarshal(m.Content, &blocks) == nil {
		return blocks
	}
	var text string
	if json.Unmarshal(m.Content, &text) == nil && text != "" {
		return []piBlock{{Type: "text", Text: text}}
	}
	return nil
}

// rawBlocks returns the native bytes of each content block, in the order
// of blocks, when the content is a list.
func (m piMessage) rawBlocks() []json.RawMessage {
	var raws []json.RawMessage
	_ = json.Unmarshal(m.Content, &raws)
	return raws
}

func (m piMessage) text() string {
	var parts []string
	for _, b := range m.blocks() {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// piEvent holds the fields of Pi's events other than message_update, which
// is the most frequent and decodes only piMessageUpdate. Message is decoded
// only by the events that read it.
type piEvent struct {
	Message          json.RawMessage `json:"message"`
	ToolCallID       string          `json:"toolCallId"`
	ToolName         string          `json:"toolName"`
	Args             json.RawMessage `json:"args"`
	PartialResult    json.RawMessage `json:"partialResult"`
	Result           json.RawMessage `json:"result"`
	IsError          bool            `json:"isError"`
	DurationMS       *int64          `json:"durationMs"`
	ParentToolCallID string          `json:"parentToolCallId"`
	Aborted          bool            `json:"aborted"`
	Steering         []string        `json:"steering"`
	FollowUp         []string        `json:"followUp"`
	Reason           string          `json:"reason"`
	ErrorMessage     string          `json:"errorMessage"`
	Attempt          int             `json:"attempt"`
	MaxAttempts      int             `json:"maxAttempts"`
	DelayMS          int64           `json:"delayMs"`
	Success          bool            `json:"success"`
	FinalError       string          `json:"finalError"`
	Name             *string         `json:"name"`
	Level            string          `json:"level"`
	ID               string          `json:"id"`
	Delta            string          `json:"delta"`
	Entry            json.RawMessage `json:"entry"`
	ExtensionPath    string          `json:"extensionPath"`
	Event            string          `json:"event"`
	Error            string          `json:"error"`
}

type piMessageUpdate struct {
	AssistantMessageEvent *messageEvent `json:"assistantMessageEvent"`
	Usage                 *piUsage      `json:"usage"`
}

type messageEvent struct {
	Type         string   `json:"type"`
	ContentIndex int      `json:"contentIndex"`
	Delta        string   `json:"delta"`
	ID           string   `json:"id"`
	ToolName     string   `json:"toolName"`
	ToolCall     *piBlock `json:"toolCall"`
}

func (ev *piEvent) message() piMessage {
	var m piMessage
	_ = json.Unmarshal(ev.Message, &m)
	return m
}

func (t *translator) handle(typ string, line []byte) {
	meta := agent.EventMeta{Raw: line}
	if typ == "message_update" {
		var u piMessageUpdate
		if json.Unmarshal(line, &u) == nil && u.AssistantMessageEvent != nil {
			t.messageUpdate(u.AssistantMessageEvent, meta)
		}
		t.liveUsage(u.Usage, meta)
		return
	}
	var ev piEvent
	_ = json.Unmarshal(line, &ev)
	switch typ {
	case "message_start", "turn_end", "agent_end", "agent_settled":
		// The bridge reports user entries before Pi prints these events.
		t.flushUsers()
	}
	switch typ {
	case "agent_start":
		t.agentStart(meta)
	case "turn_start":
		t.run.steps++
	case "message_start":
		if ev.message().Role == "assistant" {
			t.messages++
			t.msgKey = t.idPrefix + strconv.Itoa(t.messages)
			clear(t.blocks)
		}
	case "message_end":
		t.messageEnd(ev.message(), meta)
	case "tool_execution_start":
		item := t.toolItem(ev.ToolCallID, ev.ToolName, ev.ParentToolCallID, meta)
		if len(ev.Args) > 0 && !bytes.Equal(item.Tool.Input, ev.Args) {
			item.Tool.Input = ev.Args
			fillToolDetails(item.Tool, ev.Args)
			t.emit(&agent.ItemUpdated{EventMeta: meta, Item: item.Clone()})
		}
	case "tool_execution_update":
		cur := newPartialOutput(ev.PartialResult)
		delta := outputDelta(t.partial[ev.ToolCallID], &cur)
		t.partial[ev.ToolCallID] = cur
		if delta != "" {
			t.emit(&agent.ToolOutputDelta{EventMeta: meta, ItemID: ev.ToolCallID, Delta: delta})
		}
		if ev.ToolName == "subagent" {
			var args json.RawMessage
			if item, ok := t.tools[ev.ToolCallID]; ok {
				args = item.Tool.Input
			}
			t.subagentToolTasks(ev.ToolCallID, ev.PartialResult, args, false, meta)
		}
	case "tool_execution_end":
		var args json.RawMessage
		if item, ok := t.tools[ev.ToolCallID]; ok {
			args = item.Tool.Input
		}
		t.toolEnd(ev, meta)
		if ev.ToolName == "subagent" {
			t.subagentToolTasks(ev.ToolCallID, ev.Result, args, true, meta)
		}
		// Tasks the result did not report finished, as when the tool
		// failed or was aborted, end with it.
		status, errText := agent.TaskStopped, ""
		if ev.IsError {
			status, errText = agent.TaskFailed, cmp.Or(resultText(ev.Result), "the subagent tool failed")
		}
		t.closeSubtasks(ev.ToolCallID, status, errText, meta)
		if ev.ToolName == outputTool && !ev.IsError {
			var r struct {
				Details json.RawMessage `json:"details"`
			}
			if json.Unmarshal(ev.Result, &r) == nil && len(r.Details) > 0 && string(r.Details) != "null" {
				t.run.output = r.Details
			}
		}
		if ev.ToolName == "todo" && !ev.IsError {
			if steps, ok := todoSteps(ev.Result); ok {
				t.emit(&agent.PlanUpdated{EventMeta: meta, Steps: steps, ItemID: ev.ToolCallID})
			}
		}
	case "agent_settled":
		t.settled(ev.Aborted, line)
	case "queue_update":
		t.emit(&QueueUpdated{EventMeta: meta, Steering: ev.Steering, FollowUp: ev.FollowUp})
	case "compaction_start":
		t.s.compacting.Store(true)
		if id, pending := t.s.rt.Interrupting(); pending && !t.s.isBuiltin(id) {
			// Interrupted while the prompt started, before Pi began this
			// compaction: an abort now stops it. Sent off the reader
			// goroutine, which delivers the answer.
			go func() { _, _ = t.s.Command(t.s.rt.Context(), "abort", nil) }()
		}
		t.emit(&agent.Compaction{EventMeta: meta, Trigger: ev.Reason})
		t.emit(&agent.StatusChanged{EventMeta: meta, Status: agent.StatusCompacting})
	case "compaction_end":
		t.s.compacting.Store(false)
		var result *struct {
			Summary              string   `json:"summary"`
			TokensBefore         int64    `json:"tokensBefore"`
			EstimatedTokensAfter int64    `json:"estimatedTokensAfter"`
			Usage                *piUsage `json:"usage"`
		}
		_ = json.Unmarshal(ev.Result, &result)
		c := &agent.Compaction{EventMeta: meta, Done: true, Trigger: ev.Reason}
		current, _ := t.s.rt.Running()
		userRun := current != "" && !t.s.isBuiltin(current)
		switch {
		case result != nil:
			// The summary's model call is part of the run's cost, including
			// a compaction Pi runs before the prompt's run starts. Built-in
			// commands report it in their own result.
			if result.Usage != nil && userRun {
				t.run.usage.Add(result.Usage.usage())
			}
			c.Summary = result.Summary
			c.TokensBefore = result.TokensBefore
			c.TokensAfter = result.EstimatedTokensAfter
		case ev.Aborted:
			c.Error = "compaction aborted"
		default:
			c.Error = cmp.Or(ev.ErrorMessage, "compaction failed")
		}
		t.emit(c)
		status := agent.StatusIdle
		if userRun {
			status = agent.StatusRunning
		}
		t.emit(&agent.StatusChanged{EventMeta: meta, Status: status})
		if result != nil {
			go t.s.refreshContextUsage(current, t.turnUsage())
		}
	case "auto_retry_start":
		t.emit(&agent.Retry{EventMeta: meta, Attempt: ev.Attempt, MaxAttempts: ev.MaxAttempts, Delay: time.Duration(ev.DelayMS) * time.Millisecond, Message: ev.ErrorMessage})
	case "auto_retry_end":
		if ev.Success {
			t.emit(&agent.Retry{EventMeta: meta, Attempt: ev.Attempt, Done: true})
		} else {
			t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: "retry_failed", Message: ev.FinalError})
		}
	case "summarization_retry_scheduled":
		// Compaction and branch summaries retry like model requests; a
		// failure that ends the retries arrives as compaction_end.
		t.summaryAttempt = ev.Attempt
		t.emit(&agent.Retry{EventMeta: meta, Attempt: ev.Attempt, MaxAttempts: ev.MaxAttempts, Delay: time.Duration(ev.DelayMS) * time.Millisecond,
			Message: "summary: " + ev.ErrorMessage})
	case "summarization_retry_finished":
		if t.summaryAttempt > 0 {
			t.emit(&agent.Retry{EventMeta: meta, Attempt: t.summaryAttempt, Done: true})
			t.summaryAttempt = 0
		}
	case "session_info_changed":
		t.s.updateInfo(meta, func(info *agent.SessionInfo) {
			info.Title = ""
			if ev.Name != nil {
				info.Title = *ev.Name
			}
		})
	case "thinking_level_changed":
		t.s.updateInfo(meta, func(info *agent.SessionInfo) { info.Effort = effortOf(ev.Level) })
	case "bash_execution_update":
		t.emit(&agent.ToolOutputDelta{EventMeta: meta, ItemID: "bash-" + ev.ID, Delta: ev.Delta})
	case "extension_error":
		t.emit(&ExtensionError{EventMeta: meta, ExtensionPath: ev.ExtensionPath, Event: ev.Event, Error: ev.Error})
		t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: "extension_error", Message: ev.ExtensionPath + ": " + ev.Error})
	case "entry_appended":
		t.emit(&EntryAppended{EventMeta: meta, Entry: ev.Entry})
	case "agent_end", "turn_end":
	default:
		t.emit(&agent.RawEvent{EventMeta: meta, Type: typ})
	}
}

// agentStart marks the start of a turn. A run the host did not start, such
// as one an extension triggered, becomes a tracked turn.
func (t *translator) agentStart(meta agent.EventMeta) {
	current, _ := t.s.rt.RunStarted("")
	if t.run.cycleStarted {
		return
	}
	t.run.cycleStarted = true
	meta.TurnID = current
	t.emit(&agent.TurnStarted{EventMeta: meta})
	t.emit(&agent.StatusChanged{EventMeta: meta, Status: agent.StatusRunning})
}

// liveUsage reports the streaming message's cumulative usage, which Pi
// attaches to message updates, on top of the turn's finished messages.
func (t *translator) liveUsage(m *piUsage, meta agent.EventMeta) {
	if m == nil || m.TotalTokens <= 0 || m.TotalTokens == t.liveTotal {
		return
	}
	t.liveTotal = m.TotalTokens
	u := t.run.usage
	u.Add(m.usage())
	u.ContextTokens = m.contextTokens()
	u.ContextWindow = t.s.contextWindow()
	t.emit(&agent.UsageUpdated{EventMeta: meta, Usage: u})
}

// turnUsage returns the turn's usage with the context the last finished
// message filled.
func (t *translator) turnUsage() agent.Usage {
	u := t.run.usage
	u.ContextTokens = t.contextTokens
	u.ContextWindow = t.s.contextWindow()
	return u
}

func (t *translator) blockID(index int) string {
	if id, ok := t.blocks[index]; ok {
		return id
	}
	return t.msgKey + "/" + strconv.Itoa(index)
}

func (t *translator) messageUpdate(e *messageEvent, meta agent.EventMeta) {
	switch e.Type {
	case "text_start":
		id := t.msgKey + "/" + strconv.Itoa(e.ContentIndex)
		t.blocks[e.ContentIndex] = id
		t.emit(&agent.ItemStarted{EventMeta: meta, Item: agent.Item{ID: id, Kind: agent.ItemAssistantMessage, Status: agent.ItemRunning}})
	case "text_delta":
		t.emit(&agent.TextDelta{EventMeta: meta, ItemID: t.blockID(e.ContentIndex), Delta: e.Delta})
	case "thinking_start":
		id := t.msgKey + "/" + strconv.Itoa(e.ContentIndex)
		t.blocks[e.ContentIndex] = id
		t.emit(&agent.ItemStarted{EventMeta: meta, Item: agent.Item{ID: id, Kind: agent.ItemReasoning, Status: agent.ItemRunning}})
	case "thinking_delta":
		t.emit(&agent.ReasoningDelta{EventMeta: meta, ItemID: t.blockID(e.ContentIndex), Delta: e.Delta})
	case "toolcall_start":
		t.blocks[e.ContentIndex] = e.ID
		t.toolItem(e.ID, e.ToolName, "", meta)
	case "toolcall_delta":
		if e.Delta != "" {
			t.emit(&agent.ToolInputDelta{EventMeta: meta, ItemID: t.blockID(e.ContentIndex), Delta: e.Delta})
		}
	case "toolcall_end":
		if e.ToolCall != nil {
			item := t.toolItem(e.ToolCall.ID, e.ToolCall.Name, "", meta)
			item.Tool.Input = e.ToolCall.Arguments
			fillToolDetails(item.Tool, e.ToolCall.Arguments)
			t.emit(&agent.ItemUpdated{EventMeta: meta, Item: item.Clone()})
		}
	}
}

// toolItem returns the tool call item for id, emitting ItemStarted when it
// is new.
func (t *translator) toolItem(id, name, parent string, meta agent.EventMeta) *agent.Item {
	if item, ok := t.tools[id]; ok {
		return item
	}
	call := &agent.ToolCall{Name: name, Kind: toolKind(name, t.s.tools)}
	if call.Kind == agent.ToolMCP {
		server, _, _ := strings.Cut(strings.TrimPrefix(name, "mcp__"), "__")
		call.Server = server
	}
	item := &agent.Item{ID: id, Kind: agent.ItemToolCall, Status: agent.ItemRunning, ParentID: parent, Tool: call}
	t.tools[id] = item
	t.emit(&agent.ItemStarted{EventMeta: meta, Item: item.Clone()})
	return item
}

func toolKind(name string, hostTools map[string]agent.Tool) agent.ToolKind {
	if _, ok := hostTools[name]; ok {
		return agent.ToolHost
	}
	switch name {
	case "bash":
		return agent.ToolShell
	case "read":
		return agent.ToolRead
	case "edit", "write":
		return agent.ToolEdit
	case "grep", "find", "ls", "tool_search":
		return agent.ToolSearch
	case "web_search", "web_fetch", "fetch":
		return agent.ToolWeb
	case "subagent", "Agent", "task":
		return agent.ToolSubagent
	case "todo", "todos", "todo_write":
		return agent.ToolTodo
	case "ask_user_question", "question", "questionnaire":
		return agent.ToolQuestion
	}
	if strings.HasPrefix(name, "mcp__") {
		return agent.ToolMCP
	}
	return agent.ToolOther
}

// partialOutput is a tool's latest partial result: its text and, for a
// snapshot Pi truncated, the size of the whole output so far in bytes and
// whether the snapshot starts at a line start. emitted is how much of the
// whole output the deltas covered, and seen the latest of those bytes,
// ending at emitted.
type partialOutput struct {
	text      string
	total     int64
	lineStart bool
	emitted   int64
	seen      string
	// open marks a snapshot after a gap whose final newline is undecided
	// and not yet emitted; guess is 1 when it is likely there.
	open  bool
	guess int64
	// fullOutput is the file Pi writes the whole output to once it
	// truncates, on the machine Pi runs on.
	fullOutput string
}

// maxSeen bounds partialOutput.seen; a snapshot is at most Pi's 50 KB byte
// limit, so twice that always reaches back past its start.
const maxSeen = 128 << 10

func newPartialOutput(result json.RawMessage) partialOutput {
	p := partialOutput{text: resultText(result)}
	var r struct {
		Details struct {
			Truncation *struct {
				TotalBytes      int64 `json:"totalBytes"`
				LastLinePartial bool  `json:"lastLinePartial"`
			} `json:"truncation"`
			FullOutputPath string `json:"fullOutputPath"`
		} `json:"details"`
	}
	_ = json.Unmarshal(result, &r)
	p.fullOutput = r.Details.FullOutputPath
	if r.Details.Truncation != nil {
		p.total = r.Details.Truncation.TotalBytes
		// Without a cut line, Pi's snapshot starts right after a newline.
		p.lineStart = !r.Details.Truncation.LastLinePartial
	}
	return p
}

// outputDelta returns the output that cur adds to the previous partial
// result, and fills cur's emitted and seen.
//
// Pi's bash tool sends snapshots of only the latest output once it is
// large, marked with the total output size. Such a snapshot usually lacks
// the output's final newline, which the total counts, so where it sits in
// the whole output is found by checking both placements against the output
// already emitted. Otherwise a result
// that extends the previous one adds its new text, and a tool that replaced
// its progress text has the new text follow on a line of its own.
func outputDelta(prev partialOutput, cur *partialOutput) string {
	if cur.total == 0 {
		cur.emitted = int64(len(cur.text))
		cur.seen = cur.text[max(len(cur.text)-maxSeen, 0):]
		switch {
		case prev.total == 0 && strings.HasPrefix(cur.text, prev.text):
			return cur.text[len(prev.text):]
		case prev.text == "" || cur.text == "":
			return cur.text
		}
		return "\n" + cur.text
	}
	if prev.open {
		// Settle the previous snapshot's final newline: the hypothesis
		// this snapshot fits decides it, the guess first.
		with, without := prev, prev
		with.open, without.open = false, false
		with.seen, with.emitted = prev.seen+"\n", prev.total
		without.emitted = prev.total
		dw, okw := placeSnapshot(with, cur)
		do, oko := placeSnapshot(without, cur)
		if okw && oko && dw == 1 && do == 0 {
			// Both fit and give the same bytes but for cur's own final
			// newline, the bytes lost in the gap absorbing the difference:
			// emit the shared bytes and keep that newline open.
			delta := outputDelta(without, cur)
			cur.open, cur.guess = true, 1
			if !cur.lineStart && !strings.Contains(cur.text, "\n") {
				cur.guess = 0
			}
			cur.emitted = cur.total - cur.guess
			return delta
		}
		h, other := with, without
		if prev.guess == 0 {
			h, other = without, with
		}
		if _, ok := placeSnapshot(h, cur); !ok {
			if _, ok := placeSnapshot(other, cur); ok {
				h = other
			}
		}
		if h.seen != prev.seen {
			return "\n" + outputDelta(h, cur)
		}
		return outputDelta(h, cur)
	}
	newline, _ := placeSnapshot(prev, cur)
	start := cur.total - newline - int64(len(cur.text))
	full := cur.text + strings.Repeat("\n", int(newline))
	delta := ""
	switch from := prev.emitted - start; {
	case from == -1 && cur.lineStart:
		// The one byte skipped is the newline the snapshot starts after,
		// as when Pi left out a line too long for its byte limit.
		delta = "\n" + full
	case from < 0:
		// Pi skipped output between the snapshots, as it does when more
		// than its limit arrives within one 100 ms update; the gap is lost.
		// Nothing anchors this snapshot's final newline, so it waits for
		// the next snapshot (or the tool's end) to decide.
		cur.emitted, cur.seen = cur.total-newline, cur.text
		cur.open, cur.guess = true, newline
		return cur.text
	case from < int64(len(full)):
		delta = full[from:]
	}
	cur.emitted = cur.total
	cur.seen = prev.seen + delta
	cur.seen = cur.seen[max(len(cur.seen)-maxSeen, 0):]
	return delta
}

// finalNewline settles the final newline left open after a gap when the
// tool ended: from the output Pi's bash tool returns, whose tail is exact;
// else from the last byte of Pi's full output file, which Pi completes
// before it reports the end, when this process can read it (an aborted
// command returns no output); else by the guess.
func finalNewline(p partialOutput, result json.RawMessage) bool {
	var r struct {
		StructuredContent *struct {
			Output *string `json:"output"`
		} `json:"structuredContent"`
	}
	if json.Unmarshal(result, &r) == nil && r.StructuredContent != nil && r.StructuredContent.Output != nil {
		return strings.HasSuffix(*r.StructuredContent.Output, "\n")
	}
	// Only a regular file: opening a named pipe would block.
	if info, err := os.Lstat(p.fullOutput); p.fullOutput != "" && err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		if f, err := os.Open(p.fullOutput); err == nil {
			defer f.Close()
			last := make([]byte, 1)
			if _, err := f.ReadAt(last, info.Size()-1); err == nil {
				return last[0] == '\n'
			}
		}
	}
	return p.guess == 1
}

// placeSnapshot returns whether the final newline follows cur in the whole
// output, trying the likelier placement first, and whether that placement
// fits the output already emitted.
func placeSnapshot(prev partialOutput, cur *partialOutput) (newline int64, fits bool) {
	order := []int64{1, 0}
	if !cur.lineStart && !strings.Contains(cur.text, "\n") {
		// The snapshot is the end of one cut line, most likely still being
		// written. A run of one byte whose newline arrives alone can lose
		// that newline here; the completed item has it.
		order = []int64{0, 1}
	}
	for _, d := range order {
		if fitsAfter(prev, cur.total-d-int64(len(cur.text)), cur.text+strings.Repeat("\n", int(d)), cur.lineStart) {
			return d, true
		}
	}
	return 0, false
}

// fitsAfter reports whether text placed at offset start of the whole
// output agrees with the output already emitted where the two overlap,
// and, for a snapshot that starts a line, whether the byte before it is a
// newline. Without an overlap nothing contradicts it.
func fitsAfter(prev partialOutput, start int64, text string, lineStart bool) bool {
	if start < 0 {
		return false
	}
	seen := prev.seen
	seenStart := prev.emitted - int64(len(seen))
	if lineStart && start > seenStart && start <= prev.emitted && seen[start-1-seenStart] != '\n' {
		return false
	}
	lo := max(seenStart, start)
	hi := min(prev.emitted, start+int64(len(text)))
	if hi <= lo {
		return true
	}
	return seen[lo-seenStart:hi-seenStart] == text[lo-start:hi-start]
}

func fillToolDetails(call *agent.ToolCall, args json.RawMessage) {
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Query   string `json:"query"`
		URL     string `json:"url"`
	}
	_ = json.Unmarshal(args, &in)
	switch call.Kind {
	case agent.ToolShell:
		call.Command = in.Command
	case agent.ToolRead:
		call.Path = in.Path
	case agent.ToolEdit:
		call.Path = in.Path
		kind := agent.FileModified
		if call.Name == "write" {
			kind = agent.FileAdded
		}
		call.Changes = []agent.FileChange{{Path: in.Path, Kind: kind}}
	case agent.ToolSearch:
		call.Query = cmp.Or(in.Pattern, in.Query)
		call.Path = in.Path
	case agent.ToolWeb:
		call.URL = in.URL
		call.Query = in.Query
	}
}

func resultText(raw json.RawMessage) string {
	text, _ := resultContent(raw)
	return text
}

// resultContent returns the text and images of a tool result, or of a
// toolResult message, which has the same content field.
func resultContent(raw json.RawMessage) (string, []agent.Image) {
	var r struct {
		Content []piBlock `json:"content"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return "", nil
	}
	var parts []string
	var images []agent.Image
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			if data, err := base64.StdEncoding.DecodeString(b.Data); err == nil {
				images = append(images, agent.Image{MediaType: b.MimeType, Data: data})
			}
		}
	}
	return strings.Join(parts, ""), images
}

func (t *translator) toolEnd(ev piEvent, meta agent.EventMeta) {
	item := t.toolItem(ev.ToolCallID, ev.ToolName, ev.ParentToolCallID, meta)
	if p := t.partial[ev.ToolCallID]; p.open && finalNewline(p, ev.Result) {
		t.emit(&agent.ToolOutputDelta{EventMeta: meta, ItemID: ev.ToolCallID, Delta: "\n"})
	}
	delete(t.tools, ev.ToolCallID)
	delete(t.partial, ev.ToolCallID)
	item.Tool.Output, item.Tool.Images = resultContent(ev.Result)
	item.Tool.IsError = ev.IsError
	if ev.DurationMS != nil {
		item.Tool.Duration = time.Duration(*ev.DurationMS) * time.Millisecond
	}
	applyToolResult(item.Tool, ev.Result)
	t.s.mu.Lock()
	denied := t.s.denied[ev.ToolCallID]
	delete(t.s.denied, ev.ToolCallID)
	t.s.mu.Unlock()
	switch {
	case denied:
		item.Status = agent.ItemDeclined
	case ev.IsError:
		item.Status = agent.ItemFailed
	default:
		item.Status = agent.ItemDone
	}
	t.emit(&agent.ItemCompleted{EventMeta: meta, Item: *item})
}

// applyToolResult fills a tool call's output data, exit code, and diff
// from a tool result, or a toolResult message, which has the same details
// and structuredContent fields.
func applyToolResult(call *agent.ToolCall, result json.RawMessage) {
	var r struct {
		Details    json.RawMessage `json:"details"`
		Structured json.RawMessage `json:"structuredContent"`
	}
	_ = json.Unmarshal(result, &r)
	switch {
	case len(r.Structured) > 0 && string(r.Structured) != "null":
		call.OutputData = r.Structured
	case len(r.Details) > 0 && string(r.Details) != "null" && string(r.Details) != "{}":
		call.OutputData = r.Details
	}
	var details struct {
		ExitCode *int   `json:"exitCode"`
		Patch    string `json:"patch"`
		Diff     string `json:"diff"`
	}
	_ = json.Unmarshal(r.Details, &details)
	switch call.Kind {
	case agent.ToolShell:
		var sh struct {
			ExitCode *int `json:"exit_code"`
		}
		_ = json.Unmarshal(r.Structured, &sh)
		call.ExitCode = sh.ExitCode
		if call.ExitCode == nil {
			call.ExitCode = details.ExitCode
		}
	case agent.ToolEdit:
		if len(call.Changes) == 1 {
			call.Changes = slices.Clone(call.Changes)
			call.Changes[0].Diff = cmp.Or(details.Patch, details.Diff)
		}
	}
}

func (t *translator) messageEnd(m piMessage, meta agent.EventMeta) {
	switch m.Role {
	case "assistant":
		status := agent.ItemDone
		if m.StopReason == "aborted" {
			status = agent.ItemCancelled
		}
		var texts []string
		raws := m.rawBlocks()
		for i, b := range m.blocks() {
			switch b.Type {
			case "text", "thinking":
				kind := agent.ItemAssistantMessage
				text := b.Text
				if b.Type == "thinking" {
					kind = agent.ItemReasoning
					text = b.Thinking
				} else if text != "" {
					texts = append(texts, text)
				}
				id, started := t.blocks[i]
				if !started {
					id = t.msgKey + "/" + strconv.Itoa(i)
					t.emit(&agent.ItemStarted{EventMeta: meta, Item: agent.Item{ID: id, Kind: kind, Status: agent.ItemRunning}})
				}
				var raw json.RawMessage
				if i < len(raws) {
					raw = raws[i]
				}
				t.emit(&agent.ItemCompleted{EventMeta: meta, Item: agent.Item{ID: id, Kind: kind, Status: status, Text: text, Raw: raw}})
			case "toolCall":
				item := t.toolItem(b.ID, b.Name, "", meta)
				if len(item.Tool.Input) == 0 {
					item.Tool.Input = b.Arguments
					fillToolDetails(item.Tool, b.Arguments)
					t.emit(&agent.ItemUpdated{EventMeta: meta, Item: item.Clone()})
				}
			}
		}
		if len(texts) > 0 {
			t.run.text = strings.Join(texts, "\n")
		}
		t.liveTotal = 0
		if m.Usage != nil {
			t.run.usage.Add(m.Usage.usage())
			t.contextTokens = m.Usage.contextTokens()
			t.emit(&agent.UsageUpdated{EventMeta: meta, Usage: t.turnUsage()})
		}
		t.run.stopReason = m.StopReason
		if m.StopReason == "error" {
			t.run.errMsg = m.ErrorMessage
			t.run.errDetail = errorDetail(m)
			t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: "model_error", Message: m.ErrorMessage})
		} else if m.StopReason != "aborted" {
			t.run.errMsg = ""
			t.run.errDetail = ""
		}
	case "user":
		t.users++
		text := m.text()
		cur, _ := t.s.rt.Running()
		t.s.mu.Lock()
		first := false
		if cur != "" {
			if _, seen := t.s.turnStamp[cur]; !seen {
				t.s.turnStamp[cur] = m.Timestamp
				t.s.turnOrder = append(t.s.turnOrder, cur)
				if len(t.s.turnOrder) > maxRememberedTurns {
					old := t.s.turnOrder[0]
					t.s.turnOrder = t.s.turnOrder[1:]
					delete(t.s.turnStamp, old)
					delete(t.s.turnEntry, old)
				}
				first = true
			}
		}
		t.s.mu.Unlock()
		item := agent.Item{
			ID:       "user-" + strconv.Itoa(t.users) + "-" + strconv.FormatInt(m.Timestamp, 10),
			Kind:     agent.ItemUserMessage,
			Status:   agent.ItemDone,
			Text:     text,
			TurnID:   cur,
			ClientID: t.s.takeClientID(text),
			Raw:      meta.Raw,
		}
		if m.Timestamp > 0 {
			item.Time = time.UnixMilli(m.Timestamp)
		}
		if t.s.bridgeDisabled() {
			t.emit(&agent.ItemCompleted{EventMeta: meta, Item: item})
			return
		}
		// The item waits for its entry ID, which the bridge reports before
		// Pi's next event.
		t.pendingUsers = append(t.pendingUsers, pendingUser{item: item, stamp: m.Timestamp, first: first, meta: meta})
	case "custom":
		if m.Display == nil || !*m.Display {
			t.emit(&agent.RawEvent{EventMeta: meta, Type: "custom_message"})
			return
		}
		id := "custom-" + strconv.FormatInt(m.Timestamp, 10)
		t.emit(&agent.ItemCompleted{EventMeta: meta, Item: agent.Item{ID: id, Kind: agent.ItemOther, Status: agent.ItemDone, Text: m.text(), Raw: meta.Raw}})
	case "compactionSummary", "branchSummary":
		id := m.Role + "-" + strconv.FormatInt(m.Timestamp, 10)
		t.emit(&agent.ItemCompleted{EventMeta: meta, Item: agent.Item{ID: id, Kind: agent.ItemCompaction, Status: agent.ItemDone, Text: m.Summary, Raw: meta.Raw}})
	}
}

// userEntry gives a waiting user message item the session entry ID the
// bridge reported, and emits it.
func (t *translator) userEntry(text string) {
	var r struct {
		EntryID   string `json:"entryId"`
		Timestamp int64  `json:"timestamp"`
	}
	if json.Unmarshal([]byte(text), &r) != nil || r.EntryID == "" {
		return
	}
	i := slices.IndexFunc(t.pendingUsers, func(p pendingUser) bool { return p.stamp == r.Timestamp })
	if i < 0 {
		return
	}
	p := t.pendingUsers[i]
	t.pendingUsers = slices.Delete(t.pendingUsers, i, i+1)
	p.item.ID = r.EntryID
	if p.first && p.item.TurnID != "" {
		t.s.mu.Lock()
		t.s.turnEntry[p.item.TurnID] = r.EntryID
		t.s.mu.Unlock()
	}
	t.emit(&agent.ItemCompleted{EventMeta: p.meta, Item: p.item})
}

// flushUsers emits user message items whose entry ID never arrived, under
// their provisional IDs.
func (t *translator) flushUsers() {
	for _, p := range t.pendingUsers {
		t.emit(&agent.ItemCompleted{EventMeta: p.meta, Item: p.item})
	}
	t.pendingUsers = nil
}

// errorDetail describes a failed assistant message beyond its error text.
func errorDetail(m piMessage) string {
	var parts []string
	if m.StopReason != "" {
		parts = append(parts, "stop reason "+m.StopReason)
	}
	if m.Model != "" {
		parts = append(parts, "model "+strings.TrimPrefix(m.Provider+"/"+m.Model, "/"))
	}
	if m.ResponseModel != "" && m.ResponseModel != m.Model {
		parts = append(parts, "response model "+m.ResponseModel)
	}
	if m.ResponseID != "" {
		parts = append(parts, "response "+m.ResponseID)
	}
	return strings.Join(parts, ", ")
}

// settled completes the current turn when Pi has no more automatic work.
func (t *translator) settled(aborted bool, line []byte) {
	current, _ := t.s.rt.Running()
	if interrupted, _ := t.s.rt.Interrupting(); interrupted != "" && interrupted == current {
		aborted = true
	}

	usage := t.turnUsage()
	res := &agent.TurnResult{
		Status:     agent.TurnSucceeded,
		Text:       t.run.text,
		Usage:      usage,
		Steps:      t.run.steps,
		StopReason: t.run.stopReason,
		Raw:        line,
	}
	switch {
	case aborted:
		res.Status = agent.TurnInterrupted
		res.Err = &agent.TurnError{Code: "aborted", Err: agent.ErrInterrupted}
	case t.run.errMsg != "" || t.run.stopReason == "error":
		res.Status = agent.TurnFailed
		msg := cmp.Or(t.run.errMsg, "the model request failed")
		if t.run.errDetail != "" {
			msg += " (" + t.run.errDetail + ")"
		}
		res.Err = &agent.TurnError{Code: "error", Message: msg}
	}
	if wanted, wrapped := t.s.takeOutput(current); wanted {
		t.structuredOutput(res, wrapped)
	}
	t.run = runState{}
	for id, item := range t.tools {
		// Tool calls that never finished, such as ones the abort cut off.
		item.Status = agent.ItemCancelled
		t.emit(&agent.ItemCompleted{EventMeta: agent.EventMeta{Raw: line}, Item: *item})
		delete(t.tools, id)
		delete(t.partial, id)
	}
	for id := range t.subtasks {
		t.closeSubtasks(id, agent.TaskStopped, "", agent.EventMeta{Raw: line})
	}

	s := t.s
	switch {
	case current == "":
	case s.rt.HasTurn(current):
		s.rt.FinishTurn(current, res)
	default:
		s.rt.AbandonTurn(current, nil) // frees the slot
	}
	// Ending the turn may start queued input, which is not idle.
	if s.rt.Idle() {
		t.emit(&agent.StatusChanged{EventMeta: agent.EventMeta{Raw: line}, Status: agent.StatusIdle})
	}
	go s.refreshContextUsage(current, usage)
}

// maxRememberedTurns bounds the turns whose TurnID Rewind and ForkAtTurn
// map to session entries.
const maxRememberedTurns = 1000

// outputTool is the bridge tool that receives structured output.
const outputTool = "structured_output"

// structuredOutput sets the structured output of a turn that asked for
// it, or fails a turn that otherwise succeeded without it.
func (t *translator) structuredOutput(res *agent.TurnResult, wrapped bool) {
	out := t.run.output
	if wrapped && len(out) > 0 {
		var v struct {
			Value json.RawMessage `json:"value"`
		}
		if json.Unmarshal(out, &v) != nil || len(v.Value) == 0 {
			out = nil
		} else {
			out = v.Value
		}
	}
	if len(out) > 0 {
		res.StructuredOutput = out
		return
	}
	if res.Status == agent.TurnSucceeded {
		res.Status = agent.TurnFailed
		res.Err = &agent.TurnError{Code: "no_structured_output", Message: "the model did not call " + outputTool}
	}
}

// subagentToolTasks reports the runs of a subagent tool as subagent tasks
// and remembers their session files.
func (t *translator) subagentToolTasks(callID string, result, args json.RawMessage, done bool, meta agent.EventMeta) {
	tasks, files := subagentTasks(callID, result, args, done)
	if len(files) > 0 {
		t.s.mu.Lock()
		maps.Copy(t.s.subagentFiles, files)
		t.s.mu.Unlock()
	}
	for _, task := range tasks {
		running := t.subtasks[callID]
		if task.Status == agent.TaskRunning {
			if running == nil {
				running = map[string]agent.Task{}
				t.subtasks[callID] = running
			}
			running[task.ID] = task
		} else {
			delete(running, task.ID)
		}
		t.emit(&agent.TaskUpdated{EventMeta: meta, Task: task})
	}
}

// closeSubtasks ends the subagent tasks of tool call callID that are
// still running with status.
func (t *translator) closeSubtasks(callID string, status agent.TaskStatus, errText string, meta agent.EventMeta) {
	running := t.subtasks[callID]
	delete(t.subtasks, callID)
	for _, id := range slices.Sorted(maps.Keys(running)) {
		task := running[id]
		task.Status = status
		task.Error = errText
		t.emit(&agent.TaskUpdated{EventMeta: meta, Task: task})
	}
}

// subagentTasks reads the result of a "subagent" tool call. It understands
// Pi's example subagent extension and pi-subagents, whose details list one
// row per agent run, and @gotgenes/pi-subagents, whose details describe one
// agent. files maps task IDs to child session files, when reported.
func subagentTasks(callID string, result, args json.RawMessage, done bool) (tasks []agent.Task, files map[string]string) {
	var r struct {
		// Content is decoded so that a malformed one rejects the result.
		Content []piBlock `json:"content"`
		Details struct {
			Results []struct {
				Index        *int        `json:"index"`
				Agent        string      `json:"agent"`
				Task         string      `json:"task"`
				ExitCode     int         `json:"exitCode"`
				Success      *bool       `json:"success"`
				SessionFile  string      `json:"sessionFile"`
				Messages     []piMessage `json:"messages"`
				Stderr       string      `json:"stderr"`
				Model        string      `json:"model"`
				StopReason   string      `json:"stopReason"`
				ErrorMessage string      `json:"errorMessage"`
				Step         *int        `json:"step"`
				Finished     bool        `json:"finished"`
				Usage        *struct {
					Input      int64   `json:"input"`
					Output     int64   `json:"output"`
					CacheRead  int64   `json:"cacheRead"`
					CacheWrite int64   `json:"cacheWrite"`
					Cost       float64 `json:"cost"`
				} `json:"usage"`
			} `json:"results"`
			// @gotgenes/pi-subagents
			AgentID        string `json:"agentId"`
			Status         string `json:"status"`
			Description    string `json:"description"`
			SubagentType   string `json:"subagentType"`
			TranscriptPath string `json:"transcriptPath"`
			ToolUses       int    `json:"toolUses"`
			DurationMS     int64  `json:"durationMs"`
		} `json:"details"`
	}
	if json.Unmarshal(result, &r) != nil {
		return nil, nil
	}
	var in struct {
		Agent        string `json:"agent"`
		Task         string `json:"task"`
		SubagentType string `json:"subagent_type"`
		Prompt       string `json:"prompt"`
		Description  string `json:"description"`
	}
	_ = json.Unmarshal(args, &in)
	files = map[string]string{}
	d := r.Details
	if len(d.Results) == 0 && d.AgentID != "" {
		task := agent.Task{
			ID:          callID + ":subagent:0",
			Kind:        agent.TaskSubagent,
			NativeKind:  "subagent-tool",
			Status:      agent.TaskRunning,
			Description: cmp.Or(d.Description, in.Description, in.Prompt),
			ToolCallID:  callID,
			ToolUses:    d.ToolUses,
			Duration:    time.Duration(d.DurationMS) * time.Millisecond,
			Subagent:    &agent.Subagent{Type: cmp.Or(d.SubagentType, in.SubagentType), Prompt: in.Prompt, Depth: 1},
		}
		switch d.Status {
		case "completed":
			task.Status = agent.TaskCompleted
		case "error":
			task.Status = agent.TaskFailed
		case "aborted", "stopped":
			task.Status = agent.TaskStopped
		}
		if task.Status == agent.TaskFailed {
			task.Error = resultText(result)
		} else if task.Status != agent.TaskRunning {
			task.Output = resultText(result)
		}
		if d.TranscriptPath != "" {
			files[task.ID] = d.TranscriptPath
		}
		return []agent.Task{task}, files
	}
	for i, res := range d.Results {
		if res.Agent == "" {
			continue
		}
		step := i
		switch {
		case res.Index != nil:
			step = *res.Index
		case res.Step != nil:
			step = *res.Step
		}
		prompt := res.Task
		if prompt == "" && len(d.Results) == 1 {
			prompt = in.Task
		}
		task := agent.Task{
			ID:          callID + ":subagent:" + strconv.Itoa(step),
			Kind:        agent.TaskSubagent,
			NativeKind:  "subagent-tool",
			Status:      agent.TaskRunning,
			Description: prompt,
			ToolCallID:  callID,
			Subagent:    &agent.Subagent{Type: res.Agent, Prompt: prompt, Model: res.Model, Depth: 1},
		}
		output := lastAssistantText(res.Messages)
		if output == "" && len(d.Results) == 1 {
			output = resultText(result)
		}
		switch {
		case !done && !res.Finished:
			task.Summary = output
		case res.StopReason == "aborted":
			task.Status = agent.TaskStopped
		case res.ExitCode != 0 || res.StopReason == "error" || res.Success != nil && !*res.Success:
			task.Status = agent.TaskFailed
			task.Error = cmp.Or(res.ErrorMessage, strings.TrimSpace(res.Stderr), "exit code "+strconv.Itoa(res.ExitCode))
		default:
			task.Status = agent.TaskCompleted
		}
		if task.Status != agent.TaskRunning {
			task.Output = output
		}
		if u := res.Usage; u != nil {
			task.Usage = &agent.Usage{InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead,
				CacheWriteTokens: u.CacheWrite, TotalTokens: u.Input + u.Output + u.CacheRead + u.CacheWrite, CostUSD: u.Cost}
		}
		if res.SessionFile != "" {
			files[task.ID] = res.SessionFile
		}
		tasks = append(tasks, task)
	}
	return tasks, files
}

// todoSteps reads the task list from the result of a todo tool: Pi's
// example todo extension (details.todos) or rpiv-todo (details.tasks).
func todoSteps(result json.RawMessage) ([]agent.PlanStep, bool) {
	var r struct {
		Details struct {
			Error string `json:"error"`
			Todos *[]struct {
				ID   json.Number `json:"id"`
				Text string      `json:"text"`
				Done bool        `json:"done"`
			} `json:"todos"`
			Tasks *[]struct {
				ID         json.Number `json:"id"`
				Subject    string      `json:"subject"`
				Status     string      `json:"status"`
				ActiveForm string      `json:"activeForm"`
			} `json:"tasks"`
		} `json:"details"`
	}
	if json.Unmarshal(result, &r) != nil || r.Details.Error != "" {
		return nil, false
	}
	steps := []agent.PlanStep{}
	switch {
	case r.Details.Todos != nil:
		for _, td := range *r.Details.Todos {
			status := agent.StepPending
			if td.Done {
				status = agent.StepCompleted
			}
			steps = append(steps, agent.PlanStep{ID: td.ID.String(), Text: td.Text, Status: status})
		}
	case r.Details.Tasks != nil:
		for _, tk := range *r.Details.Tasks {
			var status agent.PlanStepStatus
			switch tk.Status {
			case "pending":
				status = agent.StepPending
			case "in_progress":
				status = agent.StepInProgress
			case "completed":
				status = agent.StepCompleted
			default:
				continue // deleted
			}
			steps = append(steps, agent.PlanStep{ID: tk.ID.String(), Text: tk.Subject, Status: status})
		}
	default:
		return nil, false
	}
	return steps, true
}

func lastAssistantText(msgs []piMessage) string {
	for _, msg := range slices.Backward(msgs) {
		if msg.Role != "assistant" {
			continue
		}
		var blocks []piBlock
		_ = json.Unmarshal(msg.Content, &blocks)
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}
