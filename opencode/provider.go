package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// Provider starts OpenCode sessions.
type Provider struct {
	opts   Options
	shared sharedServer
	// mcpRefs counts the sessions holding each MCP server they registered
	// on a shared or attached server; see Session.registerMCP.
	mcpMu       sync.Mutex
	mcpRefs     map[string]int
	mcpConfigs  map[string]string
	mcpDirLocks map[string]*dirLock
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

// dirLock is the lock of MCP registrations for one server and directory,
// kept while anyone holds or waits for it.
type dirLock struct {
	mu   sync.Mutex
	refs int
}

// lockMCPDir locks MCP registrations for one server and directory and
// returns the unlock function. Locks nobody uses are freed, as private
// servers come and go on random ports.
func (p *Provider) lockMCPDir(key string) (unlock func()) {
	p.mcpMu.Lock()
	if p.mcpDirLocks == nil {
		p.mcpDirLocks = map[string]*dirLock{}
	}
	l := p.mcpDirLocks[key]
	if l == nil {
		l = &dirLock{}
		p.mcpDirLocks[key] = l
	}
	l.refs++
	p.mcpMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		p.mcpMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(p.mcpDirLocks, key)
		}
		p.mcpMu.Unlock()
	}
}

// mcpHeld reports whether a session holds MCP registration key, and its
// configuration.
func (p *Provider) mcpHeld(key string) (bool, string) {
	p.mcpMu.Lock()
	defer p.mcpMu.Unlock()
	return p.mcpRefs[key] > 0, p.mcpConfigs[key]
}

// mcpHold adds a reference to registration key; config, when set, is the
// configuration it was registered with.
func (p *Provider) mcpHold(key, config string) {
	p.mcpMu.Lock()
	defer p.mcpMu.Unlock()
	if p.mcpRefs == nil {
		p.mcpRefs, p.mcpConfigs = map[string]int{}, map[string]string{}
	}
	p.mcpRefs[key]++
	if config != "" {
		p.mcpConfigs[key] = config
	}
}

// mcpDrop removes a reference to registration key and reports whether it
// was the last.
func (p *Provider) mcpDrop(key string) bool {
	p.mcpMu.Lock()
	defer p.mcpMu.Unlock()
	if p.mcpRefs[key]--; p.mcpRefs[key] > 0 {
		return false
	}
	delete(p.mcpRefs, key)
	delete(p.mcpConfigs, key)
	return true
}

// New returns a provider.
func New(opts Options) *Provider {
	return &Provider{opts: opts}
}

// Name returns "opencode".
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
			agent.PermissionReadOnly, agent.PermissionBypass,
		},
		PermissionPrompt: true,
		UserQuestions:    true,
		// Config.Tools fails on an attached server unless explicitly allowed.
		HostTools:     p.opts.ServerURL == "" || p.opts.AllowHostToolsOnAttachedServer,
		MCPServers:    true,
		Resume:        true,
		Fork:          true,
		SystemPrompt:  true,
		Tasks:         true,
		Subagents:     true,
		CostReporting: true,
		ForkAtTurn:    true,
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
	return p.startSession(ctx, cfg, opts)
}

func (p *Provider) path() string {
	if p.opts.Path != "" {
		return p.opts.Path
	}
	return "opencode"
}

func (p *Provider) startupTimeout() time.Duration {
	if p.opts.StartupTimeout > 0 {
		return p.opts.StartupTimeout
	}
	return 60 * time.Second
}

func (p *Provider) shutdownGrace() time.Duration {
	if p.opts.ShutdownGrace > 0 {
		return p.opts.ShutdownGrace
	}
	return 5 * time.Second
}

// Call sends any API request to the provider's server; see Client.Call.
// Without Options.ServerURL or a running shared server it starts a private
// server for the call.
func (p *Provider) Call(ctx context.Context, method, path string, body, out any) error {
	return p.withClient(ctx, func(c *Client) error { return c.Call(ctx, method, path, body, out) })
}

