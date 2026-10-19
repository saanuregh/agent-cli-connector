package codex

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/saanuregh/agent-cli-connector/providerkit"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/jsonrpc"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

// tokenUsage is a TokenUsageBreakdown.
type tokenUsage struct {
	TotalTokens           int64 `json:"totalTokens"`
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
}

func (u tokenUsage) minus(o tokenUsage) tokenUsage {
	return tokenUsage{
		TotalTokens:           u.TotalTokens - o.TotalTokens,
		InputTokens:           u.InputTokens - o.InputTokens,
		CachedInputTokens:     u.CachedInputTokens - o.CachedInputTokens,
		CacheWriteInputTokens: max(u.CacheWriteInputTokens-o.CacheWriteInputTokens, 0),
		OutputTokens:          u.OutputTokens - o.OutputTokens,
		ReasoningOutputTokens: u.ReasoningOutputTokens - o.ReasoningOutputTokens,
	}
}

func (u tokenUsage) plus(o tokenUsage) tokenUsage {
	return tokenUsage{
		TotalTokens:           u.TotalTokens + o.TotalTokens,
		InputTokens:           u.InputTokens + o.InputTokens,
		CachedInputTokens:     u.CachedInputTokens + o.CachedInputTokens,
		CacheWriteInputTokens: u.CacheWriteInputTokens + o.CacheWriteInputTokens,
		OutputTokens:          u.OutputTokens + o.OutputTokens,
		ReasoningOutputTokens: u.ReasoningOutputTokens + o.ReasoningOutputTokens,
	}
}

// below reports whether any running total of u is lower than in o, as
// after a revert or compaction.
func (u tokenUsage) below(o tokenUsage) bool {
	return u.TotalTokens < o.TotalTokens || u.InputTokens < o.InputTokens || u.CachedInputTokens < o.CachedInputTokens ||
		u.OutputTokens < o.OutputTokens || u.ReasoningOutputTokens < o.ReasoningOutputTokens
}

func (u tokenUsage) usage() agent.Usage {
	return agent.Usage{
		// Codex counts cached input inside input; the uniform fields keep
		// them apart.
		InputTokens:      u.InputTokens - u.CachedInputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CachedInputTokens,
		CacheWriteTokens: u.CacheWriteInputTokens,
		ReasoningTokens:  u.ReasoningOutputTokens,
		TotalTokens:      u.TotalTokens,
	}
}

// translator turns app-server messages for one thread into events. Its
// state is guarded by the session mutex.
type translator struct {
	s *Session

	turnItems   map[string][]agent.Item
	finalText   map[string]string
	schemaTurns map[string]bool
	// startSchema is set while a turn/start that asked for an output
	// schema is in flight and its turn has not started yet.
	startSchema bool
	early       map[string]*agent.TurnResult
	items       map[string]agent.Item
	// running lists started main-thread items in start order, and
	// itemTurn maps them to their turn.
	running    []string
	itemTurn   map[string]string
	children   map[string]string // child thread ID → spawning item
	tasks      map[string]*agent.Task
	childTurns map[string]string // child thread ID → running turn
	// childRunning lists the started items of each child thread in start
	// order, and childOrder lists child threads in the order they became
	// known, so the oldest ended ones can be forgotten.
	childRunning map[string][]agent.Item
	childOrder   []string
	// stopped holds recent turns that ended interrupted or failed, whose
	// late subagent turns are interrupted too.
	stopped []string
	// usageTotal is the thread total at the latest usage update, valid when
	// haveTotal; turnUsage sums the current turn's share.
	usageTotal    tokenUsage
	haveTotal     bool
	turnUsage     tokenUsage
	contextLimit  int64
	contextTokens int64
	retries       map[string]agent.Retry // turn → latest retry
	// rateLimits holds the merged native snapshot per limit ID.
	rateLimits map[string]map[string]json.RawMessage
	// turnOrder lists recent main-thread turns in start order, and
	// asyncTurns maps pending async question requests to their turn, so
	// Rewind can withdraw the questions of removed turns.
	turnOrder  []string
	asyncTurns map[string]string
	// turnItemIDs lists the IDs of the items reported for each turn in
	// turnOrder, so Rewind can report them removed.
	turnItemIDs map[string][]string
}

// maxTurnOrder bounds translator.turnOrder.
const maxTurnOrder = 256

// maxChildren bounds the child threads a translator remembers; beyond it
// the oldest ended ones are forgotten.
const maxChildren = 256

func newTranslator(s *Session) *translator {
	return &translator{
		s:            s,
		turnItems:    make(map[string][]agent.Item),
		finalText:    make(map[string]string),
		schemaTurns:  make(map[string]bool),
		early:        make(map[string]*agent.TurnResult),
		items:        make(map[string]agent.Item),
		children:     make(map[string]string),
		tasks:        make(map[string]*agent.Task),
		childTurns:   make(map[string]string),
		itemTurn:     make(map[string]string),
		turnItemIDs:  make(map[string][]string),
		childRunning: make(map[string][]agent.Item),
		retries:      make(map[string]agent.Retry),
		rateLimits:   make(map[string]map[string]json.RawMessage),
		asyncTurns:   make(map[string]string),
		// A new thread starts from zero; a resumed one has unknown totals.
		haveTotal: s.cfg.Resume == "",
	}
}

func (t *translator) emit(ev agent.Event) { t.s.rt.Emit(ev) }

// beginStart records, before turn/start is sent, whether the turn asks for
// an output schema: Codex can start and finish the turn before the reply
// to turn/start is handled.
func (t *translator) beginStart(schema bool) {
	t.s.mu.Lock()
	t.startSchema = schema
	t.s.mu.Unlock()
}

// endStart ends the turn/start of turnID, empty when it failed. A schema
// not already given to the turn by turnStarted is given to it now.
func (t *translator) endStart(turnID string) {
	t.s.mu.Lock()
	if t.startSchema && turnID != "" {
		t.schemaTurns[turnID] = true
	}
	t.startSchema = false
	t.s.mu.Unlock()
}

func (t *translator) takeAllEarlyLocked() []*agent.TurnResult {
	out := slices.Collect(maps.Values(t.early))
	clear(t.early)
	return out
}

func (t *translator) takeEarlyResultLocked(turnID string) *agent.TurnResult {
	res := t.early[turnID]
	delete(t.early, turnID)
	return res
}

type turnParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
	Turn     struct {
		ID         string         `json:"id"`
		RootTurnID *string        `json:"rootTurnId"`
		Status     string         `json:"status"`
		Error      *wireTurnError `json:"error"`
		DurationMS *int64         `json:"durationMs"`
	} `json:"turn"`
	Item json.RawMessage `json:"item"`
}

// childParent returns the spawning tool call when threadID is a subagent
// thread of this session.
func (t *translator) childParent(threadID string) (string, bool) {
	if threadID == "" || threadID == t.s.ID() {
		return "", false
	}
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	parent, ok := t.children[threadID]
	return parent, ok
}

