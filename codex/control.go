package codex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Thread operations. Each wraps one app-server method; the raw result is
// returned where its shape is large or still evolving.

// SetTitle sets the thread name.
func (s *Session) SetTitle(ctx context.Context, name string) error {
	if err := s.threadCall(ctx, "thread/name/set", map[string]any{"threadId": s.ID(), "name": name}, nil); err != nil {
		return err
	}
	s.updateInfo(agent.EventMeta{}, func(i *agent.SessionInfo) { i.Title = name })
	return nil
}

// Compact compacts the thread's context and waits until the compaction
// ends; progress also arrives as Compaction events. Codex takes no summary
// instructions, so instructions is ignored.
func (s *Session) Compact(ctx context.Context, instructions string) error {
	done := make(chan struct{})
	s.mu.Lock()
	s.endCompactionLocked()
	s.compacting = true
	s.compacted = done
	s.mu.Unlock()
	if err := s.threadCall(ctx, "thread/compact/start", map[string]any{"threadId": s.ID()}, nil); err != nil {
		s.mu.Lock()
		if s.compacted == done {
			s.endCompactionLocked()
		}
		s.mu.Unlock()
		return err
	}
	select {
	case <-done:
		return nil
	case <-s.rt.Done():
		return s.rt.ClosedErr()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ForkSession copies the thread into a new session on the same app-server.
// Empty fields of cfg are taken from this session.
func (s *Session) ForkSession(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	return s.Fork(ctx, cfg)
}

// inheritConfig fills the empty fields of cfg from this session, because
// Codex does not carry a thread's MCP servers, prompts, or model into a
// fork. Resume, Fork, ForkAtTurn, SessionID, and Title are not inherited.
func (s *Session) inheritConfig(cfg agent.Config) agent.Config {
	return providerkit.InheritConfig(cfg, s.cfg, s.Info())
}

// StopTask interrupts a subagent's running turn. taskID is the subagent's
// thread ID, as reported in Task.ID. A subagent that is not running is not
// an error; an unknown task ID is.
func (s *Session) StopTask(ctx context.Context, taskID string) error {
	s.mu.Lock()
	_, known := s.tr.children[taskID]
	turn := s.tr.childTurns[taskID]
	s.mu.Unlock()
	if !known && turn == "" {
		return fmt.Errorf("codex: unknown task %s", taskID)
	}
	if turn == "" {
		return nil // the subagent is not running
	}
	return s.threadCall(ctx, "turn/interrupt", map[string]any{"threadId": taskID, "turnId": turn}, nil)
}

// ReviewTarget selects what a review covers. Use the constructors.
type ReviewTarget map[string]any

// ReviewUncommitted reviews uncommitted changes.
func ReviewUncommitted() ReviewTarget { return ReviewTarget{"type": "uncommittedChanges"} }

// ReviewBaseBranch reviews changes against a base branch.
func ReviewBaseBranch(branch string) ReviewTarget {
	return ReviewTarget{"type": "baseBranch", "branch": branch}
}

// ReviewCommit reviews one commit.
func ReviewCommit(sha, title string) ReviewTarget {
	t := ReviewTarget{"type": "commit", "sha": sha, "title": nil}
	if title != "" {
		t["title"] = title
	}
	return t
}

// ReviewCustom reviews with custom instructions.
func ReviewCustom(instructions string) ReviewTarget {
	return ReviewTarget{"type": "custom", "instructions": instructions}
}

// Review starts an inline code review turn on this thread. The returned
// turn completes when the review finishes. It fails while a turn runs or
// input is queued. Detached reviews, which run on a
// separate thread, are not supported: open that thread as its own session
// with Client.Call("review/start", ...) and OpenSession instead.
func (s *Session) Review(ctx context.Context, target ReviewTarget) (*agent.Turn, error) {
	params := map[string]any{"threadId": s.ID(), "target": target, "delivery": "inline"}
	id := provisionalID()
	turn, ok, err := s.rt.StartIdle(ctx, id, func(ctx context.Context) error {
		var resp struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
			ReviewThreadID string `json:"reviewThreadId"`
		}
		// Like turn/start, the call is not abandoned once sent: Codex may
		// already run the review, and a later start could be mistaken for it.
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startTimeout)
		defer cancel()
		if err := s.threadCall(callCtx, "review/start", params, &resp); err != nil {
			return err
		}
		s.accepted(id, resp.Turn.ID, nil)
		return nil
	})
	if !ok {
		return nil, errors.New("codex: cannot start a review while a turn runs or input is queued")
	}
	return turn, err
}