// Check reports the installed version and whether a model is usable.
// OpenCode's free models need no account, so a usable model counts as
// signed in. A version before 2.0 is reported in Err.
func (p *Provider) Check(ctx context.Context) (*agent.ProviderStatus, error) {
	var st *agent.ProviderStatus
	if p.opts.ServerURL != "" {
		srv, err := p.attach(ctx, nil)
		if err != nil {
			return providerkit.CheckFailed(ctx, &agent.ProviderStatus{}, err)
		}
		st = &agent.ProviderStatus{Installed: true, Version: srv.version}
	} else {
		var err error
		st, err = providerkit.VersionStatus(ctx, p.opts.Spawner, p.path(), providerkit.EnvList(p.opts.Env))
		if err != nil || !st.Installed || st.Err != nil {
			return st, err
		}
		if major(st.Version) < 2 {
			st.Err = fmt.Errorf("opencode: version %q is not supported; OpenCode 2 (@opencode/cli) is required", st.Version)
			return st, nil
		}
	}
	models, err := p.ListModels(ctx)
	if err != nil {
		return providerkit.CheckFailed(ctx, st, err)
	}
	st.Auth = &agent.AuthStatus{Authenticated: len(models) > 0}
	return st, nil
}

// ListModels lists the enabled models, as "provider/model". Each model's
// variants are its efforts.
func (p *Provider) ListModels(ctx context.Context) ([]agent.Model, error) {
	var out []agent.Model
	err := p.withClient(ctx, func(c *Client) error {
		var list []json.RawMessage
		if err := c.data(ctx, "GET", "/api/model", nil, &list); err != nil {
			return err
		}
		var def *modelInfo
		_ = c.data(ctx, "GET", "/api/model/default", nil, &def)
		for _, raw := range list {
			var m modelInfo
			if json.Unmarshal(raw, &m) != nil || !m.Enabled {
				continue
			}
			id := m.ProviderID + "/" + m.ID
			out = append(out, agent.Model{
				ID:              id,
				DisplayName:     m.Name,
				Provider:        m.ProviderID,
				IsDefault:       def != nil && def.ProviderID == m.ProviderID && def.ID == m.ID,
				Efforts:         m.variants(),
				ContextWindow:   int64(m.Limit.Context),
				InputModalities: m.Capabilities.Input,
				Raw:             publicModel(raw),
			})
		}
		return nil
	})
	return out, err
}

// secretModelFields are model and variant fields that can carry provider
// credentials, such as API keys in settings or headers.
var secretModelFields = []string{"settings", "headers", "body"}