func (t *translator) notify(msg *jsonrpc.Message, line []byte) {
	meta := agent.EventMeta{Raw: line}
	var p turnParams
	_ = json.Unmarshal(msg.Params, &p)
	meta.TurnID = p.TurnID
	if msg.Method == "serverRequest/resolved" {
		var f struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.s.rt.WithdrawRequest(requestKey(f.RequestID))
		return
	}
	if parent, ok := t.childParent(p.ThreadID); ok {
		t.childNotify(msg, p, parent, meta)
		return
	}
	switch msg.Method {
	case "turn/started":
		t.turnStarted(p.Turn.ID, meta)
	case "turn/completed":
		t.turnCompleted(p, line)
	case "item/started", "item/completed":
		t.item(msg.Method == "item/completed", p, "", meta)
	case "item/agentMessage/delta":
		t.emit(&agent.TextDelta{EventMeta: meta, ItemID: p.ItemID, Delta: p.Delta})
	case "item/plan/delta":
		t.emit(&agent.PlanDelta{EventMeta: meta, ItemID: p.ItemID, Delta: p.Delta})
	case "item/reasoning/summaryTextDelta":
		t.emit(&agent.ReasoningDelta{EventMeta: meta, ItemID: p.ItemID, Delta: p.Delta, Summary: true})
	case "item/reasoning/summaryPartAdded":
		t.emit(&agent.ReasoningDelta{EventMeta: meta, ItemID: p.ItemID, Delta: "\n\n", Summary: true})
	case "item/reasoning/textDelta":
		t.emit(&agent.ReasoningDelta{EventMeta: meta, ItemID: p.ItemID, Delta: p.Delta})
	case "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
		t.emit(&agent.ToolOutputDelta{EventMeta: meta, ItemID: p.ItemID, Delta: p.Delta})
	case "item/mcpToolCall/progress":
		var f struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.emit(&agent.ToolOutputDelta{EventMeta: meta, ItemID: p.ItemID, Delta: f.Message + "\n"})
	case "item/commandExecution/terminalInteraction":
		var f struct {
			ProcessID string `json:"processId"`
			Stdin     string `json:"stdin"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.emit(&TerminalInteraction{EventMeta: meta, ItemID: p.ItemID, ProcessID: f.ProcessID, Stdin: f.Stdin})
	case "turn/plan/updated":
		var f struct {
			Explanation *string `json:"explanation"`
			Plan        []struct {
				Step   string `json:"step"`
				Status string `json:"status"`
			} `json:"plan"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		steps := make([]agent.PlanStep, 0, len(f.Plan))
		for _, st := range f.Plan {
			status := agent.StepPending
			switch st.Status {
			case "inProgress":
				status = agent.StepInProgress
			case "completed":
				status = agent.StepCompleted
			}
			steps = append(steps, agent.PlanStep{Text: st.Step, Status: status})
		}
		ev := &agent.PlanUpdated{EventMeta: meta, Steps: steps}
		if f.Explanation != nil {
			ev.Explanation = *f.Explanation
		}
		t.emit(ev)
	case "turn/diff/updated":
		var f struct {
			Diff string `json:"diff"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.emit(&DiffUpdated{EventMeta: meta, Diff: f.Diff})
	case "thread/tokenUsage/updated":
		t.tokenUsage(p.TurnID, msg.Params, meta)
	case "thread/status/changed":
		var f struct {
			Status struct {
				Type        string   `json:"type"`
				ActiveFlags []string `json:"activeFlags"`
			} `json:"status"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		switch f.Status.Type {
		case "idle":
			t.emit(&agent.StatusChanged{EventMeta: meta, Status: agent.StatusIdle})
		case "active":
			status := agent.StatusRunning
			for _, flag := range f.Status.ActiveFlags {
				if strings.HasPrefix(flag, "waitingOn") {
					status = agent.StatusWaiting
				}
			}
			t.emit(&agent.StatusChanged{EventMeta: meta, Status: status})
		case "systemError":
			t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: "systemError", Message: "thread entered a system error state"})
		}
	case "error":
		var f struct {
			Error     wireTurnError `json:"error"`
			WillRetry bool          `json:"willRetry"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		if f.WillRetry {
			t.emit(t.retry(p.TurnID, f.Error, meta))
		} else {
			t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeError, Code: errorCode(f.Error.CodexErrorInfo), Message: f.Error.message()})
		}
	case "thread/name/updated":
		var f struct {
			Name *string `json:"threadName"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.s.updateInfo(meta, func(i *agent.SessionInfo) {
			i.Title = ""
			if f.Name != nil {
				i.Title = *f.Name
			}
		})
	case "thread/settings/updated":
		var f struct {
			Settings struct {
				Model  *string `json:"model"`
				Effort *string `json:"effort"`
			} `json:"threadSettings"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.s.updateInfo(meta, func(i *agent.SessionInfo) {
			if f.Settings.Model != nil {
				i.Model = *f.Settings.Model
				t.s.model = *f.Settings.Model
			}
			if f.Settings.Effort != nil {
				i.Effort = agent.Effort(*f.Settings.Effort)
			}
		})
	case "model/rerouted":
		var f struct {
			From   string `json:"fromModel"`
			To     string `json:"toModel"`
			Reason any    `json:"reason"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeWarning, Code: "model_rerouted", Message: fmt.Sprintf("model rerouted from %s to %s (%v)", f.From, f.To, f.Reason)})
		t.s.updateInfo(meta, func(i *agent.SessionInfo) { i.Model = f.To })
	case "warning", "configWarning", "deprecationNotice", "guardianWarning":
		var f struct {
			Message string `json:"message"`
			Summary string `json:"summary"`
			Details string `json:"details"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeWarning, Code: msg.Method, Message: cmp.Or(f.Message, f.Summary, f.Details)})
	case "mcpServer/startupStatus/updated":
		var f struct {
			Name   string  `json:"name"`
			Status string  `json:"status"`
			Error  *string `json:"error"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.s.updateInfo(meta, func(i *agent.SessionInfo) {
			status := agent.MCPServerStatus{Name: f.Name, Status: f.Status}
			if f.Error != nil {
				status.Error = *f.Error
			}
			idx := slices.IndexFunc(i.MCPServers, func(m agent.MCPServerStatus) bool { return m.Name == f.Name })
			if idx >= 0 {
				i.MCPServers[idx] = status
			} else {
				i.MCPServers = append(i.MCPServers, status)
			}
		})
	case "hook/started", "hook/completed":
		var f struct {
			Run struct {
				ID            string  `json:"id"`
				EventName     string  `json:"eventName"`
				SourcePath    string  `json:"sourcePath"`
				Status        string  `json:"status"`
				StatusMessage *string `json:"statusMessage"`
				Entries       []struct {
					Text string `json:"text"`
				} `json:"entries"`
			} `json:"run"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		var out []string
		for _, e := range f.Run.Entries {
			out = append(out, e.Text)
		}
		if f.Run.StatusMessage != nil && *f.Run.StatusMessage != "" {
			out = append(out, *f.Run.StatusMessage)
		}
		t.emit(&agent.HookRun{EventMeta: meta, ID: f.Run.ID, Event: f.Run.EventName, Name: f.Run.SourcePath,
			Done: msg.Method == "hook/completed", Status: f.Run.Status, Outcome: hookOutcome(f.Run.Status), Output: strings.Join(out, "\n")})
	case "thread/goal/updated":
		var f struct {
			Goal goalWire `json:"goal"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.emit(&agent.GoalUpdated{EventMeta: meta, Goal: f.Goal.goal()})
	case "thread/goal/cleared":
		t.emit(&agent.GoalUpdated{EventMeta: meta})
	case "account/rateLimits/updated":
		var f struct {
			RateLimits map[string]json.RawMessage `json:"rateLimits"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		snap := t.mergeRateLimits(f.RateLimits)
		if snap.id() == mainLimitID {
			t.emit(&agent.RateLimitUpdated{EventMeta: meta, Limits: snap.limits()})
		} else {
			t.emit(&OtherRateLimitUpdated{EventMeta: meta, LimitID: snap.id(), LimitName: providerutil.Deref(snap.LimitName), Limits: snap.limits()})
		}
	case "thread/closed":
		t.emit(&agent.Notice{EventMeta: meta, Level: agent.NoticeWarning, Code: "thread_closed", Message: "the app server closed the thread"})
		t.s.client.unregister(t.s.ID())
		t.s.rt.Finish(fmt.Errorf("codex: thread %s was closed by the app server", t.s.ID()))
	default:
		t.emit(&agent.RawEvent{EventMeta: meta, Type: msg.Method})
	}
}

// hookOutcome normalizes a HookRunStatus. A "stopped" hook halted the
// prompt or turn it ran for, so it counts as blocked.
func hookOutcome(status string) agent.HookOutcome {
	switch status {
	case "completed":
		return agent.HookSucceeded
	case "failed":
		return agent.HookFailed
	case "blocked", "stopped":
		return agent.HookBlocked
	}
	return ""
}

func errorCode(info json.RawMessage) string {
	if nonNull(info) == nil {
		return ""
	}
	var s string
	if json.Unmarshal(info, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(info, &m) == nil {
		for k := range m {
			return k
		}
	}
	return string(info)
}

func requestKey(id json.RawMessage) string {
	var n int64
	if json.Unmarshal(id, &n) == nil {
		return fmt.Sprint(n)
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s
	}
	return string(id)
}

// wireTurnError is a native TurnError.
type wireTurnError struct {
	Message           string          `json:"message"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
	AdditionalDetails *string         `json:"additionalDetails"`
}

// message joins the message and its additional details.
func (e wireTurnError) message() string {
	details := strings.TrimSpace(providerutil.Deref(e.AdditionalDetails))
	if details == "" || details == e.Message {
		return e.Message
	}
	if e.Message == "" {
		return details
	}
	return e.Message + ": " + details
}

// httpStatus returns the HTTP status a codexErrorInfo object carries.
func httpStatus(info json.RawMessage) int {
	var m map[string]struct {
		Status *int `json:"httpStatusCode"`
	}
	if json.Unmarshal(info, &m) != nil {
		return 0
	}
	for _, v := range m {
		if v.Status != nil {
			return *v.Status
		}
	}
	return 0
}

var retryProgress = regexp.MustCompile(`\b(\d+)\s*/\s*(\d+)\b`)

// retry builds a Retry event, reading "N/M" attempt progress from the
// message and counting on from the turn's previous retry otherwise.
func (t *translator) retry(turnID string, e wireTurnError, meta agent.EventMeta) *agent.Retry {
	ev := &agent.Retry{EventMeta: meta, Message: e.message(), Code: errorCode(e.CodexErrorInfo), HTTPStatus: httpStatus(e.CodexErrorInfo)}
	t.s.mu.Lock()
	prev := t.retries[turnID]
	ev.Attempt, ev.MaxAttempts = prev.Attempt+1, prev.MaxAttempts
	if m := retryProgress.FindStringSubmatch(e.Message); m != nil {
		attempt, _ := strconv.Atoi(m[1])
		maxAttempts, _ := strconv.Atoi(m[2])
		if attempt > 0 && maxAttempts > 0 {
			ev.Attempt, ev.MaxAttempts = attempt, maxAttempts
		}
	}
	t.retries[turnID] = agent.Retry{Attempt: ev.Attempt, MaxAttempts: ev.MaxAttempts}
	t.s.mu.Unlock()
	return ev
}

// turnError maps a failed turn's error. Usage and rate limits are
// retryable and carry the time the exhausted windows reset.
func (t *translator) turnError(e *wireTurnError) *agent.TurnError {
	out := &agent.TurnError{Code: "failed"}
	if e == nil {
		return out
	}
	out.Message = e.message()
	out.HTTPStatus = httpStatus(e.CodexErrorInfo)
	if c := errorCode(e.CodexErrorInfo); c != "" {
		out.Code = c
	}
	switch out.Code {
	case "usageLimitExceeded", "rateLimitExceeded":
		out.Retryable = true
		t.s.mu.Lock()
		out.ResetsAt = t.rateLimitSnapshotLocked(mainLimitID).resetsAt()
		t.s.mu.Unlock()
	case "serverOverloaded":
		out.Retryable = true
	}
	return out
}

func (t *translator) turnStarted(id string, meta agent.EventMeta) {
	s := t.s
	s.mu.Lock()
	t.turnUsage = tokenUsage{}
	// A turn starting while a turn/start is in flight is that start's
	// turn (see Runtime.RunStarted), so it takes the start's schema.
	if t.startSchema && id != "" {
		t.schemaTurns[id] = true
		t.startSchema = false
	}
	if id != "" && !slices.Contains(t.turnOrder, id) {
		t.turnOrder = append(t.turnOrder, id)
		if len(t.turnOrder) > maxTurnOrder {
			delete(t.turnItemIDs, t.turnOrder[0])
			t.turnOrder = t.turnOrder[1:]
		}
	}
	s.mu.Unlock()
	// While a start is in flight, the start registers its turn.
	if _, starting := s.rt.RunStarted(id); !starting {
		s.rt.TrackTurn(id)
	}
	meta.TurnID = id
	t.emit(&agent.TurnStarted{EventMeta: meta})
}

func (t *translator) turnCompleted(p turnParams, line []byte) {
	s := t.s
	// A compaction that failed ends with its turn rather than its item.
	s.mu.Lock()
	if s.compacting {
		s.endCompactionLocked()
	}
	s.mu.Unlock()
	id := p.Turn.ID
	status := p.Turn.Status
	if interrupting, pending := s.rt.Interrupting(); id != "" && interrupting == id && !pending && status == "completed" {
		// Codex can finish a turn normally while the interrupt is in
		// flight; the caller asked for it to stop.
		status = "interrupted"
	}
	res := &agent.TurnResult{TurnID: id, Status: agent.TurnSucceeded, Raw: line, StopReason: p.Turn.Status}
	if p.Turn.DurationMS != nil {
		res.Duration = time.Duration(*p.Turn.DurationMS) * time.Millisecond
	}
	switch status {
	case "interrupted":
		res.Status = agent.TurnInterrupted
		res.Err = &agent.TurnError{Code: "interrupted", Err: agent.ErrInterrupted}
	case "failed":
		res.Status = agent.TurnFailed
		res.Err = t.turnError(p.Turn.Error)
	}
	if res.Status != agent.TurnSucceeded {
		t.endRunningItems(id, res.Status)
	}
	if res.Status == agent.TurnInterrupted {
		// An interrupt dismisses the questions the agent asked.
		t.withdrawAsync(func(string) bool { return true })
	}

	s.mu.Lock()
	if res.Status != agent.TurnSucceeded {
		t.stopped = append(t.stopped, id)
		if len(t.stopped) > maxStoppedTurns {
			t.stopped = t.stopped[1:]
		}
	}
	delete(t.retries, id)
	res.Items = t.turnItems[id]
	delete(t.turnItems, id)
	res.Text = t.finalText[id]
	delete(t.finalText, id)
	if t.schemaTurns[id] && json.Valid([]byte(res.Text)) {
		res.StructuredOutput = json.RawMessage(res.Text)
	}
	delete(t.schemaTurns, id)
	res.Usage = t.turnUsageLocked()
	t.turnUsage = tokenUsage{}
	if _, starting := s.rt.Running(); starting && !s.rt.HasTurn(id) {
		t.early[id] = res
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.rt.FinishTurn(id, res)
}

// maxStoppedTurns bounds translator.stopped.
const maxStoppedTurns = 32

// endRunningItems completes the tool items of a stopped turn that Codex
// left running, so they do not stay in progress forever.
func (t *translator) endRunningItems(turnID string, status agent.TurnStatus) {
	itemStatus := agent.ItemCancelled
	if status == agent.TurnFailed {
		itemStatus = agent.ItemFailed
	}
	var ended []agent.Item
	t.s.mu.Lock()
	t.running = slices.DeleteFunc(t.running, func(id string) bool {
		it, ok := t.items[id]
		if !ok || t.itemTurn[id] != turnID {
			return false
		}
		delete(t.items, id)
		delete(t.itemTurn, id)
		if it.Tool != nil {
			call := *it.Tool
			call.IsError = status == agent.TurnFailed
			it.Tool = &call
		}
		it.Status = itemStatus
		t.turnItems[turnID] = append(t.turnItems[turnID], it)
		ended = append(ended, it)
		return true
	})
	t.s.mu.Unlock()
	for _, it := range ended {
		t.emit(&agent.ItemCompleted{EventMeta: agent.EventMeta{TurnID: turnID}, Item: it})
	}
}

// tokenUsage adds the growth of the thread total to the running turn. A
// total that went down, as after a revert or compaction, counts the latest
// request's usage instead; updates for other turns only move the baseline.
func (t *translator) tokenUsage(turnID string, params json.RawMessage, meta agent.EventMeta) {
	var f struct {
		TokenUsage struct {
			Total              tokenUsage `json:"total"`
			Last               tokenUsage `json:"last"`
			ModelContextWindow *int64     `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &f) != nil {
		return
	}
	total, last := f.TokenUsage.Total, f.TokenUsage.Last
	t.s.mu.Lock()
	if f.TokenUsage.ModelContextWindow != nil {
		t.contextLimit = *f.TokenUsage.ModelContextWindow
	}
	t.contextTokens = last.InputTokens + last.OutputTokens
	running, _ := t.s.rt.Running()
	counted := turnID == "" || turnID == running
	if counted {
		delta := last
		if t.haveTotal && !total.below(t.usageTotal) {
			delta = total.minus(t.usageTotal)
		}
		t.turnUsage = t.turnUsage.plus(delta)
	}
	t.usageTotal, t.haveTotal = total, true
	u := t.turnUsageLocked()
	t.s.mu.Unlock()
	if counted {
		t.emit(&agent.UsageUpdated{EventMeta: meta, Usage: u})
	}
}

// turnUsageLocked returns the running turn's usage with the context
// window figures. The caller holds s.mu.
func (t *translator) turnUsageLocked() agent.Usage {
	u := t.turnUsage.usage()
	u.ContextWindow = t.contextLimit
	u.ContextTokens = t.contextTokens
	return u
}

func (t *translator) item(completed bool, p turnParams, parent string, meta agent.EventMeta) {
	it, extra := convertItem(p.Item)
	it.ParentID = parent
	it.TurnID = p.TurnID
	if !completed {
		it.Status = agent.ItemRunning
	}
	s := t.s
	s.mu.Lock()
	t.noteItemLocked(p.TurnID, it.ID)
	if completed {
		if parent == "" {
			t.turnItems[p.TurnID] = append(t.turnItems[p.TurnID], it)
			if it.Kind == agent.ItemAssistantMessage && (extra.phase == "final_answer" || extra.phase == "" || t.finalText[p.TurnID] == "") {
				t.finalText[p.TurnID] = it.Text
			}
		}
		delete(t.items, it.ID)
		delete(t.itemTurn, it.ID)
		t.running = slices.DeleteFunc(t.running, func(id string) bool { return id == it.ID })
	} else {
		if _, known := t.items[it.ID]; !known {
			t.running = append(t.running, it.ID)
		}
		t.items[it.ID] = it
		t.itemTurn[it.ID] = p.TurnID
	}
	trigger := "auto"
	var restore map[string]any
	if it.Kind == agent.ItemCompaction {
		if s.compacting {
			trigger = "manual"
		}
		if completed {
			s.endCompactionLocked()
			if !s.opts.DisableContextRestore {
				restore = s.context
			}
		}
	}
	s.mu.Unlock()
	for _, child := range extra.children {
		t.addChild(child, it.ID)
	}
	if parent == "" && completed {
		t.subagentItem(p.Item, "", meta)
	}

	if it.Kind == agent.ItemCompaction {
		t.emit(&agent.Compaction{EventMeta: meta, Done: completed, Trigger: trigger})
	}
	if len(restore) > 0 {
		// The reader goroutine must stay free to read the reply.
		go t.restoreContext(restore)
	}
	if completed {
		t.emit(&agent.ItemCompleted{EventMeta: meta, Item: it})
		if extra.async && len(extra.questions) > 0 {
			t.asyncQuestions(it, extra.questions, meta)
		}
	} else {
		t.emit(&agent.ItemStarted{EventMeta: meta, Item: it})
	}
}

// restoreContext injects the last additional context again after
// compaction dropped it: application entries as developer messages,
// untrusted entries as user messages in <external_key> tags.
func (t *translator) restoreContext(entries map[string]any) {
	keys := slices.Sorted(maps.Keys(entries))
	items := make([]json.RawMessage, 0, len(keys))
	for _, key := range keys {
		var entry struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		}
		data, _ := json.Marshal(entries[key])
		if json.Unmarshal(data, &entry) != nil {
			continue
		}
		role, text := "developer", "<"+key+">"+entry.Value+"</"+key+">"
		if entry.Kind == "untrusted" {
			role, text = "user", "<external_"+key+">"+entry.Value+"</external_"+key+">"
		}
		items = append(items, providerutil.Raw(map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": "input_text", "text": text}}}))
	}
	if len(items) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(t.s.rt.Context(), 10*time.Second)
	defer cancel()
	if err := t.s.InjectItems(ctx, items); err != nil {
		t.s.rt.Logger().Warn("restoring additional context after compaction failed", "error", err)
	}
}

// asyncQuestions raises the questions of an asynchronous agent message.
// The answer is sent as the next user message.
func (t *translator) asyncQuestions(it agent.Item, qs []asyncQuestion, meta agent.EventMeta) {
	questions := make([]agent.Question, 0, len(qs))
	for i, q := range qs {
		opts := make([]agent.QuestionOption, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, agent.QuestionOption{Label: o})
		}
		questions = append(questions, agent.Question{ID: strconv.Itoa(i), Header: "Question", Text: q.Title, Options: opts, AllowOther: true})
	}
	req := agent.UserInputRequest{EventMeta: meta, ID: "async:" + it.ID, Questions: questions, Async: true}
	t.s.mu.Lock()
	t.asyncTurns[req.ID] = it.TurnID
	t.s.mu.Unlock()
	t.s.rt.Request(providerkit.NewUserInputRequest(req, func(ctx context.Context, resp agent.UserInputResponse) error {
		t.s.mu.Lock()
		delete(t.asyncTurns, req.ID)
		t.s.mu.Unlock()
		if resp.Cancelled {
			return nil
		}
		var lines []string
		for i, q := range qs {
			answer := strings.Join(resp.Answers[strconv.Itoa(i)], ", ")
			if answer == "" {
				continue
			}
			if len(qs) == 1 {
				lines = append(lines, answer)
			} else {
				lines = append(lines, q.Title+"\n"+answer)
			}
		}
		if len(lines) == 0 {
			return nil
		}
		return t.s.answerAsync(ctx, strings.Join(lines, "\n\n"))
	}))
}

// withdrawAsync withdraws the pending async questions whose turn matches.
func (t *translator) withdrawAsync(match func(turnID string) bool) {
	t.s.mu.Lock()
	var ids []string
	for id, turn := range t.asyncTurns {
		if match(turn) {
			ids = append(ids, id)
			delete(t.asyncTurns, id)
		}
	}
	t.s.mu.Unlock()
	for _, id := range ids {
		t.s.rt.WithdrawRequest(id)
	}
}

// noteItemLocked records that the item id was reported for turn, a turn
// in turnOrder. The caller holds s.mu.
func (t *translator) noteItemLocked(turn, id string) {
	if turn == "" || id == "" || !slices.Contains(t.turnOrder, turn) {
		return
	}
	if ids := t.turnItemIDs[turn]; !slices.Contains(ids, id) {
		t.turnItemIDs[turn] = append(ids, id)
	}
}

// rewound forgets turnID and later turns after a rewind, withdraws their
// pending async questions, and reports their items removed. An unknown
// turnID came before every turn this session saw, so all of them go.
func (t *translator) rewound(turnID string) {
	t.s.mu.Lock()
	idx := max(slices.Index(t.turnOrder, turnID), 0)
	removed := map[string]bool{}
	var ids []string
	for _, id := range t.turnOrder[idx:] {
		removed[id] = true
		ids = append(ids, t.turnItemIDs[id]...)
		delete(t.turnItemIDs, id)
	}
	t.turnOrder = t.turnOrder[:idx]
	t.s.mu.Unlock()
	known := removed[turnID]
	t.withdrawAsync(func(turn string) bool { return !known || removed[turn] })
	if len(ids) > 0 {
		t.emit(&agent.ItemsRemoved{IDs: ids})
	}
}

// childNotify maps a subagent thread's notifications into the parent's
// stream: its items nest under the spawning item and carry the child
// thread as SubagentID, and its turns update the subagent's task.
func (t *translator) childNotify(msg *jsonrpc.Message, p turnParams, parent string, meta agent.EventMeta) {
	meta.TurnID = t.s.rt.CurrentTurnID()
	child := p.ThreadID
	switch msg.Method {
	case "item/started", "item/completed":
		it, extra := convertItem(p.Item)
		it.ParentID = parent
		it.SubagentID = child
		it.TurnID = p.TurnID
		started := msg.Method == "item/started"
		if started {
			it.Status = agent.ItemRunning
		}
		t.trackChildItem(child, it, started, meta.TurnID)
		for _, grandchild := range extra.children {
			t.addChild(grandchild, it.ID)
		}
		if !started {
			t.childItemDone(child, it, extra, meta)
			t.subagentItem(p.Item, child, meta)
			t.emit(&agent.ItemCompleted{EventMeta: meta, Item: it})
		} else {
			t.emit(&agent.ItemStarted{EventMeta: meta, Item: it})
		}
	case "item/agentMessage/delta":
		t.emit(&agent.TextDelta{EventMeta: meta, ItemID: p.ItemID, ParentID: parent, SubagentID: child, Delta: p.Delta})
	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		t.emit(&agent.ReasoningDelta{EventMeta: meta, ItemID: p.ItemID, ParentID: parent, SubagentID: child, Delta: p.Delta, Summary: msg.Method == "item/reasoning/summaryTextDelta"})
	case "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
		t.emit(&agent.ToolOutputDelta{EventMeta: meta, ItemID: p.ItemID, Delta: p.Delta})
	case "turn/started":
		t.updateTask(child, meta, func(task *agent.Task) {
			task.Status = agent.TaskRunning
			t.childTurns[child] = p.Turn.ID
		})
		t.stopLateChildTurn(child, p.Turn.ID, providerutil.Deref(p.Turn.RootTurnID))
	case "thread/settings/updated":
		var f struct {
			Settings struct {
				Model  *string `json:"model"`
				Effort *string `json:"effort"`
			} `json:"threadSettings"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.updateTask(child, meta, func(task *agent.Task) {
			if f.Settings.Model != nil {
				task.Subagent.Model = *f.Settings.Model
			}
			if f.Settings.Effort != nil {
				task.Subagent.Effort = agent.Effort(*f.Settings.Effort)
			}
		})
	case "model/rerouted":
		var f struct {
			To string `json:"toModel"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		t.updateTask(child, meta, func(task *agent.Task) { task.Subagent.Model = f.To })
	case "turn/completed":
		switch p.Turn.Status {
		case "failed":
			t.endChildItems(child, p.Turn.ID, agent.ItemFailed, meta)
		case "interrupted":
			t.endChildItems(child, p.Turn.ID, agent.ItemCancelled, meta)
		}
		t.updateTask(child, meta, func(task *agent.Task) {
			delete(t.childTurns, child)
			switch p.Turn.Status {
			case "failed":
				task.Status = agent.TaskFailed
				if p.Turn.Error != nil {
					task.Error = p.Turn.Error.Message
				}
			case "interrupted":
				task.Status = agent.TaskStopped
			default:
				task.Status = agent.TaskCompleted
			}
			if p.Turn.DurationMS != nil {
				task.Duration = time.Duration(*p.Turn.DurationMS) * time.Millisecond
			}
		})
	case "thread/tokenUsage/updated":
		var f struct {
			TokenUsage struct {
				Total tokenUsage `json:"total"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(msg.Params, &f) == nil {
			t.updateTask(child, meta, func(task *agent.Task) {
				u := f.TokenUsage.Total.usage()
				task.Usage = &u
			})
		}
	default:
		t.emit(&agent.RawEvent{EventMeta: meta, Type: "subagent/" + msg.Method})
	}
}

// stopLateChildTurn interrupts a subagent turn that started after its
// parent turn was interrupted or failed, or while an interrupt of it is in
// flight.
func (t *translator) stopLateChildTurn(child, turn, root string) {
	interrupting, pending := t.s.rt.Interrupting()
	if pending {
		interrupting = ""
	}
	t.s.mu.Lock()
	stop := (root != "" && slices.Contains(t.stopped, root)) ||
		(interrupting != "" && (root == "" || root == interrupting))
	t.s.mu.Unlock()
	if !stop {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(t.s.rt.Context(), 10*time.Second)
		defer cancel()
		if err := t.s.client.Call(ctx, "turn/interrupt", map[string]any{"threadId": child, "turnId": turn}, nil); err != nil {
			t.s.rt.Logger().Debug("interrupting late subagent turn failed", "thread", child, "error", err)
		}
	}()
}

// trackChildItem records a child thread's item as running when started
// and forgets it when completed. mainTurn is the main-thread turn the item
// is reported under.
func (t *translator) trackChildItem(thread string, it agent.Item, started bool, mainTurn string) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	t.noteItemLocked(mainTurn, it.ID)
	items := t.childRunning[thread]
	idx := slices.IndexFunc(items, func(r agent.Item) bool { return r.ID == it.ID })
	switch {
	case started && idx >= 0:
		items[idx] = it
	case started:
		t.childRunning[thread] = append(items, it)
	case idx >= 0:
		items = slices.Delete(items, idx, idx+1)
		if len(items) == 0 {
			delete(t.childRunning, thread)
		} else {
			t.childRunning[thread] = items
		}
	}
}

// endChildItems completes the items of a child thread's stopped turn that
// Codex left running, as endRunningItems does for the main thread.
func (t *translator) endChildItems(thread, turn string, status agent.ItemStatus, meta agent.EventMeta) {
	var ended []agent.Item
	t.s.mu.Lock()
	kept := slices.DeleteFunc(t.childRunning[thread], func(it agent.Item) bool {
		if turn != "" && it.TurnID != "" && it.TurnID != turn {
			return false
		}
		if it.Tool != nil {
			call := *it.Tool
			call.IsError = status == agent.ItemFailed
			it.Tool = &call
		}
		it.Status = status
		ended = append(ended, it)
		return true
	})
	if len(kept) == 0 {
		delete(t.childRunning, thread)
	} else {
		t.childRunning[thread] = kept
	}
	t.s.mu.Unlock()
	for _, it := range ended {
		t.emit(&agent.ItemCompleted{EventMeta: meta, Item: it})
	}
}

// addChild routes a subagent thread's messages to this session. spawn is
// the item that started it, or "" when only the turn chain links them.
func (t *translator) addChild(thread, spawn string) {
	t.s.mu.Lock()
	old, ok := t.children[thread]
	if !ok {
		t.trackChildLocked(thread)
	}
	if !ok || (old == "" && spawn != "") {
		t.children[thread] = spawn
	}
	t.s.mu.Unlock()
	t.s.client.registerChild(thread, t.s)
	t.pruneChildren(thread)
}

// trackChildLocked adds a newly known child thread to childOrder. The
// caller holds s.mu.
func (t *translator) trackChildLocked(thread string) {
	if _, ok := t.children[thread]; ok {
		return
	}
	if _, ok := t.tasks[thread]; ok {
		return
	}
	t.childOrder = append(t.childOrder, thread)
}

// pruneChildren forgets the oldest child threads that are not running
// while more than maxChildren are known, and unregisters them from the
// client: their later messages are held like those of any unknown thread
// until a turn chain or an item names them again. keep is never
// forgotten.
func (t *translator) pruneChildren(keep string) {
	var forgotten []string
	t.s.mu.Lock()
	for i := 0; len(t.childOrder) > maxChildren && i < len(t.childOrder); {
		thread := t.childOrder[i]
		task := t.tasks[thread]
		if thread == keep || t.childTurns[thread] != "" || len(t.childRunning[thread]) > 0 || (task != nil && task.Status == agent.TaskRunning) {
			i++
			continue
		}
		t.childOrder = slices.Delete(t.childOrder, i, i+1)
		delete(t.children, thread)
		delete(t.tasks, thread)
		delete(t.childTurns, thread)
		delete(t.childRunning, thread)
		forgotten = append(forgotten, thread)
	}
	t.s.mu.Unlock()
	for _, thread := range forgotten {
		t.s.client.unregisterChild(thread, t.s)
	}
}

// newSubagentTask returns the running task of a subagent thread.
func newSubagentTask(thread string) *agent.Task {
	return &agent.Task{ID: thread, Kind: agent.TaskSubagent, NativeKind: "thread", Status: agent.TaskRunning, Subagent: &agent.Subagent{}}
}

// updateTask changes a subagent's task and emits its new state.
func (t *translator) updateTask(thread string, meta agent.EventMeta, change func(*agent.Task)) {
	t.s.mu.Lock()
	task := t.tasks[thread]
	if task == nil {
		t.trackChildLocked(thread)
		task = newSubagentTask(thread)
		task.ToolCallID = t.children[thread]
		t.tasks[thread] = task
	}
	change(task)
	snapshot := task.Clone()
	t.s.mu.Unlock()
	t.emit(&agent.TaskUpdated{EventMeta: meta, Task: snapshot})
	if snapshot.Status != agent.TaskRunning {
		t.pruneChildren("")
	}
}

// childItemDone records what a finished child item says about its task.
func (t *translator) childItemDone(thread string, it agent.Item, extra itemExtra, meta agent.EventMeta) {
	switch {
	case it.Kind == agent.ItemAssistantMessage && (extra.phase == "final_answer" || extra.phase == ""):
		t.updateTask(thread, meta, func(task *agent.Task) { task.Output = it.Text })
	case it.Kind == agent.ItemToolCall:
		t.updateTask(thread, meta, func(task *agent.Task) {
			task.ToolUses++
			task.LastTool = it.Tool.Name
		})
	}
}

// subagentItem updates tasks from a subAgentActivity or collabAgentToolCall
// item. sender is the thread that produced the item: "" for the main
// thread, or a subagent thread that started nested subagents.
func (t *translator) subagentItem(raw json.RawMessage, sender string, meta agent.EventMeta) {
	main := t.s.ID()
	depth := 0
	if sender != "" {
		t.s.mu.Lock()
		depth = 1
		if task := t.tasks[sender]; task != nil && task.Subagent != nil && task.Subagent.Depth > 0 {
			depth = task.Subagent.Depth
		}
		t.s.mu.Unlock()
	}
	for _, c := range subagentChanges(raw, sender, depth, func(thread string) bool { return thread == main || thread == sender }) {
		t.updateTask(c.thread, meta, c.apply)
	}
}

// taskChange is an update of one subagent's task.
type taskChange struct {
	thread string
	apply  func(*agent.Task)
}

// subagentChanges returns the task updates a subAgentActivity or
// collabAgentToolCall item implies. sender is the thread that produced the
// item ("" for the main thread) and senderDepth its Subagent.Depth (0 for
// the main thread); skip excludes threads that are not subagents of it.
func subagentChanges(raw json.RawMessage, sender string, senderDepth int, skip func(string) bool) []taskChange {
	var ti threadItem
	if json.Unmarshal(raw, &ti) != nil {
		return nil
	}
	spawned := func(task *agent.Task) {
		if task.Subagent.Depth == 0 {
			task.Subagent.Depth = senderDepth + 1
		}
		if task.ParentID == "" {
			task.ParentID = sender
		}
	}
	var out []taskChange
	switch ti.Type {
	case "subAgentActivity":
		if ti.AgentThreadID == "" || skip(ti.AgentThreadID) {
			return nil
		}
		out = append(out, taskChange{ti.AgentThreadID, func(task *agent.Task) {
			sa := task.Subagent
			if ti.AgentPath != "" {
				task.Description = ti.AgentPath
				sa.Type = ti.AgentPath[strings.LastIndex(ti.AgentPath, "/")+1:]
			}
			if ti.Model != nil {
				sa.Model = *ti.Model
			}
			if ti.ReasoningEffort != nil {
				sa.Effort = agent.Effort(*ti.ReasoningEffort)
			}
			switch ti.Kind {
			case "started":
				task.ToolCallID = ti.ID
				task.Status = agent.TaskRunning
				spawned(task)
			case "interrupted":
				task.Status = agent.TaskStopped
			case "completed":
				if task.Status == agent.TaskRunning {
					task.Status = agent.TaskCompleted
				}
			}
		}})
	case "collabAgentToolCall":
		var f struct {
			AgentsStates map[string]struct {
				Status  string  `json:"status"`
				Message *string `json:"message"`
			} `json:"agentsStates"`
		}
		_ = json.Unmarshal(raw, &f)
		for _, thread := range ti.ReceiverThreadIDs {
			if skip(thread) {
				continue
			}
			state := f.AgentsStates[thread]
			out = append(out, taskChange{thread, func(task *agent.Task) {
				if ti.Tool == "spawnAgent" {
					task.ToolCallID = ti.ID
					if ti.Prompt != nil {
						task.Subagent.Prompt = *ti.Prompt
					}
					if ti.Model != nil {
						task.Subagent.Model = *ti.Model
					}
					if ti.ReasoningEffort != nil {
						task.Subagent.Effort = agent.Effort(*ti.ReasoningEffort)
					}
					spawned(task)
				}
				switch state.Status {
				case "running", "pendingInit":
					task.Status = agent.TaskRunning
				case "completed":
					task.Status = agent.TaskCompleted
				case "errored", "notFound":
					task.Status = agent.TaskFailed
				case "interrupted", "shutdown":
					task.Status = agent.TaskStopped
				}
				if state.Message != nil && *state.Message != "" {
					if task.Status == agent.TaskFailed {
						task.Error = *state.Message
					} else {
						task.Output = *state.Message
					}
				}
			}})
		}
	}
	return out
}

type itemExtra struct {
	phase    string
	children []string
	// async marks an agent message with questions answered by a later
	// user message.
	async     bool
	questions []asyncQuestion
}

type asyncQuestion struct {
	Title   string   `json:"title"`
	Options []string `json:"options"`
}

type threadItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Text             string          `json:"text"`
	Phase            *string         `json:"phase"`
	Content          json.RawMessage `json:"content"`
	Summary          []string        `json:"summary"`
	Command          string          `json:"command"`
	Cwd              string          `json:"cwd"`
	Status           string          `json:"status"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	DurationMS       *int64          `json:"durationMs"`
	Changes          []struct {
		Path string `json:"path"`
		Kind struct {
			Type     string  `json:"type"`
			MovePath *string `json:"move_path"`
		} `json:"kind"`
		Diff string `json:"diff"`
	} `json:"changes"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Namespace *string         `json:"namespace"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
	ContentItems []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"imageUrl"`
	} `json:"contentItems"`
	Success           *bool           `json:"success"`
	ReceiverThreadIDs []string        `json:"receiverThreadIds"`
	Prompt            *string         `json:"prompt"`
	AgentThreadID     string          `json:"agentThreadId"`
	AgentPath         string          `json:"agentPath"`
	Kind              string          `json:"kind"`
	Model             *string         `json:"model"`
	ReasoningEffort   *string         `json:"reasoningEffort"`
	Query             string          `json:"query"`
	Path              string          `json:"path"`
	Review            string          `json:"review"`
	Delivery          *string         `json:"delivery"`
	Questions         []asyncQuestion `json:"questions"`
	ProcessID         *string         `json:"processId"`
	ClientID          *string         `json:"clientId"`
	SavedPath         *string         `json:"savedPath"`
	RevisedPrompt     *string         `json:"revisedPrompt"`
	Failure           json.RawMessage `json:"failure"`
	Action            *struct {
		Type    string   `json:"type"`
		Query   *string  `json:"query"`
		Queries []string `json:"queries"`
		URL     *string  `json:"url"`
		Pattern *string  `json:"pattern"`
	} `json:"action"`
	AppContext *struct {
		ActionName *string `json:"actionName"`
		AppName    *string `json:"appName"`
	} `json:"appContext"`
}

func itemStatus(status string) agent.ItemStatus {
	switch status {
	case "inProgress", "":
		return agent.ItemRunning
	case "failed":
		return agent.ItemFailed
	case "declined":
		return agent.ItemDeclined
	case "interrupted":
		return agent.ItemCancelled
	default:
		return agent.ItemDone
	}
}

// convertItem maps a Codex ThreadItem to a uniform item.
func convertItem(raw json.RawMessage) (agent.Item, itemExtra) {
	var ti threadItem
	_ = json.Unmarshal(raw, &ti)
	it := agent.Item{ID: ti.ID, Status: agent.ItemDone, Raw: raw}
	var extra itemExtra
	switch ti.Type {
	case "userMessage":
		it.Kind = agent.ItemUserMessage
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ti.Content, &parts)
		var texts []string
		for _, p := range parts {
			if p.Type == "text" {
				texts = append(texts, p.Text)
			}
		}
		it.Text = strings.Join(texts, "\n")
		it.ClientID = providerutil.Deref(ti.ClientID)
	case "agentMessage":
		it.Kind = agent.ItemAssistantMessage
		it.Text = ti.Text
		if ti.Phase != nil {
			extra.phase = *ti.Phase
		}
		extra.async = providerutil.Deref(ti.Delivery) == "async"
		extra.questions = ti.Questions
	case "reasoning":
		it.Kind = agent.ItemReasoning
		var content []string
		_ = json.Unmarshal(ti.Content, &content)
		it.Text = strings.Join(append(append([]string(nil), ti.Summary...), content...), "\n\n")
	case "plan":
		it.Kind = agent.ItemPlan
		it.Text = ti.Text
	case "contextCompaction":
		it.Kind = agent.ItemCompaction
	case "commandExecution":
		it.Kind = agent.ItemToolCall
		it.Status = itemStatus(ti.Status)
		call := &agent.ToolCall{Name: "commandExecution", Kind: agent.ToolShell, Command: ti.Command, Cwd: ti.Cwd, ExitCode: ti.ExitCode}
		if ti.AggregatedOutput != nil {
			call.Output = *ti.AggregatedOutput
		}
		if ti.DurationMS != nil {
			call.Duration = time.Duration(*ti.DurationMS) * time.Millisecond
		}
		call.IsError = it.Status == agent.ItemFailed || (ti.ExitCode != nil && *ti.ExitCode != 0)
		input := map[string]any{"command": ti.Command, "cwd": ti.Cwd}
		if ti.ProcessID != nil {
			input["processId"] = *ti.ProcessID
		}
		call.Input = providerutil.Raw(input)
		it.Tool = call
	case "fileChange":
		it.Kind = agent.ItemToolCall
		it.Status = itemStatus(ti.Status)
		call := &agent.ToolCall{Name: "fileChange", Kind: agent.ToolEdit, IsError: it.Status == agent.ItemFailed}
		for _, c := range ti.Changes {
			fc := agent.FileChange{Path: c.Path, Diff: c.Diff, Kind: agent.FileModified}
			switch c.Kind.Type {
			case "add":
				fc.Kind = agent.FileAdded
			case "delete":
				fc.Kind = agent.FileDeleted
			}
			if c.Kind.MovePath != nil {
				fc.MovePath = *c.Kind.MovePath
			}
			call.Changes = append(call.Changes, fc)
		}
		if len(call.Changes) > 0 {
			call.Path = call.Changes[0].Path
		}
		it.Tool = call
	case "mcpToolCall":
		it.Kind = agent.ItemToolCall
		it.Status = itemStatus(ti.Status)
		call := &agent.ToolCall{Name: "mcp__" + ti.Server + "__" + ti.Tool, Kind: agent.ToolMCP, Server: ti.Server, Input: ti.Arguments}
		if ti.AppContext != nil {
			call.DisplayName = providerutil.Deref(ti.AppContext.ActionName)
			call.ServerDisplayName = providerutil.Deref(ti.AppContext.AppName)
		}
		if ti.DurationMS != nil {
			call.Duration = time.Duration(*ti.DurationMS) * time.Millisecond
		}
		if ti.Error != nil {
			call.IsError = true
			call.Output = ti.Error.Message
		} else if nonNull(ti.Result) != nil {
			var r struct {
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					Data     string `json:"data"`
					MimeType string `json:"mimeType"`
				} `json:"content"`
				Structured json.RawMessage `json:"structuredContent"`
			}
			_ = json.Unmarshal(ti.Result, &r)
			var texts []string
			for _, c := range r.Content {
				switch c.Type {
				case "text":
					texts = append(texts, c.Text)
				case "image":
					if data, err := base64.StdEncoding.DecodeString(c.Data); err == nil {
						call.Images = append(call.Images, agent.Image{MediaType: c.MimeType, Data: data})
					}
				}
			}
			call.Output = strings.Join(texts, "\n")
			if nonNull(r.Structured) != nil {
				call.OutputData = r.Structured
			}
		}
		it.Tool = call
	case "dynamicToolCall":
		it.Kind = agent.ItemToolCall
		it.Status = itemStatus(ti.Status)
		name := ti.Tool
		if ns := strings.TrimSpace(providerutil.Deref(ti.Namespace)); ns != "" {
			name = ns + "." + ti.Tool
		}
		call := &agent.ToolCall{Name: name, Kind: agent.ToolHost, Input: ti.Arguments}
		if ti.DurationMS != nil {
			call.Duration = time.Duration(*ti.DurationMS) * time.Millisecond
		}
		var texts []string
		for _, c := range ti.ContentItems {
			if c.Type == "inputText" {
				texts = append(texts, c.Text)
			}
		}
		call.Output = strings.Join(texts, "\n")
		call.IsError = ti.Success != nil && !*ti.Success
		it.Tool = call
	case "collabAgentToolCall":
		it.Kind = agent.ItemToolCall
		it.Status = itemStatus(ti.Status)
		input := map[string]any{"tool": ti.Tool, "receiverThreadIds": ti.ReceiverThreadIDs}
		if ti.Prompt != nil {
			input["prompt"] = *ti.Prompt
		}
		it.Tool = &agent.ToolCall{Name: ti.Tool, Kind: agent.ToolSubagent, Input: providerutil.Raw(input)}
		extra.children = ti.ReceiverThreadIDs
	case "subAgentActivity":
		it.Kind = agent.ItemToolCall
		input := map[string]any{"kind": ti.Kind, "agentThreadId": ti.AgentThreadID, "agentPath": ti.AgentPath}
		if ti.Model != nil {
			input["model"] = *ti.Model
		}
		it.Tool = &agent.ToolCall{Name: "subagent", Kind: agent.ToolSubagent, Input: providerutil.Raw(input), Output: ti.Kind}
		if ti.AgentThreadID != "" {
			extra.children = []string{ti.AgentThreadID}
		}
	case "webSearch":
		it.Kind = agent.ItemToolCall
		call := &agent.ToolCall{Name: "webSearch", Kind: agent.ToolWeb, Query: ti.Query}
		input := map[string]any{"query": ti.Query}
		if a := ti.Action; a != nil {
			input["action"] = a.Type
			switch a.Type {
			case "search":
				call.Query = cmp.Or(ti.Query, providerutil.Deref(a.Query), strings.Join(a.Queries, "\n"))
			case "openPage":
				call.URL = providerutil.Deref(a.URL)
			case "findInPage":
				call.Query = cmp.Or(providerutil.Deref(a.Pattern), ti.Query)
				call.URL = providerutil.Deref(a.URL)
			}
			if call.URL != "" {
				input["url"] = call.URL
			}
			input["query"] = call.Query
		}
		call.Input = providerutil.Raw(input)
		it.Tool = call
	case "imageView":
		it.Kind = agent.ItemToolCall
		it.Tool = &agent.ToolCall{Name: "imageView", Kind: agent.ToolRead, Path: ti.Path, Input: providerutil.Raw(map[string]any{"path": ti.Path})}
	case "imageGeneration":
		it.Kind = agent.ItemToolCall
		call := &agent.ToolCall{Name: "imageGeneration", Kind: agent.ToolOther, Path: providerutil.Deref(ti.SavedPath)}
		input := map[string]any{}
		if ti.RevisedPrompt != nil {
			input["revisedPrompt"] = *ti.RevisedPrompt
		}
		call.Input = providerutil.Raw(input)
		var result string
		_ = json.Unmarshal(ti.Result, &result)
		switch {
		case call.Path != "":
			call.Images = []agent.Image{{Path: call.Path}}
		case result != "":
			if data, err := base64.StdEncoding.DecodeString(result); err == nil {
				call.Images = []agent.Image{{MediaType: "image/png", Data: data}}
			}
		}
		it.Status = itemStatus(ti.Status)
		if f := nonNull(ti.Failure); f != nil || it.Status == agent.ItemFailed {
			it.Status = agent.ItemFailed
			call.IsError = true
			var failure struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(f, &failure)
			call.Output = cmp.Or(failure.Type, "image generation failed")
			call.OutputData = f
		}
		it.Tool = call
	case "enteredReviewMode", "exitedReviewMode":
		it.Kind = agent.ItemOther
		it.Text = ti.Review
	default:
		it.Kind = agent.ItemOther
	}
	return it, extra
}

// serverRequest answers or defers an app-server request for this thread.
func (t *translator) serverRequest(msg *jsonrpc.Message, line []byte) (any, error) {
	s := t.s
	meta := agent.EventMeta{Raw: line}
	var p turnParams
	_ = json.Unmarshal(msg.Params, &p)
	meta.TurnID = p.TurnID
	id := requestKey(msg.ID)
	reply := func(result any) error { return s.client.reply(msg.ID, result, nil) }
	askPermission := func(req agent.PermissionRequest, result func(agent.PermissionResponse) any) (any, error) {
		s.rt.Request(providerkit.NewPermissionRequest(req, func(ctx context.Context, resp agent.PermissionResponse) error {
			if err := providerkit.CheckUpdatedInput(Name, resp); err != nil {
				return err
			}
			return reply(result(resp))
		}))
		return nil, jsonrpc.ErrDeferred
	}
	// A subagent's thread routes its requests to the parent's session.
	var subagent string
	if th := threadOf(msg.Params); th != "" && th != s.ID() {
		subagent = th
	}

	switch msg.Method {
	case "item/tool/call":
		var f struct {
			CallID    string          `json:"callId"`
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		tool, ok := s.tools[f.Tool]
		if !ok {
			return map[string]any{"contentItems": []any{map[string]any{"type": "inputText", "text": "unknown tool " + f.Tool}}, "success": false}, nil
		}
		res := providerkit.CallTool(s.rt.Context(), tool, agent.ToolCallRequest{ID: f.CallID, Name: f.Tool, Arguments: f.Arguments, SessionID: s.ID()})
		return dynamicToolResult(res), nil

	case "item/commandExecution/requestApproval":
		var f struct {
			ItemID    string            `json:"itemId"`
			Reason    *string           `json:"reason"`
			Command   *string           `json:"command"`
			Cwd       *string           `json:"cwd"`
			Amendment json.RawMessage   `json:"proposedExecpolicyAmendment"`
			Available []json.RawMessage `json:"availableDecisions"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		req := agent.PermissionRequest{
			EventMeta:  meta,
			SubagentID: subagent,
			ID:         id,
			Kind:       agent.PermissionCommand,
			ToolName:   "commandExecution",
			ToolCallID: f.ItemID,
			Input:      msg.Params,
			Command:    providerutil.Deref(f.Command),
			Cwd:        providerutil.Deref(f.Cwd),
			Reason:     providerutil.Deref(f.Reason),
			Title:      "Run " + providerutil.Deref(f.Command),
			Decisions:  commandDecisions(f.Available),
		}
		return askPermission(req, func(resp agent.PermissionResponse) any {
			return map[string]any{"decision": commandDecision(resp, f.Available, f.Amendment)}
		})

	case "item/fileChange/requestApproval":
		var f struct {
			ItemID    string  `json:"itemId"`
			Reason    *string `json:"reason"`
			GrantRoot *string `json:"grantRoot"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		s.mu.Lock()
		item := t.items[f.ItemID]
		if subagent != "" {
			if idx := slices.IndexFunc(t.childRunning[subagent], func(it agent.Item) bool { return it.ID == f.ItemID }); idx >= 0 {
				item = t.childRunning[subagent][idx]
			}
		}
		s.mu.Unlock()
		req := agent.PermissionRequest{
			EventMeta:  meta,
			SubagentID: subagent,
			ID:         id,
			Kind:       agent.PermissionFileChange,
			ToolName:   "fileChange",
			ToolCallID: f.ItemID,
			Input:      msg.Params,
			Reason:     providerutil.Deref(f.Reason),
			Title:      "Apply file changes",
			Decisions:  []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionAllowSession, agent.DecisionDeny, agent.DecisionCancel},
		}
		if item.Tool != nil {
			req.Changes = item.Tool.Changes
		}
		if f.GrantRoot != nil {
			req.Paths = []string{*f.GrantRoot}
		}
		return askPermission(req, func(resp agent.PermissionResponse) any {
			if resp.ProviderResponse != nil {
				return map[string]any{"decision": resp.ProviderResponse}
			}
			decision := "accept"
			switch resp.Decision {
			case agent.DecisionAllowSession, agent.DecisionAllowAlways:
				decision = "acceptForSession"
			case agent.DecisionDeny:
				decision = "decline"
			case agent.DecisionCancel:
				decision = "cancel"
			}
			return map[string]any{"decision": decision}
		})

	case "item/permissions/requestApproval":
		var f struct {
			ItemID      string          `json:"itemId"`
			Cwd         string          `json:"cwd"`
			Reason      *string         `json:"reason"`
			Permissions json.RawMessage `json:"permissions"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		req := agent.PermissionRequest{
			EventMeta:  meta,
			SubagentID: subagent,
			ID:         id,
			Kind:       agent.PermissionGrant,
			ToolName:   "permissions",
			ToolCallID: f.ItemID,
			Input:      f.Permissions,
			Cwd:        f.Cwd,
			Reason:     providerutil.Deref(f.Reason),
			Title:      "Grant additional permissions",
			Decisions:  []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionAllowSession, agent.DecisionDeny},
		}
		return askPermission(req, func(resp agent.PermissionResponse) any {
			if resp.ProviderResponse != nil {
				return resp.ProviderResponse
			}
			switch resp.Decision {
			case agent.DecisionDeny, agent.DecisionCancel:
				return map[string]any{"permissions": map[string]any{}, "scope": "turn"}
			}
			scope := "turn"
			if resp.Decision == agent.DecisionAllowSession || resp.Decision == agent.DecisionAllowAlways {
				scope = "session"
			}
			granted := json.RawMessage(`{}`)
			if len(f.Permissions) > 0 {
				granted = grantedPermissions(f.Permissions)
			}
			return map[string]any{"permissions": granted, "scope": scope}
		})

	case "item/tool/requestUserInput":
		var f struct {
			ItemID    string `json:"itemId"`
			Questions []struct {
				ID       string `json:"id"`
				Header   string `json:"header"`
				Question string `json:"question"`
				IsOther  bool   `json:"isOther"`
				IsSecret bool   `json:"isSecret"`
				Options  []struct {
					Label       string `json:"label"`
					Description string `json:"description"`
				} `json:"options"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		questions := make([]agent.Question, 0, len(f.Questions))
		for _, q := range f.Questions {
			opts := make([]agent.QuestionOption, 0, len(q.Options))
			for _, o := range q.Options {
				opts = append(opts, agent.QuestionOption{Label: o.Label, Description: o.Description})
			}
			questions = append(questions, agent.Question{ID: q.ID, Header: q.Header, Text: q.Question, Options: opts, AllowOther: q.IsOther, Secret: q.IsSecret})
		}
		s.rt.Request(providerkit.NewUserInputRequest(agent.UserInputRequest{EventMeta: meta, ID: id, Questions: questions, ToolCallID: f.ItemID, SubagentID: subagent},
			func(ctx context.Context, resp agent.UserInputResponse) error {
				answers := map[string]any{}
				if !resp.Cancelled {
					for qid, vals := range resp.Answers {
						answers[qid] = map[string]any{"answers": vals}
					}
				}
				return reply(map[string]any{"answers": answers})
			}))
		return nil, jsonrpc.ErrDeferred

	case "mcpServer/elicitation/request":
		var f struct {
			ServerName      string          `json:"serverName"`
			Mode            string          `json:"mode"`
			Message         string          `json:"message"`
			Description     string          `json:"description"`
			URL             string          `json:"url"`
			ElicitationID   string          `json:"elicitationId"`
			RequestedSchema json.RawMessage `json:"requestedSchema"`
			Meta            json.RawMessage `json:"_meta"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		s.rt.Request(providerkit.NewElicitationRequest(agent.ElicitationRequest{
			EventMeta:     meta,
			ID:            id,
			Server:        f.ServerName,
			Message:       cmp.Or(f.Message, f.Description),
			Mode:          f.Mode,
			Schema:        f.RequestedSchema,
			URL:           f.URL,
			ElicitationID: f.ElicitationID,
			MCPMeta:       nonNull(f.Meta),
			SubagentID:    subagent,
		}, func(ctx context.Context, resp agent.ElicitationResponse) error {
			var content, meta any
			if len(resp.Content) > 0 {
				content = resp.Content
			}
			if len(resp.MCPMeta) > 0 {
				meta = resp.MCPMeta
			}
			return reply(map[string]any{"action": string(resp.Action), "content": content, "_meta": meta})
		}))
		return nil, jsonrpc.ErrDeferred

	case "applyPatchApproval", "execCommandApproval":
		var f struct {
			CallID      string          `json:"callId"`
			Command     []string        `json:"command"`
			Cwd         string          `json:"cwd"`
			Reason      *string         `json:"reason"`
			FileChanges json.RawMessage `json:"fileChanges"`
			GrantRoot   *string         `json:"grantRoot"`
		}
		_ = json.Unmarshal(msg.Params, &f)
		kind := agent.PermissionCommand
		if msg.Method == "applyPatchApproval" {
			kind = agent.PermissionFileChange
		}
		req := agent.PermissionRequest{
			EventMeta:  meta,
			SubagentID: subagent,
			ID:         id,
			Kind:       kind,
			ToolName:   msg.Method,
			ToolCallID: f.CallID,
			Input:      msg.Params,
			Command:    strings.Join(f.Command, " "),
			Cwd:        f.Cwd,
			Reason:     providerutil.Deref(f.Reason),
			Decisions:  []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionAllowSession, agent.DecisionDeny, agent.DecisionCancel},
		}
		if f.GrantRoot != nil {
			req.Paths = []string{*f.GrantRoot}
		}
		return askPermission(req, func(resp agent.PermissionResponse) any {
			if resp.ProviderResponse != nil {
				return map[string]any{"decision": resp.ProviderResponse}
			}
			var decision any = "approved"
			switch resp.Decision {
			case agent.DecisionAllowSession, agent.DecisionAllowAlways:
				decision = "approved_for_session"
			case agent.DecisionDeny:
				decision = map[string]any{"denied": map[string]any{"rejection": cmp.Or(resp.Message, "The user denied this action.")}}
			case agent.DecisionCancel:
				decision = "abort"
			}
			return map[string]any{"decision": decision}
		})
	}
	return nil, &jsonrpc.Error{Code: -32601, Message: "unsupported request: " + msg.Method}
}

// nonNull returns raw, or nil for an absent or null value.
func nonNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

// decisionName returns the name of a string or single-key object decision.
func decisionName(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		for k := range m {
			return k
		}
	}
	return ""
}

func commandDecisions(available []json.RawMessage) []agent.PermissionDecision {
	if len(available) == 0 {
		return []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionAllowSession, agent.DecisionDeny, agent.DecisionCancel}
	}
	var out []agent.PermissionDecision
	for _, raw := range available {
		switch decisionName(raw) {
		case "accept":
			out = append(out, agent.DecisionAllow)
		case "acceptForSession":
			out = append(out, agent.DecisionAllowSession)
		case "acceptWithExecpolicyAmendment":
			out = append(out, agent.DecisionAllowAlways)
		case "decline":
			out = append(out, agent.DecisionDeny)
		case "cancel":
			out = append(out, agent.DecisionCancel)
		}
	}
	return out
}

// commandDecision picks the native decision for a uniform answer, falling
// back to the closest decision the server offered.
func commandDecision(resp agent.PermissionResponse, available []json.RawMessage, amendment json.RawMessage) any {
	if resp.ProviderResponse != nil {
		return resp.ProviderResponse
	}
	offered := func(name string) bool {
		return len(available) == 0 || slices.ContainsFunc(available, func(raw json.RawMessage) bool { return decisionName(raw) == name })
	}
	switch resp.Decision {
	case agent.DecisionAllowAlways:
		if offered("acceptWithExecpolicyAmendment") && nonNull(amendment) != nil {
			return map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": amendment}}
		}
		fallthrough
	case agent.DecisionAllowSession:
		if offered("acceptForSession") {
			return "acceptForSession"
		}
		return "accept"
	case agent.DecisionDeny:
		if offered("decline") {
			return "decline"
		}
		return "cancel"
	case agent.DecisionCancel:
		return "cancel"
	default:
		return "accept"
	}
}

// grantedPermissions grants everything that was requested, dropping null
// members that the response type does not accept.
func grantedPermissions(requested json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(requested, &m) != nil {
		return json.RawMessage(`{}`)
	}
	for k, v := range m {
		if string(v) == "null" {
			delete(m, k)
		}
	}
	data, _ := json.Marshal(m)
	return data
}

func dynamicToolResult(res agent.ToolResult) map[string]any {
	items := make([]map[string]any, 0, len(res.Content))
	for _, c := range res.Content {
		switch c.Type {
		case agent.ToolContentImage:
			items = append(items, map[string]any{"type": "inputImage", "imageUrl": "data:" + c.MediaType + ";base64," + base64.StdEncoding.EncodeToString(c.Data)})
		default:
			items = append(items, map[string]any{"type": "inputText", "text": c.Text})
		}
	}
	if len(items) == 0 {
		items = append(items, map[string]any{"type": "inputText", "text": ""})
	}
	return map[string]any{"contentItems": items, "success": !res.IsError}
}

type goalWire struct {
	Objective       string `json:"objective"`
	Status          string `json:"status"`
	TokenBudget     *int64 `json:"tokenBudget"`
	TokensUsed      int64  `json:"tokensUsed"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds"`
}

func (g goalWire) goal() *agent.Goal {
	out := &agent.Goal{Objective: g.Objective, Status: g.Status, TokensUsed: g.TokensUsed, TimeUsed: time.Duration(g.TimeUsedSeconds) * time.Second}
	if g.TokenBudget != nil {
		out.TokenBudget = *g.TokenBudget
	}
	return out
}

type rateLimitWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}

// mainLimitID is the limit ID of the account's main Codex allowance.
// Snapshots without a limit ID describe it too.
const mainLimitID = "codex"

type rateLimitSnapshot struct {
	LimitID     *string          `json:"limitId"`
	LimitName   *string          `json:"limitName"`
	Primary     *rateLimitWindow `json:"primary"`
	Secondary   *rateLimitWindow `json:"secondary"`
	ReachedType *string          `json:"rateLimitReachedType"`
}

func (r rateLimitSnapshot) id() string {
	return cmp.Or(providerutil.Deref(r.LimitID), mainLimitID)
}

// mergeRateLimits folds a partial rate-limit update into the snapshot of
// its limit: fields the update carries replace earlier values, and absent
// fields keep them.
func (t *translator) mergeRateLimits(update map[string]json.RawMessage) rateLimitSnapshot {
	var head rateLimitSnapshot
	_ = json.Unmarshal(update["limitId"], &head.LimitID)
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	merged := t.rateLimits[head.id()]
	if merged == nil {
		merged = map[string]json.RawMessage{}
		t.rateLimits[head.id()] = merged
	}
	for k, v := range update {
		merged[k] = mergeObject(merged[k], v)
	}
	return t.rateLimitSnapshotLocked(head.id())
}

// mergeObject returns update with the fields of old it lacks when both are
// JSON objects, such as a usage window updated with only its used share;
// otherwise update.
func mergeObject(old, update json.RawMessage) json.RawMessage {
	var o, u map[string]json.RawMessage
	if json.Unmarshal(old, &o) != nil || json.Unmarshal(update, &u) != nil || o == nil || u == nil {
		return update
	}
	maps.Copy(o, u)
	data, err := json.Marshal(o)
	if err != nil {
		return update
	}
	return data
}

func (t *translator) rateLimitSnapshotLocked(limitID string) rateLimitSnapshot {
	var snap rateLimitSnapshot
	data, _ := json.Marshal(t.rateLimits[limitID])
	_ = json.Unmarshal(data, &snap)
	if snap.LimitID == nil {
		snap.LimitID = &limitID
	}
	return snap
}

// resetsAt returns when every exhausted window has reset, or zero when no
// window is exhausted or a reset time is unknown.
func (r rateLimitSnapshot) resetsAt() time.Time {
	var latest time.Time
	for _, l := range r.limits() {
		if l.UsedPercent < 100 {
			continue
		}
		if l.ResetsAt.IsZero() {
			return time.Time{}
		}
		if l.ResetsAt.After(latest) {
			latest = l.ResetsAt
		}
	}
	return latest
}

// limits maps the primary and secondary windows. Codex reports no warning
// level, so a window is exceeded at 100% or when the server says a limit
// was reached.
func (r rateLimitSnapshot) limits() []agent.RateLimit {
	var out []agent.RateLimit
	add := func(name string, w *rateLimitWindow) {
		if w == nil {
			return
		}
		l := agent.RateLimit{Name: name, UsedPercent: w.UsedPercent, Status: agent.RateLimitOK, LimitID: r.id()}
		if w.WindowDurationMins != nil {
			l.Window = time.Duration(*w.WindowDurationMins) * time.Minute
		}
		if w.ResetsAt != nil {
			l.ResetsAt = time.Unix(*w.ResetsAt, 0)
		}
		if w.UsedPercent >= 100 {
			l.Status = agent.RateLimitExceeded
		}
		out = append(out, l)
	}
	add("primary", r.Primary)
	add("secondary", r.Secondary)
	if r.ReachedType != nil && len(out) > 0 {
		out[0].Status = agent.RateLimitExceeded
	}
	return out
}
