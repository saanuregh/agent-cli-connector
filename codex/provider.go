package codex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// Provider starts Codex sessions.
type Provider struct {
	opts Options

	mu     sync.Mutex
	shared *Client
}

var (
	_ agent.Provider       = (*Provider)(nil)
	_ agent.ModelLister    = (*Provider)(nil)
	_ agent.SessionLister  = (*Provider)(nil)
	_ agent.HistoryReader  = (*Provider)(nil)
	_ agent.SessionDeleter = (*Provider)(nil)
	_ agent.Checker        = (*Provider)(nil)
	_ agent.SubagentLister = (*Provider)(nil)
)

// New returns a provider.
func New(opts Options) *Provider {
	return &Provider{opts: opts}
}

// Name returns "codex".
func (p *Provider) Name() string { return Name }

// Capabilities reports the supported uniform features.
func (p *Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:       true,
		ReasoningStream: true,
		Images:          true,
		Steer:           true,
		Queue:           true,
		Interrupt:       true,
		SetModel:        true,
		SetEffort:       true,
		PermissionModes: []agent.PermissionMode{
			agent.PermissionDefault, agent.PermissionAcceptEdits, agent.PermissionPlan,
			agent.PermissionBypass, agent.PermissionReadOnly, agent.PermissionDontAsk, agent.PermissionAuto,
		},
		PermissionPrompt: true,
		UserQuestions:    true,
		Elicitation:      true,
		HostTools:        !p.opts.DisableExperimentalAPI,
		MCPServers:       true,
		Resume:           true,
		Fork:             true,
		SystemPrompt:     true,
		OutputSchema:     true,
		Plans:            true,
		Tasks:            true,
		Subagents:        true,
		CostReporting:    false,
		RateLimits:       true,
		Hooks:            true,
		Goals:            true,
		ForkAtTurn:       true,
	}
}

// Start launches a session.
func (p *Provider) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	return p.StartSession(ctx, cfg)
}