// Rewind removes turnID and every later turn from the thread history.
// File changes are not undone. A paginated thread is reverted in place
// with thread/revert and keeps its ID. A legacy thread, or one whose
// revert fails, is forked before turnID with this session's settings, and
// the session continues on the fork: its ID changes, which a
// SessionUpdated event reports, and the original thread stays unchanged.
// Pending asynchronous questions from the removed turns are withdrawn, and
// an agent.ItemsRemoved event lists the items this session reported for
// them; items from before a resume are not listed.
func (s *Session) Rewind(ctx context.Context, turnID string) error {
	if !s.rt.Idle() {
		return errors.New("codex: cannot rewind while a turn runs")
	}
	mode, readErr := s.historyMode(ctx)
	var revertErr error
	if readErr != nil || mode == "paginated" {
		revertErr = s.threadCall(ctx, "thread/revert", map[string]any{"threadId": s.ID(), "beforeTurnId": turnID}, nil)
		if revertErr == nil {
			s.tr.rewound(turnID)
			return nil
		}
	}
	if err := s.forkBefore(ctx, turnID); err != nil {
		if revertErr != nil {
			return fmt.Errorf("codex: rewind: thread/revert: %v; thread/fork: %w", revertErr, err)
		}
		return fmt.Errorf("codex: rewind: %w", err)
	}
	s.tr.rewound(turnID)
	return nil
}

// historyMode returns the thread's persisted history contract,
// "paginated" or "legacy".
func (s *Session) historyMode(ctx context.Context) (string, error) {
	var resp struct {
		Thread struct {
			HistoryMode string `json:"historyMode"`
		} `json:"thread"`
	}
	if err := s.threadCall(ctx, "thread/read", map[string]any{"threadId": s.ID(), "includeTurns": false}, &resp); err != nil {
		return "", err
	}
	return cmp.Or(resp.Thread.HistoryMode, "legacy"), nil
}

// forkBefore forks the thread before turnID and moves the session to the
// fork.
func (s *Session) forkBefore(ctx context.Context, turnID string) error {
	old := s.ID()
	cfg := s.inheritConfig(agent.Config{})
	cfg.Resume, cfg.Fork = old, true
	params := s.threadParams(cfg)
	params["threadId"] = old
	params["beforeTurnId"] = turnID
	params["excludeTurns"] = true
	raw, err := s.client.CallRaw(ctx, "thread/fork", params)
	if err != nil {
		return err
	}
	var resp threadResponse
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Thread.ID == "" {
		return fmt.Errorf("decode thread/fork: %v", err)
	}
	id := resp.Thread.ID
	s.setThreadID(id)
	s.rememberSandbox(resp.Sandbox)
	s.client.rekey(old, id, s)
	s.rt.SetSessionID(id)
	s.updateInfo(agent.EventMeta{Raw: raw}, func(i *agent.SessionInfo) {
		i.ID = id
		// The fork's token totals start from its own history.
		s.tr.haveTotal = false
	})
	if err := s.threadCall(ctx, "thread/unsubscribe", map[string]any{"threadId": old}, nil); err != nil {
		s.rt.Logger().Debug("unsubscribing the rewound thread failed", "thread", old, "error", err)
	}
	s.client.forget(old)
	return nil
}