// publicModel returns a model's JSON without its credential fields.
func publicModel(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	for _, k := range secretModelFields {
		delete(m, k)
	}
	var variants []map[string]json.RawMessage
	if json.Unmarshal(m["variants"], &variants) == nil && variants != nil {
		for _, v := range variants {
			for _, k := range secretModelFields {
				delete(v, k)
			}
		}
		m["variants"], _ = json.Marshal(variants)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

// ListSessions lists top-level sessions, newest first. Subagent sessions
// are left out; see ListSubagents.
func (p *Provider) ListSessions(ctx context.Context, opts agent.ListSessionsOptions) ([]agent.SessionSummary, error) {
	var out []agent.SessionSummary
	err := p.withClient(ctx, func(c *Client) error {
		dir := ""
		if opts.Cwd != "" {
			dir = providerutil.AbsDir(opts.Cwd)
		}
		skipped := 0
		return pageSessions(ctx, c, []string{param("order", "desc"), param("directory", dir)}, func(raw json.RawMessage) bool {
			var n nativeSession
			if json.Unmarshal(raw, &n) != nil || n.ParentID != "" {
				return true
			}
			if dir != "" && n.Location.Directory != dir {
				return true
			}
			if skipped < opts.Offset {
				skipped++
				return true
			}
			out = append(out, summary(n, raw))
			return opts.Limit <= 0 || len(out) < opts.Limit
		})
	})
	return out, err
}

// cursorString returns a page cursor as a query value.
func cursorString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func summary(n nativeSession, raw json.RawMessage) agent.SessionSummary {
	return agent.SessionSummary{
		ID:        n.ID,
		Provider:  Name,
		Title:     n.Title,
		Cwd:       n.Location.Directory,
		CreatedAt: providerutil.FromUnixMilli(n.Time.Created),
		UpdatedAt: providerutil.FromUnixMilli(n.Time.Updated),
		Model:     n.Model.slug(),
		Effort:    n.Model.effort(),
		Raw:       raw,
	}
}

// ReadHistory reads a persisted session's transcript. Items carry the ID
// of the user message that started their turn as TurnID, which Rewind and
// ForkAtTurn accept. With opts.SubagentID, a subagent task ID, it reads
// that subagent's session.
func (p *Provider) ReadHistory(ctx context.Context, sessionID string, opts agent.ReadHistoryOptions) ([]agent.Item, error) {
	sid := sessionID
	if opts.SubagentID != "" {
		sid = opts.SubagentID
	}
	var items []agent.Item
	err := p.withClient(ctx, func(c *Client) error {
		var err error
		items, err = readHistory(ctx, c, sid, opts.Limit, opts.Offset)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("opencode: read history of %s: %w", sid, err)
	}
	return items, nil
}

// DeleteSession deletes a persisted session. It cannot be undone.
func (p *Provider) DeleteSession(ctx context.Context, sessionID, cwd string) error {
	return p.withClient(ctx, func(c *Client) error {
		if err := c.Call(ctx, "DELETE", sessionPath(sessionID, ""), nil, nil); err != nil {
			return fmt.Errorf("opencode: delete session %s: %w", sessionID, err)
		}
		return nil
	})
}

// ListSubagents lists the subagent sessions a session started, including
// nested ones. Each task's ID is the subagent's session ID.
func (p *Provider) ListSubagents(ctx context.Context, sessionID string, _ agent.ListSubagentsOptions) ([]agent.Task, error) {
	var out []agent.Task
	err := p.withClient(ctx, func(c *Client) error {
		type pending struct {
			id    string
			depth int
		}
		// An unknown session lists no children; look it up so it reports
		// agent.ErrSessionNotFound as the other providers do.
		if err := c.data(ctx, "GET", sessionPath(sessionID, ""), nil, nil); err != nil {
			return err
		}
		queue := []pending{{id: sessionID}}
		for len(queue) > 0 && len(out) < 1000 {
			cur := queue[0]
			queue = queue[1:]
			children, err := listChildren(ctx, c, cur.id)
			if err != nil {
				if cur.id == sessionID {
					return err
				}
				continue // a subagent session that cannot be listed is skipped
			}
			for _, n := range children {
				task := agent.Task{ID: n.ID, Kind: agent.TaskSubagent, NativeKind: "subagent", Status: outcomeStatus(n.Outcome),
					Description: n.Title, Subagent: &agent.Subagent{Type: n.Agent, Model: n.Model.slug(), Effort: n.Model.effort(), Depth: cur.depth + 1}}
				if cur.depth > 0 {
					task.ParentID = cur.id
				}
				if n.Cost > 0 || n.Tokens.Input+n.Tokens.Output > 0 {
					u := &agent.Usage{CostUSD: n.Cost}
					n.Tokens.addTo(u)
					task.Usage = u
				}
				out = append(out, task)
				queue = append(queue, pending{id: n.ID, depth: cur.depth + 1})
			}
		}
		return nil
	})
	return out, err
}

func listChildren(ctx context.Context, c *Client, parent string) ([]nativeSession, error) {
	var out []nativeSession
	err := pageSessions(ctx, c, []string{param("parentID", parent), param("order", "asc")}, func(n nativeSession) bool {
		if n.ParentID == parent {
			out = append(out, n)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pageSessions lists sessions page by page, sending first only with the
// first page, and stops when visit returns false.
func pageSessions[T any](ctx context.Context, c *Client, first []string, visit func(T) bool) error {
	cursor := ""
	for {
		q := []string{param("limit", "100")}
		if cursor != "" {
			q = append(q, param("cursor", cursor))
		} else {
			q = append(q, first...)
		}
		var page struct {
			Data   []T `json:"data"`
			Cursor struct {
				Next json.RawMessage `json:"next"`
			} `json:"cursor"`
		}
		if err := c.Call(ctx, "GET", withQuery("/api/session", q...), nil, &page); err != nil {
			return err
		}
		for _, item := range page.Data {
			if !visit(item) {
				return nil
			}
		}
		cursor = cursorString(page.Cursor.Next)
		if cursor == "" || len(page.Data) == 0 {
			return nil
		}
	}
}

func outcomeStatus(outcome string) agent.TaskStatus {
	switch outcome {
	case "succeeded":
		return agent.TaskCompleted
	case "failed":
		return agent.TaskFailed
	case "interrupted":
		return agent.TaskStopped
	}
	return agent.TaskRunning
}