// StartSession launches a session and returns the concrete type.
func (p *Provider) StartSession(ctx context.Context, cfg agent.Config) (*Session, error) {
	opts, err := providerkit.SessionOptions[SessionOptions](Name, cfg.ProviderOptions)
	if err != nil {
		return nil, err
	}
	if cfg.SessionID != "" {
		return nil, providerkit.Unsupported(Name, "choosing the session ID")
	}
	if cfg.Fork && cfg.Resume == "" {
		return nil, agentError("Fork requires Resume")
	}
	if cfg.ForkAtTurn != "" && !cfg.Fork {
		return nil, agentError("ForkAtTurn requires Fork")
	}
	// Refused before an app-server starts.
	if err := refusedConfig(cfg, !p.opts.SharedServer); err != nil {
		return nil, err
	}
	if _, err := providerkit.IgnoredConfig(Name, cfg, ignoredConfig(cfg, !p.opts.SharedServer, !p.opts.DisableExperimentalAPI)); err != nil {
		return nil, err
	}
	client, owned, err := p.client(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s, err := p.openSession(ctx, client, owned, cfg, opts)
	if err != nil {
		if owned {
			_ = client.Close()
		}
		return nil, err
	}
	return s, nil
}

// OpenSession starts a thread on an existing client. Closing the session
// leaves the client running.
func (p *Provider) OpenSession(ctx context.Context, client *Client, cfg agent.Config) (*Session, error) {
	opts, err := providerkit.SessionOptions[SessionOptions](Name, cfg.ProviderOptions)
	if err != nil {
		return nil, err
	}
	return p.openSession(ctx, client, false, cfg, opts)
}

type agentError string

func (e agentError) Error() string { return "codex: " + string(e) }

// client returns the app-server for a new session: the shared one, or a
// dedicated one the session owns.
func (p *Provider) client(ctx context.Context, cfg agent.Config) (*Client, bool, error) {
	if !p.opts.SharedServer {
		c, err := StartClient(ctx, p.opts, ClientConfig{Cwd: cfg.Cwd, Env: cfg.Env, UnsetEnv: cfg.UnsetEnv, Logger: cfg.Logger, WireTap: cfg.WireTap})
		return c, true, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shared != nil {
		select {
		case <-p.shared.Done():
			p.shared = nil
		default:
		}
	}
	if p.shared == nil {
		c, err := StartClient(ctx, p.opts, ClientConfig{WireTap: p.opts.WireTap})
		if err != nil {
			return nil, false, err
		}
		p.shared = c
	}
	return p.shared, false, nil
}

// Client returns the shared app-server, starting it if needed. It is only
// available with Options.SharedServer.
func (p *Provider) Client(ctx context.Context) (*Client, error) {
	if !p.opts.SharedServer {
		return nil, agentError("Client needs Options.SharedServer")
	}
	c, _, err := p.client(ctx, agent.Config{})
	return c, err
}

// Close stops the shared app-server, if any, ending its sessions.
func (p *Provider) Close() error {
	p.mu.Lock()
	c := p.shared
	p.shared = nil
	p.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

// withClient runs fn on the shared server or on a short-lived one.
func (p *Provider) withClient(ctx context.Context, fn func(*Client) error) error {
	if p.opts.SharedServer {
		c, err := p.Client(ctx)
		if err != nil {
			return err
		}
		return fn(c)
	}
	c, err := StartClient(ctx, p.opts, ClientConfig{})
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c)
}

// Check reports the installed Codex version from "codex --version" and the
// sign-in state from account/read on a short-lived app-server, or on the
// shared one. A missing executable yields Installed false and Err set.
func (p *Provider) Check(ctx context.Context) (*agent.ProviderStatus, error) {
	path := cmp.Or(p.opts.Path, "codex")
	status, err := providerkit.VersionStatus(ctx, p.opts.Spawner, path, providerkit.EnvList(p.opts.Env))
	if err != nil || !status.Installed || status.Err != nil {
		return status, err
	}
	err = p.withClient(ctx, func(c *Client) error {
		auth, err := c.AuthStatus(ctx)
		status.Auth = auth
		return err
	})
	if err != nil {
		return providerkit.CheckFailed(ctx, status, err)
	}
	return status, nil
}

// ListModels lists the models the app-server offers.
func (p *Provider) ListModels(ctx context.Context) ([]agent.Model, error) {
	var out []agent.Model
	err := p.withClient(ctx, func(c *Client) error {
		models, err := c.ListModels(ctx, false)
		out = models
		return err
	})
	return out, err
}

// ListSessions lists persisted threads, newest first. It reads every
// thread/list page until Offset and Limit are covered. When Options.Config sets
// model_provider, only threads of that provider are listed, because Codex
// otherwise lists only threads of its configured default provider.
func (p *Provider) ListSessions(ctx context.Context, opts agent.ListSessionsOptions) ([]agent.SessionSummary, error) {
	var out []agent.SessionSummary
	err := p.withClient(ctx, func(c *Client) error {
		seen := map[string]bool{}
		var cursor string
		for {
			params := map[string]any{"sortKey": "updated_at"}
			if opts.Limit > 0 {
				params["limit"] = opts.Offset + opts.Limit - len(seen)
			}
			if opts.Cwd != "" {
				params["cwd"] = providerutil.AbsDir(opts.Cwd)
			}
			if mp := p.modelProvider(); mp != "" {
				params["modelProviders"] = []string{mp}
			}
			if cursor != "" {
				params["cursor"] = cursor
			}
			var resp threadListResponse
			if err := c.Call(ctx, "thread/list", params, &resp); err != nil {
				return err
			}
			added := 0
			for _, raw := range resp.Data {
				sum, ok := sessionSummary(raw)
				if !ok || seen[sum.ID] {
					continue
				}
				seen[sum.ID] = true
				added++
				if len(seen) <= opts.Offset {
					continue
				}
				out = append(out, sum)
				if opts.Limit > 0 && len(out) >= opts.Limit {
					return nil
				}
			}
			// A page without new rows means the cursor stopped advancing.
			if added == 0 || providerutil.Deref(resp.NextCursor) == "" {
				return nil
			}
			cursor = *resp.NextCursor
		}
	})
	return out, err
}

// DeleteSession permanently deletes a thread with thread/delete. cwd is
// unused: Codex finds threads by ID.
func (p *Provider) DeleteSession(ctx context.Context, sessionID, cwd string) error {
	return p.withClient(ctx, func(c *Client) error {
		err := c.DeleteThread(ctx, sessionID)
		if err != nil && isNoRollout(err) {
			return fmt.Errorf("codex: delete thread %s: %w: %w", sessionID, agent.ErrSessionNotFound, err)
		}
		return err
	})
}

// modelProvider returns the model_provider set in Options.Config, without
// TOML quotes.
func (p *Provider) modelProvider() string {
	v := strings.TrimSpace(p.opts.Config["model_provider"])
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return v
}

func sessionSummary(raw json.RawMessage) (agent.SessionSummary, bool) {
	var th threadSummaryWire
	if json.Unmarshal(raw, &th) != nil || th.ID == "" {
		return agent.SessionSummary{}, false
	}
	preview := strings.TrimSpace(th.Preview)
	sum := agent.SessionSummary{
		ID:          th.ID,
		Provider:    Name,
		Title:       preview,
		Cwd:         th.Cwd,
		CreatedAt:   providerutil.FromUnixMilli(th.CreatedAt * 1000),
		UpdatedAt:   providerutil.FromUnixMilli(th.UpdatedAt * 1000),
		Path:        providerutil.Deref(th.Path),
		Model:       providerutil.Deref(th.Model),
		Effort:      agent.Effort(providerutil.Deref(th.ReasoningEffort)),
		FirstPrompt: preview,
		Raw:         raw,
	}
	if th.Name != nil && *th.Name != "" {
		sum.Title = *th.Name
	}
	return sum, true
}

// historyTurn is a persisted turn as thread/read returns it.
type historyTurn struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	StartedAt   *int64            `json:"startedAt"`
	CompletedAt *int64            `json:"completedAt"`
	Items       []json.RawMessage `json:"items"`
}

func readTurns(ctx context.Context, c *Client, threadID string) ([]historyTurn, error) {
	var resp threadReadTurnsResponse
	err := c.Call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &resp)
	return resp.Thread.Turns, err
}