// ListTurnsOptions controls ListTurns.
type ListTurnsOptions struct {
	// Cursor continues from a previous page.
	Cursor string
	// Limit is the page size; zero means the server default.
	Limit int
	// Ascending lists oldest first; the default is newest first.
	Ascending bool
	// ItemsView is "notLoaded", "summary" (the server default), or "full".
	ItemsView string
}

// TurnPage is one page of ListTurns.
type TurnPage struct {
	Turns []TurnRecord
	// NextCursor continues the listing; empty on the last page.
	// BackwardsCursor lists from this page in the other direction.
	NextCursor      string
	BackwardsCursor string
}

// TurnRecord is a persisted turn.
type TurnRecord struct {
	ID string
	// Status is "completed", "interrupted", "failed", or "inProgress".
	Status string
	Error  string
	// StartedAt and CompletedAt are zero when unknown.
	StartedAt   time.Time
	CompletedAt time.Time
	Duration    time.Duration
	// Items holds what ItemsView loaded.
	Items []agent.Item
	Raw   json.RawMessage
}

// ListTurns lists the thread's turns with thread/turns/list.
func (s *Session) ListTurns(ctx context.Context, opts ListTurnsOptions) (*TurnPage, error) {
	params := map[string]any{"threadId": s.ID(), "sortDirection": "desc"}
	if opts.Ascending {
		params["sortDirection"] = "asc"
	}
	if opts.Cursor != "" {
		params["cursor"] = opts.Cursor
	}
	if opts.Limit > 0 {
		params["limit"] = opts.Limit
	}
	if opts.ItemsView != "" {
		params["itemsView"] = opts.ItemsView
	}
	var resp struct {
		Data            []json.RawMessage `json:"data"`
		NextCursor      *string           `json:"nextCursor"`
		BackwardsCursor *string           `json:"backwardsCursor"`
	}
	if err := s.threadCall(ctx, "thread/turns/list", params, &resp); err != nil {
		return nil, err
	}
	page := &TurnPage{NextCursor: providerutil.Deref(resp.NextCursor), BackwardsCursor: providerutil.Deref(resp.BackwardsCursor)}
	for _, raw := range resp.Data {
		var w struct {
			ID          string            `json:"id"`
			Status      string            `json:"status"`
			Error       *wireTurnError    `json:"error"`
			StartedAt   *int64            `json:"startedAt"`
			CompletedAt *int64            `json:"completedAt"`
			DurationMS  *int64            `json:"durationMs"`
			Items       []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(raw, &w) != nil {
			continue
		}
		rec := TurnRecord{ID: w.ID, Status: w.Status, Raw: raw}
		if w.Error != nil {
			rec.Error = w.Error.message()
		}
		if w.StartedAt != nil {
			rec.StartedAt = time.Unix(*w.StartedAt, 0)
		}
		if w.CompletedAt != nil {
			rec.CompletedAt = time.Unix(*w.CompletedAt, 0)
		}
		if w.DurationMS != nil {
			rec.Duration = time.Duration(*w.DurationMS) * time.Millisecond
		}
		for _, it := range w.Items {
			item, _ := convertItem(it)
			item.TurnID = w.ID
			rec.Items = append(rec.Items, item)
		}
		page.Turns = append(page.Turns, rec)
	}
	return page, nil
}

// Fork copies the thread into a new session on the same app-server.
// cfg supplies overrides for the new thread; its empty fields are taken
// from this session, and Resume and Fork are set.
func (s *Session) Fork(ctx context.Context, cfg agent.Config) (*Session, error) {
	cfg = s.inheritConfig(cfg)
	cfg.Resume = s.ID()
	cfg.Fork = true
	return s.provider.OpenSession(ctx, s.client, cfg)
}

// ReadThread returns the thread with thread/read.
func (s *Session) ReadThread(ctx context.Context, includeTurns bool) (json.RawMessage, error) {
	var raw json.RawMessage
	err := s.threadCall(ctx, "thread/read", map[string]any{"threadId": s.ID(), "includeTurns": includeTurns}, &raw)
	return raw, err
}

// Goal returns the thread goal, or nil when none is set.
func (s *Session) Goal(ctx context.Context) (*agent.Goal, error) {
	var resp struct {
		Goal *goalWire `json:"goal"`
	}
	if err := s.threadCall(ctx, "thread/goal/get", map[string]any{"threadId": s.ID()}, &resp); err != nil || resp.Goal == nil {
		return nil, err
	}
	return resp.Goal.goal(), nil
}

// SetGoalStatus changes the goal state, such as "paused" or "active".
func (s *Session) SetGoalStatus(ctx context.Context, status string) error {
	return s.threadCall(ctx, "thread/goal/set", map[string]any{"threadId": s.ID(), "status": status}, nil)
}

// SetGoal sets or updates the thread goal. A zero tokenBudget sets none.
func (s *Session) SetGoal(ctx context.Context, objective string, tokenBudget int64) error {
	params := map[string]any{"threadId": s.ID(), "objective": objective}
	if tokenBudget > 0 {
		params["tokenBudget"] = tokenBudget
	}
	return s.threadCall(ctx, "thread/goal/set", params, nil)
}

// ClearGoal removes the thread goal.
func (s *Session) ClearGoal(ctx context.Context) error {
	return s.threadCall(ctx, "thread/goal/clear", map[string]any{"threadId": s.ID()}, nil)
}

// ShellCommand runs a user shell command in the thread's context, like "!"
// in the Codex TUI. Output arrives as items. A zero timeout means the
// default.
func (s *Session) ShellCommand(ctx context.Context, command string, timeout time.Duration) error {
	params := map[string]any{"threadId": s.ID(), "command": command}
	if timeout > 0 {
		params["timeoutMs"] = timeout.Milliseconds()
	}
	return s.threadCall(ctx, "thread/shellCommand", params, nil)
}

// InjectItems appends raw Responses API items to the thread history.
func (s *Session) InjectItems(ctx context.Context, items []json.RawMessage) error {
	return s.threadCall(ctx, "thread/inject_items", map[string]any{"threadId": s.ID(), "items": items}, nil)
}

// Archive archives the thread.
func (s *Session) Archive(ctx context.Context) error {
	return s.threadCall(ctx, "thread/archive", map[string]any{"threadId": s.ID()}, nil)
}

// Unarchive restores an archived thread.
func (s *Session) Unarchive(ctx context.Context) error {
	return s.client.UnarchiveThread(ctx, s.ID())
}

// RateLimits reads the account's usage windows with
// account/rateLimits/read: the main Codex allowance first, then any
// model-specific ones, each with LimitID set. Later rate-limit updates are
// merged into this snapshot.
func (s *Session) RateLimits(ctx context.Context) ([]agent.RateLimit, error) {
	var resp struct {
		RateLimits map[string]json.RawMessage            `json:"rateLimits"`
		ByLimitID  map[string]map[string]json.RawMessage `json:"rateLimitsByLimitId"`
	}
	if err := s.threadCall(ctx, "account/rateLimits/read", nil, &resp); err != nil {
		return nil, err
	}
	snaps := resp.ByLimitID
	if len(snaps) == 0 && resp.RateLimits != nil {
		var head rateLimitSnapshot
		_ = json.Unmarshal(resp.RateLimits["limitId"], &head.LimitID)
		snaps = map[string]map[string]json.RawMessage{head.id(): resp.RateLimits}
	}
	ids := make([]string, 0, len(snaps))
	s.mu.Lock()
	for id, snap := range snaps {
		s.tr.rateLimits[id] = snap
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if a == mainLimitID || b == mainLimitID {
			return boolCompare(b == mainLimitID, a == mainLimitID)
		}
		return strings.Compare(a, b)
	})
	var out []agent.RateLimit
	for _, id := range ids {
		out = append(out, s.tr.rateLimitSnapshotLocked(id).limits()...)
	}
	s.mu.Unlock()
	return out, nil
}

func boolCompare(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// ReadMCPResource reads a resource, such as an MCP App's ui:// page, from
// an MCP server with mcpServer/resource/read. The result holds the native
// "contents" array.
func (s *Session) ReadMCPResource(ctx context.Context, server, uri string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := s.threadCall(ctx, "mcpServer/resource/read", map[string]any{"threadId": s.ID(), "server": server, "uri": uri}, &raw)
	return raw, err
}

// MCPToolResult is the result of CallMCPTool.
type MCPToolResult struct {
	Content           []json.RawMessage `json:"content"`
	IsError           bool              `json:"isError"`
	StructuredContent json.RawMessage   `json:"structuredContent"`
	Meta              json.RawMessage   `json:"_meta"`
}

// CallMCPTool calls an MCP server tool outside a turn with
// mcpServer/tool/call. args and meta may be nil.
func (s *Session) CallMCPTool(ctx context.Context, server, tool string, args, meta any) (*MCPToolResult, error) {
	params := map[string]any{"threadId": s.ID(), "server": server, "tool": tool}
	if args != nil {
		params["arguments"] = args
	}
	if meta != nil {
		params["_meta"] = meta
	}
	var out MCPToolResult
	if err := s.threadCall(ctx, "mcpServer/tool/call", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MCPServerStatus lists the MCP servers of this thread, reusing its MCP
// connections. opts.ThreadID is filled in.
func (s *Session) MCPServerStatus(ctx context.Context, opts MCPServerStatusOptions) ([]json.RawMessage, error) {
	opts.ThreadID = s.ID()
	return s.client.ListMCPServerStatus(ctx, opts)
}

// BackgroundTerminal is a shell process that outlived its command item.
type BackgroundTerminal struct {
	ProcessID string `json:"processId"`
	// ItemID is the commandExecution item that started it.
	ItemID  string `json:"itemId"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	OSPID   *int   `json:"osPid"`
}

// BackgroundTerminals lists the thread's background terminals.
func (s *Session) BackgroundTerminals(ctx context.Context) ([]BackgroundTerminal, error) {
	return listPages[BackgroundTerminal](ctx, s.threadCall, "thread/backgroundTerminals/list", map[string]any{"threadId": s.ID()})
}

// listPages calls a cursor-paginated method until its last page and
// returns every page's data. On an error it returns the pages read so far.
func listPages[T any](ctx context.Context, call func(ctx context.Context, method string, params, result any) error, method string, params map[string]any) ([]T, error) {
	var out []T
	for {
		var resp struct {
			Data       []T     `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := call(ctx, method, params, &resp); err != nil {
			return out, err
		}
		out = append(out, resp.Data...)
		if resp.NextCursor == nil || *resp.NextCursor == "" {
			return out, nil
		}
		params["cursor"] = *resp.NextCursor
	}
}

// TerminateBackgroundTerminal stops one background terminal, identified
// by its processId. It reports whether a process was stopped.
func (s *Session) TerminateBackgroundTerminal(ctx context.Context, processID string) (bool, error) {
	var resp struct {
		Terminated bool `json:"terminated"`
	}
	err := s.threadCall(ctx, "thread/backgroundTerminals/terminate", map[string]any{"threadId": s.ID(), "processId": processID}, &resp)
	return resp.Terminated, err
}

// CleanBackgroundTerminals stops every background terminal of the thread.
func (s *Session) CleanBackgroundTerminals(ctx context.Context) error {
	return s.threadCall(ctx, "thread/backgroundTerminals/clean", map[string]any{"threadId": s.ID()}, nil)
}

// UploadFeedback sends feedback about this thread with feedback/upload.
// classification is a category such as "bug"; reason may be empty.
func (s *Session) UploadFeedback(ctx context.Context, classification, reason string, includeLogs bool) error {
	params := map[string]any{"threadId": s.ID(), "classification": classification, "includeLogs": includeLogs}
	if reason != "" {
		params["reason"] = reason
	}
	return s.threadCall(ctx, "feedback/upload", params, nil)
}

// Call sends any thread method, filling in threadId.
func (s *Session) Call(ctx context.Context, method string, params map[string]any, result any) error {
	if params == nil {
		params = map[string]any{}
	}
	params["threadId"] = s.ID()
	return s.threadCall(ctx, method, params, result)
}

// threadCall sends a request about this session's thread, refusing once
// the session ended: a shared app-server would otherwise still act on it.
func (s *Session) threadCall(ctx context.Context, method string, params, result any) error {
	if s.rt.Finished() {
		return s.rt.ClosedErr()
	}
	return s.client.Call(ctx, method, params, result)
}

// Client methods.

// ListModels lists models. includeHidden includes models hidden from
// pickers.
func (c *Client) ListModels(ctx context.Context, includeHidden bool) ([]agent.Model, error) {
	raws, err := listPages[json.RawMessage](ctx, c.Call, "model/list", map[string]any{"includeHidden": includeHidden})
	var out []agent.Model
	for _, raw := range raws {
		var m struct {
			ID                        string `json:"id"`
			Model                     string `json:"model"`
			DisplayName               string `json:"displayName"`
			Description               string `json:"description"`
			IsDefault                 bool   `json:"isDefault"`
			DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
			SupportedReasoningEfforts []struct {
				ReasoningEffort string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
			ServiceTiers []struct {
				ID string `json:"id"`
			} `json:"serviceTiers"`
			AdditionalSpeedTiers []string `json:"additionalSpeedTiers"`
			DefaultServiceTier   *string  `json:"defaultServiceTier"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		model := agent.Model{
			ID:            cmp.Or(m.Model, m.ID),
			DisplayName:   m.DisplayName,
			Description:   m.Description,
			IsDefault:     m.IsDefault,
			DefaultEffort: agent.Effort(m.DefaultReasoningEffort),
			Raw:           raw,
		}
		for _, e := range m.SupportedReasoningEfforts {
			model.Efforts = append(model.Efforts, agent.Effort(e.ReasoningEffort))
		}
		for _, tier := range m.ServiceTiers {
			model.ServiceTiers = append(model.ServiceTiers, tier.ID)
		}
		if len(model.ServiceTiers) == 0 {
			model.ServiceTiers = m.AdditionalSpeedTiers
		}
		model.DefaultServiceTier = providerutil.Deref(m.DefaultServiceTier)
		out = append(out, model)
	}
	return out, err
}

// Account returns the signed-in account.
func (c *Client) Account(ctx context.Context) (json.RawMessage, error) {
	return c.CallRaw(ctx, "account/read", map[string]any{})
}

// RateLimits returns the account's rate limits.
func (c *Client) RateLimits(ctx context.Context) (json.RawMessage, error) {
	return c.CallRaw(ctx, "account/rateLimits/read", nil)
}

// ConsumeRateLimitResetCredit spends a rate-limit reset credit. Reuse
// idempotencyKey when retrying the same attempt. The outcome is "reset",
// "nothingToReset", "noCredit", or "alreadyRedeemed".
func (c *Client) ConsumeRateLimitResetCredit(ctx context.Context, idempotencyKey string) (string, error) {
	var resp struct {
		Outcome string `json:"outcome"`
	}
	err := c.Call(ctx, "account/rateLimitResetCredit/consume", map[string]any{"idempotencyKey": idempotencyKey}, &resp)
	return resp.Outcome, err
}

// UnarchiveThread restores an archived thread. A thread that is not
// archived is not an error.
func (c *Client) UnarchiveThread(ctx context.Context, threadID string) error {
	err := c.Call(ctx, "thread/unarchive", map[string]any{"threadId": threadID}, nil)
	if err != nil && isNotArchivedError(err) {
		return nil
	}
	return err
}

// DeleteThread permanently deletes a thread and its rollout with
// thread/delete.
func (c *Client) DeleteThread(ctx context.Context, threadID string) error {
	return c.Call(ctx, "thread/delete", map[string]any{"threadId": threadID}, nil)
}

// AuthStatus reads the signed-in account with account/read.
func (c *Client) AuthStatus(ctx context.Context) (*agent.AuthStatus, error) {
	raw, err := c.Account(ctx)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Account *struct {
			Type     string  `json:"type"`
			Email    *string `json:"email"`
			PlanType *string `json:"planType"`
		} `json:"account"`
		RequiresOpenaiAuth bool `json:"requiresOpenaiAuth"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("codex: decode account/read: %w", err)
	}
	// A provider that needs no OpenAI sign-in, such as a local model, is
	// usable without an account.
	out := &agent.AuthStatus{Authenticated: !resp.RequiresOpenaiAuth, Raw: raw}
	if a := resp.Account; a != nil {
		out.Authenticated = true
		out.Method = a.Type
		out.Email = providerutil.Deref(a.Email)
		out.Plan = providerutil.Deref(a.PlanType)
	}
	return out, nil
}

// ReadConfig returns the effective configuration.
func (c *Client) ReadConfig(ctx context.Context, cwd string) (json.RawMessage, error) {
	params := map[string]any{"includeLayers": false}
	if cwd != "" {
		params["cwd"] = cwd
	}
	return c.CallRaw(ctx, "config/read", params)
}

// WriteConfigValue writes one configuration value. mergeStrategy is
// "replace" or "upsert".
func (c *Client) WriteConfigValue(ctx context.Context, keyPath string, value any, mergeStrategy string) (json.RawMessage, error) {
	if mergeStrategy == "" {
		mergeStrategy = "replace"
	}
	return c.CallRaw(ctx, "config/value/write", map[string]any{"keyPath": keyPath, "value": value, "mergeStrategy": mergeStrategy})
}

// MCPServerStatusOptions filters ListMCPServerStatus.
type MCPServerStatusOptions struct {
	// ThreadID reuses that thread's MCP connections.
	ThreadID string
	// ServerName limits the listing to one server.
	ServerName string
	// Detail is "full" (the default) or "toolsAndAuthOnly".
	Detail string
	// Limit is the page size; zero means the server default.
	Limit int
}

// ListMCPServerStatus lists MCP servers with mcpServerStatus/list, reading
// every page. Each entry is a native McpServerStatus.
func (c *Client) ListMCPServerStatus(ctx context.Context, opts MCPServerStatusOptions) ([]json.RawMessage, error) {
	params := map[string]any{}
	if opts.ThreadID != "" {
		params["threadId"] = opts.ThreadID
	}
	if opts.ServerName != "" {
		params["serverName"] = opts.ServerName
	}
	if opts.Detail != "" {
		params["detail"] = opts.Detail
	}
	if opts.Limit > 0 {
		params["limit"] = opts.Limit
	}
	return listPages[json.RawMessage](ctx, c.Call, "mcpServerStatus/list", params)
}

// ReloadMCPServers reloads MCP server configuration.
func (c *Client) ReloadMCPServers(ctx context.Context) error {
	return c.Call(ctx, "config/mcpServer/reload", nil, nil)
}

// ListSkills lists skills for the given working directories.
func (c *Client) ListSkills(ctx context.Context, cwds []string) (json.RawMessage, error) {
	params := map[string]any{}
	if len(cwds) > 0 {
		params["cwds"] = cwds
	}
	return c.CallRaw(ctx, "skills/list", params)
}

// ListThreads lists threads with the native thread/list parameters.
func (c *Client) ListThreads(ctx context.Context, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	return c.CallRaw(ctx, "thread/list", params)
}

// ExecCommand runs a command in the Codex sandbox outside any thread with
// command/exec. params uses the native field names, such as "command"
// (an argv array) and "cwd".
func (c *Client) ExecCommand(ctx context.Context, params map[string]any) (json.RawMessage, error) {
	return c.CallRaw(ctx, "command/exec", params)
}

// FuzzyFileSearch searches file names under roots.
func (c *Client) FuzzyFileSearch(ctx context.Context, query string, roots []string) (json.RawMessage, error) {
	return c.CallRaw(ctx, "fuzzyFileSearch", map[string]any{"query": query, "roots": roots})
}