// readLatestTurns reads a thread's newest turns with thread/turns/list
// until they hold at least want items, and returns them oldest first.
func readLatestTurns(ctx context.Context, c *Client, threadID string, want int) ([]historyTurn, error) {
	var turns []historyTurn
	items := 0
	cursor := ""
	for items < want {
		params := map[string]any{"threadId": threadID, "sortDirection": "desc", "itemsView": "full", "limit": 20}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var resp threadTurnsListHistoryResponse
		if err := c.Call(ctx, "thread/turns/list", params, &resp); err != nil {
			return nil, err
		}
		for _, turn := range resp.Data {
			turns = append(turns, turn)
			items += len(turn.Items)
		}
		cursor = providerutil.Deref(resp.NextCursor)
		if cursor == "" || len(resp.Data) == 0 {
			break
		}
	}
	slices.Reverse(turns)
	return turns, nil
}

// ReadHistory reads a persisted thread's items, or a subagent thread's
// when opts.SubagentID is set. Items carry their turn ID, and their time
// when Codex recorded one, else their turn's start (user messages) or
// completion time. With opts.Limit it reads only the newest turns it
// needs. Items an interrupted or failed turn left in progress come back
// cancelled or failed. A subagent's items carry the item that spawned it
// as ParentID when its parent thread's history names it.
func (p *Provider) ReadHistory(ctx context.Context, sessionID string, opts agent.ReadHistoryOptions) ([]agent.Item, error) {
	target := cmp.Or(opts.SubagentID, sessionID)
	var out []agent.Item
	err := p.withClient(ctx, func(c *Client) error {
		var parent string
		if opts.SubagentID != "" {
			parent = spawnItem(ctx, c, sessionID, opts.SubagentID)
		}
		var turns []historyTurn
		var err error
		if opts.Limit > 0 {
			turns, err = readLatestTurns(ctx, c, target, max(opts.Offset, 0)+opts.Limit)
		}
		if _, ok := errors.AsType[*agent.ProtocolError](err); opts.Limit <= 0 || ok {
			// A server that refuses thread/turns/list, such as an older
			// one, reads the whole thread.
			turns, err = readTurns(ctx, c, target)
		}
		if err != nil {
			return missingThread(target, err)
		}
		for _, turn := range turns {
			for _, raw := range turn.Items {
				it, _ := convertItem(raw)
				it.TurnID = turn.ID
				it.ParentID = parent
				it.SubagentID = opts.SubagentID
				it.Time = itemTime(raw, it.Kind, turn)
				endStoppedItem(&it, turn.Status)
				out = append(out, it)
			}
		}
		return nil
	})
	return providerutil.LastPage(out, opts.Limit, opts.Offset), err
}

// endStoppedItem ends an item that an interrupted or failed turn left in
// progress, as the live session does when the turn ends.
func endStoppedItem(it *agent.Item, turnStatus string) {
	if it.Status != agent.ItemRunning {
		return
	}
	switch turnStatus {
	case "interrupted":
		it.Status = agent.ItemCancelled
	case "failed":
		it.Status = agent.ItemFailed
		if it.Tool != nil {
			call := *it.Tool
			call.IsError = true
			it.Tool = &call
		}
	}
}

// spawnItem returns the item that spawned the subagent thread, read from
// the history of its parent thread (sessionID when Codex does not name
// one), or "" when that history does not name it.
func spawnItem(ctx context.Context, c *Client, sessionID, subagent string) string {
	var resp threadReadParentResponse
	parent := sessionID
	if c.Call(ctx, "thread/read", map[string]any{"threadId": subagent, "includeTurns": false}, &resp) == nil {
		parent = cmp.Or(providerutil.Deref(resp.Thread.ParentThreadID), sessionID)
	}
	turns, err := readTurns(ctx, c, parent)
	if err != nil {
		return ""
	}
	task := newSubagentTask(subagent)
	for _, turn := range turns {
		for _, raw := range turn.Items {
			for _, change := range subagentChanges(raw, "", 0, func(thread string) bool { return thread != subagent }) {
				change.apply(task)
			}
		}
	}
	return task.ToolCallID
}

// itemTime returns an item's own timestamp, or its turn's time.
func itemTime(raw json.RawMessage, kind agent.ItemKind, turn historyTurn) time.Time {
	var f itemTimestamps
	_ = json.Unmarshal(raw, &f)
	for _, v := range []json.RawMessage{f.Timestamp, f.CreatedAt} {
		if t := parseTime(v); !t.IsZero() {
			return t
		}
	}
	first, second := turn.CompletedAt, turn.StartedAt
	if kind == agent.ItemUserMessage {
		first, second = second, first
	}
	for _, v := range []*int64{first, second} {
		if v != nil && *v > 0 {
			return time.Unix(*v, 0)
		}
	}
	return time.Time{}
}

// parseTime reads Unix seconds, Unix milliseconds, or an RFC 3339 string.
func parseTime(v json.RawMessage) time.Time {
	var n int64
	if json.Unmarshal(v, &n) == nil && n > 0 {
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// maxSubagentThreads bounds the threads ListSubagents reads.
const maxSubagentThreads = 100

// ListSubagents lists the subagents a persisted thread started, including
// nested ones, from the subAgentActivity and collabAgentToolCall items of
// its history and its subagent threads' histories. Pass a subagent's
// thread ID as sessionID to list the subagents it started. A subagent
// thread that cannot be read is skipped.
func (p *Provider) ListSubagents(ctx context.Context, sessionID string, _ agent.ListSubagentsOptions) ([]agent.Task, error) {
	type pending struct {
		thread string
		depth  int
	}
	root := pending{thread: sessionID}
	tasks := map[string]*agent.Task{}
	var order []string
	err := p.withClient(ctx, func(c *Client) error {
		queue := []pending{root}
		seen := map[string]bool{root.thread: true}
		for reads := 0; len(queue) > 0 && reads < maxSubagentThreads; reads++ {
			cur := queue[0]
			queue = queue[1:]
			turns, err := readTurns(ctx, c, cur.thread)
			if err != nil {
				if cur == root {
					return missingThread(cur.thread, err)
				}
				continue
			}
			if task := tasks[cur.thread]; task != nil && task.Status == agent.TaskRunning && len(turns) > 0 {
				switch turns[len(turns)-1].Status {
				case "completed":
					task.Status = agent.TaskCompleted
				case "failed":
					task.Status = agent.TaskFailed
				case "interrupted":
					task.Status = agent.TaskStopped
				}
			}
			sender := cur.thread
			if cur.thread == sessionID {
				sender = ""
			}
			skip := func(thread string) bool { return thread == cur.thread || thread == sessionID }
			for _, turn := range turns {
				for _, raw := range turn.Items {
					for _, change := range subagentChanges(raw, sender, cur.depth, skip) {
						task := tasks[change.thread]
						if task == nil {
							task = newSubagentTask(change.thread)
							tasks[change.thread] = task
							order = append(order, change.thread)
						}
						change.apply(task)
						if !seen[change.thread] {
							seen[change.thread] = true
							queue = append(queue, pending{thread: change.thread, depth: max(task.Subagent.Depth, cur.depth+1)})
						}
					}
				}
			}
		}
		return nil
	})
	out := make([]agent.Task, 0, len(order))
	for _, id := range order {
		out = append(out, tasks[id].Clone())
	}
	return out, err
}

type threadListResponse struct {
	Data       []json.RawMessage `json:"data"`
	NextCursor *string           `json:"nextCursor"`
}

type threadSummaryWire struct {
	ID              string  `json:"id"`
	Name            *string `json:"name"`
	Preview         string  `json:"preview"`
	Cwd             string  `json:"cwd"`
	CreatedAt       int64   `json:"createdAt"`
	UpdatedAt       int64   `json:"updatedAt"`
	Path            *string `json:"path"`
	Model           *string `json:"model"`
	ReasoningEffort *string `json:"reasoningEffort"`
}

type threadReadTurnsResponse struct {
	Thread struct {
		Turns []historyTurn `json:"turns"`
	} `json:"thread"`
}

type threadTurnsListHistoryResponse struct {
	Data       []historyTurn `json:"data"`
	NextCursor *string       `json:"nextCursor"`
}

type threadReadParentResponse struct {
	Thread struct {
		ParentThreadID *string `json:"parentThreadId"`
	} `json:"thread"`
}

type itemTimestamps struct {
	Timestamp json.RawMessage `json:"timestamp"`
	CreatedAt json.RawMessage `json:"createdAt"`
}
