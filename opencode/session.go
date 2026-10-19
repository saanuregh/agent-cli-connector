package opencode

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// Session is a live OpenCode session. It implements agent.Session and adds
// typed methods for OpenCode features.
type Session struct {
	provider *Provider
	cfg      agent.Config
	opts     SessionOptions
	rt       *providerkit.Runtime
	srv      *serverLease
	c        *Client
	host     *hostTools
	cwd      string

	stream    *eventStream
	streamMu  sync.Mutex
	connected chan struct{}
	connOnce  sync.Once
	closing   atomic.Bool
	closeOnce sync.Once

	// steerMu runs one steer at a time; the runtime tracks a single steer.
	steerMu sync.Mutex
	// startMu is held for reading while input is sent; see start.
	startMu sync.RWMutex

	mu   sync.Mutex
	info agent.SessionInfo
	// agentName is the session's current primary agent.
	agentName string
	model     *modelRef
	models    map[string]modelInfo
	commands  map[string]bool
	skills    map[string]bool
	// rulesMu serializes rule writes with the mode they are built for, so
	// a grant never writes the rules of a mode being replaced. lastMode is
	// the latest mode other than plan, which leaving plan restores.
	rulesMu  sync.Mutex
	lastMode agent.PermissionMode
	// rules are the session's permission rules on the server as far as
	// this library knows, unless rulesStale says a write may have failed
	// after the server applied it. inherited are the rules a resumed
	// session had, which an empty permission mode keeps. grants are the
	// AllowSession answers; see rules.go. agentRules are the rules of each
	// agent and defaultAgent the server's default agent, read again before
	// each rule write. subagentRules are the rule state of subagent
	// sessions by ID.
	rules        []PermissionRule
	rulesStale   bool
	inherited    []PermissionRule
	grants       []permissionGrant
	agentRules   map[string][]PermissionRule
	agentsLoaded bool
	defaultAgent string
	// agentFollowsDefault reports that the session has no agent of its own,
	// so it runs the server's default agent. rulesAgent is the agent the
	// rules on the server were built for.
	agentFollowsDefault bool
	rulesAgent          string
	// agentChanges counts agent selections, by events and switchAgent, so
	// that a read of the session's agent that a selection overtook is not
	// applied. rewritePending reports a background rewrite not yet started,
	// and resyncWanted that it reads the session's agent first.
	agentChanges   uint64
	rewritePending bool
	resyncWanted   bool
	// background tracks the background rule writes, which shutdown waits
	// for; backgroundClosed refuses new ones.
	backgroundMu     sync.Mutex
	background       sync.WaitGroup
	backgroundClosed bool
	subagentRules    map[string]*subagentRules
	// created reports that this library created the native session, as a
	// new session or a fork, so Ephemeral may delete it.
	created bool
	// mcpAdded lists the MCP servers this session holds a reference to.
	mcpAdded []string

	// Translation state; see translate.go.
	tr translator
}

var (
	_ agent.Session     = (*Session)(nil)
	_ agent.Titler      = (*Session)(nil)
	_ agent.Compacter   = (*Session)(nil)
	_ agent.Forker      = (*Session)(nil)
	_ agent.Rewinder    = (*Session)(nil)
	_ agent.TaskStopper = (*Session)(nil)
)

// modelRef names a model and its variant.
type modelRef struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
	Variant    string `json:"variant,omitempty"`
}

func (m *modelRef) slug() string {
	if m == nil || m.ID == "" {
		return ""
	}
	return m.ProviderID + "/" + m.ID
}

// effort returns the model's variant as an effort; the "default" variant
// is no effort.
func (m *modelRef) effort() agent.Effort {
	if m == nil || m.Variant == "default" {
		return ""
	}
	return agent.Effort(m.Variant)
}

// parseModel parses "provider/model" with an optional "#variant".
func parseModel(s string) (*modelRef, error) {
	s, variant, _ := strings.Cut(strings.TrimSpace(s), "#")
	provider, id, ok := strings.Cut(s, "/")
	if !ok || provider == "" || id == "" {
		return nil, fmt.Errorf("opencode: model %q must be provider/model", s)
	}
	return &modelRef{ProviderID: provider, ID: id, Variant: variant}, nil
}

// tokens is OpenCode's token use.
type tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

func (t tokens) addTo(u *agent.Usage) {
	u.Add(agent.Usage{
		InputTokens:      t.Input,
		OutputTokens:     t.Output + t.Reasoning,
		ReasoningTokens:  t.Reasoning,
		CacheReadTokens:  t.Cache.Read,
		CacheWriteTokens: t.Cache.Write,
		TotalTokens:      t.Input + t.Output + t.Reasoning + t.Cache.Read + t.Cache.Write,
	})
}

// nativeSession is OpenCode's SessionInfo.
type nativeSession struct {
	ID       string    `json:"id"`
	ParentID string    `json:"parentID"`
	Agent    string    `json:"agent"`
	Model    *modelRef `json:"model"`
	Cost     float64   `json:"cost"`
	Tokens   tokens    `json:"tokens"`
	Outcome  string    `json:"outcome"`
	Title    string    `json:"title"`
	Time     struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
	Permissions []PermissionRule `json:"permissions"`
	Location    struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

// modelInfo is OpenCode's ModelInfo.
type modelInfo struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	Variants   []struct {
		ID string `json:"id"`
	} `json:"variants"`
	Capabilities struct {
		Input []string `json:"input"`
	} `json:"capabilities"`
	Limit struct {
		Context int64 `json:"context"`
		Input   int64 `json:"input"`
		Output  int64 `json:"output"`
	} `json:"limit"`
}

func (m modelInfo) variants() []agent.Effort {
	out := make([]agent.Effort, 0, len(m.Variants))
	for _, v := range m.Variants {
		out = append(out, agent.Effort(v.ID))
	}
	return out
}

// window is the context size the model accepts as input.
func (m modelInfo) window() int64 { return cmp.Or(m.Limit.Input, m.Limit.Context) }

var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (p *Provider) startSession(ctx context.Context, cfg agent.Config, opts SessionOptions) (*Session, error) {
	if cfg.Fork && cfg.Resume == "" {
		return nil, errors.New("opencode: Fork requires Resume")
	}
	if cfg.ForkAtTurn != "" && !cfg.Fork {
		return nil, errors.New("opencode: ForkAtTurn requires Fork")
	}
	if p.opts.ServerURL != "" && (len(cfg.Env) > 0 || len(cfg.UnsetEnv) > 0) {
		// OpenCode replaces a session's whole environment, so this process
		// would have to send its own environment, secrets included, and
		// overwrite the remote server's PATH and HOME.
		return nil, providerkit.Unsupported(Name, "Env and UnsetEnv on an attached server (Options.ServerURL)")
	}
	if p.opts.ServerURL != "" && len(cfg.Tools) > 0 && !p.opts.AllowHostToolsOnAttachedServer {
		// Other clients' sessions in the same directory could call them.
		return nil, providerkit.Unsupported(Name, "Tools on an attached server without Options.AllowHostToolsOnAttachedServer")
	}
	shared := p.opts.SharedServer || p.opts.ServerURL != ""
	if shared && (cfg.SystemPrompt != "" || len(opts.Config) > 0) {
		return nil, providerkit.Unsupported(Name, "SystemPrompt and SessionOptions.Config on a shared or attached server (they configure the server process)")
	}
	for name := range cfg.MCPServers {
		if !mcpServerName.MatchString(name) {
			return nil, fmt.Errorf("opencode: invalid MCP server name %q (use up to 64 letters, digits, \"_\", and \"-\")", name)
		}
		if strings.HasPrefix(name, hostToolPrefix) {
			return nil, fmt.Errorf("opencode: MCP server name %q uses the reserved prefix %q", name, hostToolPrefix)
		}
	}
	if err := mcpNameCollision(cfg.MCPServers, slices.Collect(maps.Keys(cfg.MCPServers))); err != nil {
		return nil, err
	}
	if err := providerkit.CheckPermissionMode(Name, cfg.PermissionMode, p.Capabilities().PermissionModes); err != nil {
		return nil, err
	}
	var ignored []string
	if len(cfg.OutputSchema) > 0 {
		ignored = append(ignored, "OutputSchema")
	}
	notice, err := providerkit.IgnoredConfig(Name, cfg, ignored)
	if err != nil {
		return nil, err
	}
	s := &Session{
		provider:  p,
		cfg:       cfg,
		opts:      opts,
		rt:        providerkit.NewRuntime(Name, cfg),
		cwd:       providerutil.AbsDir(cfg.Cwd),
		connected: make(chan struct{}),
		models:    map[string]modelInfo{},
		commands:  map[string]bool{},
		skills:    map[string]bool{},
	}
	s.tr.init()
	s.rt.SetStarter(s.startQueued)
	startCtx, cancel := context.WithTimeout(ctx, p.startupTimeout())
	defer cancel()
	if err := s.connect(startCtx); err != nil {
		s.shutdown(nil)
		return nil, err
	}
	native, raw, err := s.open(startCtx)
	if err != nil {
		s.shutdown(nil)
		return nil, err
	}
	if err := s.configure(startCtx, native); err != nil {
		s.shutdown(nil)
		return nil, err
	}
	if cfg.Resume != "" && !cfg.Fork {
		// A resumed session may be running, waiting on requests, or have
		// subagents running in the background.
		s.reconcile(startCtx)
	}
	if s.Info().Model == "" {
		// The session follows the server's default model; name it.
		if def, err := s.defaultModel(startCtx); err == nil {
			s.setInfo(func(i *agent.SessionInfo) { i.Model = def.slug() })
		}
	}
	s.rt.Started(&agent.SessionStarted{EventMeta: agent.EventMeta{Raw: raw}, Info: s.Info(), Resumed: cfg.Resume != "" && !cfg.Fork})
	if notice != nil {
		s.rt.Emit(notice)
	}
	go s.watch()
	return s, nil
}

// connect gets a server and subscribes to its events. The stream is
// volatile, so it is opened before anything whose events matter.
func (s *Session) connect(ctx context.Context) error {
	p := s.provider
	switch {
	case p.opts.ServerURL != "":
		srv, err := p.attach(ctx, nil)
		if err != nil {
			return err
		}
		s.srv = &serverLease{Server: srv, shared: true, release: func() {}}
	case p.opts.SharedServer:
		lease, err := p.acquireShared(ctx, s.rt.Logger())
		if err != nil {
			return err
		}
		s.srv = lease
	default:
		srv, err := p.startServer(ctx, serverSpec{
			dir:    s.cwd,
			env:    s.cfg.Env,
			unset:  s.cfg.UnsetEnv,
			config: s.serverConfig(),
			logger: s.rt.Logger(),
		})
		if err != nil {
			return err
		}
		s.srv = &serverLease{Server: srv, release: func() { _ = srv.Close() }}
	}
	s.c = s.srv.client.withTap(s.cfg.WireTap)
	if len(s.cfg.Tools) > 0 {
		h, err := startHostTools(p.opts.HostToolsAddr, p.opts.HostToolsURL, s.cfg.Tools, s.ID)
		if err != nil {
			return fmt.Errorf("opencode: start host tool server: %w", err)
		}
		s.host = h
	}
	stream, err := s.c.subscribe(s.rt.Context(), s.handleRaw)
	if err != nil {
		return err
	}
	s.streamMu.Lock()
	s.stream = stream
	s.streamMu.Unlock()
	select {
	case <-s.connected:
		return nil
	case <-stream.done:
		return fmt.Errorf("opencode: event stream closed during startup: %w", stream.Err())
	case <-ctx.Done():
		return fmt.Errorf("opencode: waiting for the event stream: %w", ctx.Err())
	}
}

// serverConfig returns the OPENCODE_CONFIG_CONTENT additions of a private
// server.
func (s *Session) serverConfig() map[string]any {
	config := map[string]any{}
	if s.cfg.SystemPrompt != "" {
		agents := map[string]any{}
		for _, name := range []string{"build", "plan", s.opts.Agent} {
			if name != "" {
				agents[name] = map[string]any{"system": s.cfg.SystemPrompt}
			}
		}
		config["agents"] = agents
	}
	deepMerge(config, s.opts.Config)
	return config
}

// open creates, resumes, or forks the native session.
func (s *Session) open(ctx context.Context) (*nativeSession, json.RawMessage, error) {
	cfg := s.cfg
	var raw json.RawMessage
	// The agents give the default agent, which a session without one runs.
	if err := s.loadAgents(ctx); err != nil {
		return nil, nil, err
	}
	switch {
	case cfg.Resume != "" && cfg.Fork:
		body := map[string]any{}
		if cfg.ForkAtTurn != "" {
			before, err := s.forkBoundary(ctx, cfg.Resume, cfg.ForkAtTurn)
			if err != nil {
				return nil, nil, err
			}
			if before != "" {
				body["before"] = before
			}
		}
		if err := s.c.data(ctx, "POST", sessionPath(cfg.Resume, "/fork"), body, &raw); err != nil {
			return nil, nil, fmt.Errorf("opencode: fork session %s: %w", cfg.Resume, err)
		}
	case cfg.Resume != "":
		if err := s.c.data(ctx, "GET", sessionPath(cfg.Resume, ""), nil, &raw); err != nil {
			return nil, nil, fmt.Errorf("opencode: resume session %s: %w", cfg.Resume, err)
		}
	default:
		if err := s.loadModels(ctx); err != nil {
			s.rt.Logger().Warn("opencode: cannot list models", "error", err)
		}
		body := map[string]any{"location": map[string]any{"directory": s.cwd}}
		if cfg.SessionID != "" {
			body["id"] = cfg.SessionID
		}
		if cfg.Title != "" {
			// A session created with a title is never titled by OpenCode.
			body["title"] = cfg.Title
		}
		if cfg.PermissionMode == agent.PermissionPlan || s.opts.Agent != "" {
			body["agent"] = s.wantAgent(cfg.PermissionMode)
		}
		if cfg.Model != "" {
			m, err := s.resolveModel(cfg.Model, cfg.Effort)
			if err != nil {
				return nil, nil, err
			}
			body["model"] = m
		}
		if len(s.opts.Metadata) > 0 {
			body["metadata"] = s.opts.Metadata
		}
		rules := s.buildRules(cfg.PermissionMode, s.wantAgent(cfg.PermissionMode), nil)
		if rules != nil {
			body["permissions"] = rules
		}
		if err := s.c.data(ctx, "POST", "/api/session", body, &raw); err != nil {
			return nil, nil, fmt.Errorf("opencode: create session: %w", err)
		}
	}
	var native nativeSession
	if err := json.Unmarshal(raw, &native); err != nil || native.ID == "" {
		return nil, nil, fmt.Errorf("opencode: unexpected session answer %s", raw)
	}
	s.rt.SetSessionID(native.ID)
	s.mu.Lock()
	s.rules = native.Permissions
	// The rules the session has when it opens are what an empty mode
	// keeps, for new sessions too: OpenCode's own defaults allow
	// everything, so dropping them would drop the asks they hold.
	s.inherited = native.Permissions
	s.lastMode = cfg.PermissionMode
	if cfg.PermissionMode == agent.PermissionPlan {
		// Leaving plan by SetAgent asks before changes.
		s.lastMode = agent.PermissionDefault
	}
	s.created = cfg.Resume == "" || cfg.Fork
	s.agentName = cmp.Or(native.Agent, s.defaultAgent, "build")
	s.agentFollowsDefault = native.Agent == ""
	if cfg.Resume == "" {
		// The rules were created for wantAgent's agent; s.mu is held here.
		s.rulesAgent = cmp.Or(s.opts.Agent, s.defaultAgent, "build")
		if cfg.PermissionMode == agent.PermissionPlan {
			s.rulesAgent = "plan"
		}
	}
	s.model = native.Model
	s.info = agent.SessionInfo{
		Provider:       Name,
		Cwd:            cmp.Or(native.Location.Directory, s.cwd),
		Model:          native.Model.slug(),
		Effort:         native.Model.effort(),
		PermissionMode: cfg.PermissionMode,
		Title:          native.Title,
		Version:        s.srv.version,
		Raw:            raw,
	}
	if cfg.Cwd == "" && native.Location.Directory != "" {
		s.cwd = native.Location.Directory
	}
	s.mu.Unlock()
	return &native, raw, nil
}

// configure applies the settings a resumed or forked session does not take
// at creation, then registers tools and reads the catalog.
func (s *Session) configure(ctx context.Context, native *nativeSession) error {
	cfg := s.cfg
	id := native.ID
	base := sessionPath(id, "")
	if cfg.Resume != "" {
		if cfg.Cwd != "" && native.Location.Directory != "" && native.Location.Directory != s.cwd {
			if err := s.c.Call(ctx, "POST", base+"/move", map[string]any{"directory": s.cwd}, nil); err != nil {
				return fmt.Errorf("opencode: move session to %s: %w", s.cwd, err)
			}
			s.setInfo(func(i *agent.SessionInfo) { i.Cwd = s.cwd })
		}
		if err := s.loadModels(ctx); err != nil {
			s.rt.Logger().Warn("opencode: cannot list models", "error", err)
		}
		if cfg.Model != "" || cfg.Effort != "" {
			want := cmp.Or(cfg.Model, native.Model.slug())
			if want == "" {
				if def, err := s.defaultModel(ctx); err == nil {
					want = def.slug()
				}
			}
			if want != "" {
				m, err := s.resolveModel(want, cmp.Or(cfg.Effort, native.Model.effort()))
				if err != nil {
					return err
				}
				if native.Model == nil || m.slug() != native.Model.slug() || m.effort() != native.Model.effort() {
					if err := s.switchModel(ctx, m); err != nil {
						return err
					}
				}
			}
		}
		if a := s.wantAgent(cfg.PermissionMode); a != s.currentAgent() && (cfg.PermissionMode != "" || s.opts.Agent != "") {
			if err := s.switchAgent(ctx, a); err != nil {
				return err
			}
		}
		if err := s.writeRules(ctx, cfg.PermissionMode); err != nil {
			return err
		}
		if cfg.Title != "" && cfg.Title != native.Title {
			if err := s.SetTitle(ctx, cfg.Title); err != nil {
				return err
			}
		}
	}
	if cfg.Resume == "" && cfg.Model == "" && cfg.Effort != "" {
		// The effort of a new session on the default model.
		if err := s.SetEffort(ctx, cfg.Effort); err != nil {
			return err
		}
	}
	if err := s.applyEnvironment(ctx); err != nil {
		return err
	}
	entries := maps.Clone(s.opts.Instructions)
	if cfg.AppendSystemPrompt != "" {
		if entries == nil {
			entries = map[string]string{}
		}
		entries[instructionsKey] = cfg.AppendSystemPrompt
	}
	for key, value := range entries {
		if err := s.SetInstructions(ctx, key, value); err != nil {
			return err
		}
	}
	if err := s.registerMCP(ctx); err != nil {
		return err
	}
	s.loadCatalog(ctx)
	return nil
}

// instructionsKey names the instruction entry AppendSystemPrompt writes.
const instructionsKey = "agent-cli-connector"

// applyEnvironment sets the session environment on a shared or attached
// server, whose process environment is not the session's. OpenCode
// replaces the whole environment of the session's tools, so it is this
// process's environment with Options.Env and Config.Env applied and
// Config.UnsetEnv removed.
func (s *Session) applyEnvironment(ctx context.Context) error {
	cfg := s.cfg
	// Only a shared private server needs it: an attached server refuses
	// Env and UnsetEnv at Start, and a private one has them already.
	if !s.srv.shared || !s.srv.Private() || len(cfg.Env) == 0 && len(cfg.UnsetEnv) == 0 {
		return nil
	}
	overrides := map[string]string{}
	maps.Copy(overrides, s.provider.opts.Env)
	maps.Copy(overrides, cfg.Env)
	vars := map[string]string{}
	for _, kv := range providerkit.MergeEnv(providerkit.UnsetEnv(os.Environ(), cfg.UnsetEnv), providerkit.EnvList(overrides)) {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	if err := s.c.Call(ctx, "PUT", s.path("/environment"), map[string]any{"variables": vars}, nil); err != nil {
		return fmt.Errorf("opencode: set session environment: %w", err)
	}
	return nil
}

// mcpKey identifies an MCP registration of a server's directory, for the
// provider's reference counts.
func (s *Session) mcpKey(name string) string {
	return s.mcpDirKey() + "\x00" + name
}

// mcpDirKey identifies the session's server and directory, for the
// provider's per-directory MCP locks.
func (s *Session) mcpDirKey() string {
	return s.c.URL() + "\x00" + s.cwd
}

// mcpPath returns the route of MCP server name in the session's directory.
func (s *Session) mcpPath(name string) string {
	return withQuery("/api/experimental/mcp/"+url.PathEscape(name), locationQuery(s.cwd))
}

// mcpNameCollision refuses a server of servers whose tool rules could
// also decide the tools of another server in names; tool actions are
// "<server>_<tool>" with both names cleaned. EnabledTools deny
// "<server>_*" and allow listed tools, so such a server refuses any other
// whose cleaned name equals its own or extends it with "_", and any whose
// cleaned name it extends with "_". ApprovedTools allow single actions, so
// a server with only those refuses another server only when one of the
// actions starts with that server's cleaned name and "_", the only way the
// action can name that server's tool.
func mcpNameCollision(servers map[string]agent.MCPServer, names []string) error {
	for name, srv := range servers {
		if len(srv.EnabledTools) == 0 && len(srv.ApprovedTools) == 0 {
			continue
		}
		own := cleanToolName(name)
		for _, other := range names {
			if other == name {
				continue
			}
			theirs := cleanToolName(other)
			collides := false
			if len(srv.EnabledTools) > 0 {
				collides = own == theirs || strings.HasPrefix(theirs, own+"_") || strings.HasPrefix(own, theirs+"_")
			}
			for _, tool := range srv.ApprovedTools {
				collides = collides || own == theirs || strings.HasPrefix(toolAction(name, tool), theirs+"_")
			}
			if collides {
				return fmt.Errorf("opencode: MCP server %q has tool rules that would also match the tools of MCP server %q; rename one", name, other)
			}
		}
	}
	return nil
}

// registerMCP adds the host tool server and Config.MCPServers for the
// session's directory. A server name another session of this provider
// registered is shared; one the directory configures otherwise is left
// alone.
func (s *Session) registerMCP(ctx context.Context) error {
	if s.host == nil && len(s.cfg.MCPServers) == 0 {
		return nil
	}
	p := s.provider
	// One registration or removal at a time per server and directory: a
	// PUT connects the MCP server and can take its whole startup time, so
	// sessions in other directories must not wait for it.
	defer p.lockMCPDir(s.mcpDirKey())()
	existing := map[string]bool{}
	if len(s.cfg.MCPServers) > 0 {
		var list struct {
			Data []struct {
				Name string `json:"name"`
			} `json:"data"`
		}
		if err := s.c.Call(ctx, "GET", withQuery("/api/mcp", locationQuery(s.cwd)), nil, &list); err != nil {
			return fmt.Errorf("opencode: list MCP servers: %w", err)
		}
		for _, srv := range list.Data {
			existing[srv.Name] = true
		}
		if err := mcpNameCollision(s.cfg.MCPServers, slices.Collect(maps.Keys(existing))); err != nil {
			return err
		}
	}
	hold := func(name, config string) {
		p.mcpHold(s.mcpKey(name), config)
		s.mu.Lock()
		s.mcpAdded = append(s.mcpAdded, name)
		s.mu.Unlock()
	}
	// add registers server name with config, whose JSON encoding is raw.
	add := func(name string, config map[string]any, raw string) error {
		if err := s.c.Call(ctx, "PUT", s.mcpPath(name), map[string]any{"config": config}, nil); err != nil {
			return fmt.Errorf("opencode: add MCP server %s: %w", name, err)
		}
		hold(name, raw)
		return nil
	}
	if s.host != nil {
		config := s.host.config()
		if err := add(s.host.name, config, string(providerutil.Raw(config))); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.cfg.MCPServers)) {
		configMap := mcpConfig(s.cfg.MCPServers[name])
		config := string(providerutil.Raw(configMap))
		held, heldConfig := p.mcpHeld(s.mcpKey(name))
		switch {
		case held:
			if config != heldConfig {
				s.rt.Emit(&agent.Notice{Level: agent.NoticeWarning, Code: "mcp_server_config_differs",
					Message: "MCP server " + name + " is already registered for " + s.cwd + " by another session with a different configuration; that one is used"})
			}
			hold(name, "")
		case existing[name]:
			s.rt.Emit(&agent.Notice{Level: agent.NoticeWarning, Code: "mcp_server_exists",
				Message: "MCP server " + name + " is already configured for " + s.cwd + "; the configured one is kept"})
		default:
			if err := add(name, configMap, config); err != nil {
				return err
			}
		}
	}
	return nil
}

// releaseMCP drops this session's references to the MCP servers it
// registered and removes those no other session of the provider holds. A
// private server forgets them when it stops. The removal holds the
// directory's lock, so a session registering the same server waits for it
// instead of finding it listed and skipping it.
func (s *Session) releaseMCP(ctx context.Context, remove bool) {
	s.mu.Lock()
	added := s.mcpAdded
	s.mcpAdded = nil
	s.mu.Unlock()
	if len(added) == 0 {
		return
	}
	p := s.provider
	defer p.lockMCPDir(s.mcpDirKey())()
	for _, name := range added {
		if p.mcpDrop(s.mcpKey(name)) && remove {
			_ = s.c.Call(ctx, "DELETE", s.mcpPath(name), nil, nil)
		}
	}
}

func mcpConfig(srv agent.MCPServer) map[string]any {
	c := map[string]any{}
	switch srv.TransportOrDefault() {
	case agent.MCPStdio:
		c["type"] = "local"
		c["command"] = append([]string{srv.Command}, srv.Args...)
		if srv.Cwd != "" {
			c["cwd"] = srv.Cwd
		}
		if len(srv.Env) > 0 {
			c["environment"] = srv.Env
		}
	default:
		// OpenCode negotiates streamable HTTP or SSE for remote servers.
		c["type"] = "remote"
		c["url"] = srv.URL
		if len(srv.Headers) > 0 {
			c["headers"] = srv.Headers
		}
	}
	if srv.Enabled != nil {
		c["disabled"] = !*srv.Enabled
	}
	if srv.AlwaysLoad {
		c["codemode"] = false
	}
	timeout := map[string]any{}
	if srv.StartupTimeout > 0 {
		timeout["startup"] = srv.StartupTimeout.Milliseconds()
	}
	if srv.ToolTimeout > 0 {
		timeout["execution"] = srv.ToolTimeout.Milliseconds()
	}
	if len(timeout) > 0 {
		c["timeout"] = timeout
	}
	return c
}

// loadModels reads the models offered in the session's directory. A server
// that just started can list none for a moment, so an empty list is read
// again briefly.
func (s *Session) loadModels(ctx context.Context) error {
	var list []modelInfo
	for attempt := 0; ; attempt++ {
		if err := s.c.data(ctx, "GET", withQuery("/api/model", locationQuery(s.cwd)), nil, &list); err != nil {
			return err
		}
		if len(list) > 0 || attempt >= 8 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range list {
		s.models[m.ProviderID+"/"+m.ID] = m
	}
	return nil
}

// loadCatalog reads commands, skills, and MCP status into Info. Failures
// leave the catalog empty.
func (s *Session) loadCatalog(ctx context.Context) {
	var cmds []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	var skills []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Path        string `json:"path"`
	}
	var mcps []struct {
		Name   string `json:"name"`
		Status struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"status"`
	}
	loc := locationQuery(s.cwd)
	var cmdsErr, skillsErr, mcpsErr error
	var wg sync.WaitGroup
	wg.Go(func() { cmdsErr = s.c.data(ctx, "GET", withQuery("/api/command", loc), nil, &cmds) })
	wg.Go(func() { skillsErr = s.c.data(ctx, "GET", withQuery("/api/skill", loc), nil, &skills) })
	wg.Go(func() { mcpsErr = s.c.data(ctx, "GET", withQuery("/api/mcp", loc), nil, &mcps) })
	wg.Wait()
	var out []agent.SlashCommand
	known := map[string]bool{}
	skillSet := map[string]bool{}
	if cmdsErr == nil {
		for _, c := range cmds {
			known[c.Name] = true
			out = append(out, agent.SlashCommand{Name: c.Name, Description: c.Description, Kind: "command"})
		}
	}
	if skillsErr == nil {
		for _, sk := range skills {
			scope := "user"
			if strings.HasPrefix(sk.Path, "/builtin/") {
				scope = "builtin"
			} else if strings.HasPrefix(sk.Path, s.cwd+string(filepath.Separator)) {
				scope = "project"
			}
			skillSet[sk.ID] = true
			out = append(out, agent.SlashCommand{Name: sk.ID, Description: sk.Description, Kind: "skill", Path: sk.Path, Scope: scope})
		}
	}
	var statuses []agent.MCPServerStatus
	if mcpsErr == nil {
		for _, m := range mcps {
			statuses = append(statuses, agent.MCPServerStatus{Name: m.Name, Status: m.Status.Status, Error: m.Status.Error})
		}
	}
	s.mu.Lock()
	s.commands = known
	s.skills = skillSet
	s.info.Commands = out
	s.info.MCPServers = statuses
	s.mu.Unlock()
}

// ID returns the OpenCode session ID.
func (s *Session) ID() string { return s.rt.SessionID() }

// Provider returns "opencode".
func (s *Session) Provider() string { return Name }

// Info returns the latest session metadata.
func (s *Session) Info() agent.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.infoLocked()
}

func (s *Session) setInfo(update func(*agent.SessionInfo)) agent.SessionInfo {
	s.mu.Lock()
	update(&s.info)
	info := s.info
	s.mu.Unlock()
	info.ID = s.rt.SessionID()
	return info
}

// Capabilities reports the supported uniform features.
func (s *Session) Capabilities() agent.Capabilities { return s.provider.Capabilities() }

// Events returns the event stream.
func (s *Session) Events() <-chan agent.Event { return s.rt.Events() }

// PendingRequests lists unanswered requests.
func (s *Session) PendingRequests() []agent.Request { return s.rt.PendingRequests() }

// Done is closed after the session ended.
func (s *Session) Done() <-chan struct{} { return s.rt.Done() }

// Err returns why the session ended.
func (s *Session) Err() error { return s.rt.Err() }

// Server returns the server the session runs on. Do not close it: Close
// of the session releases it, and a shared server serves other sessions.
func (s *Session) Server() *Server { return s.srv.Server }

// Client returns the API client of the session's server. Its calls are
// passed to Config.WireTap.
func (s *Session) Client() *Client { return s.c }

// Call sends any API request; see Client.Call.
func (s *Session) Call(ctx context.Context, method, path string, body, out any) error {
	if s.rt.Finished() {
		return s.rt.ClosedErr()
	}
	return s.c.Call(ctx, method, path, body, out)
}

// data is Call for a route that wraps its answer in a data field.
func (s *Session) data(ctx context.Context, method, path string, body, out any) error {
	if s.rt.Finished() {
		return s.rt.ClosedErr()
	}
	return s.c.data(ctx, method, path, body, out)
}

// Run sends input and waits for the turn to finish.
func (s *Session) Run(ctx context.Context, in agent.Input) (*agent.TurnResult, error) {
	return providerkit.Run(ctx, s, in)
}

// promptKind is what a prepared input starts.
type promptKind int

const (
	promptMessage promptKind = iota
	promptCommand
	promptCompact
)

// prompt is input rendered for the API.
type prompt struct {
	kind     promptKind
	text     string
	command  string
	files    []map[string]any
	skills   []map[string]any
	agents   []map[string]any
	metadata map[string]any
	images   bool
}

func (p *prompt) body(id string) map[string]any {
	b := map[string]any{"text": p.text}
	if id != "" {
		b["id"] = id
	}
	if len(p.files) > 0 {
		b["files"] = p.files
	}
	if len(p.skills) > 0 {
		b["skills"] = p.skills
	}
	if len(p.agents) > 0 {
		b["agents"] = p.agents
	}
	if len(p.metadata) > 0 {
		b["metadata"] = p.metadata
	}
	return b
}

// newMessageID returns a client message ID. OpenCode requires the "msg_"
// prefix and refuses an ID another session already uses.
func newMessageID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "msg_" + strconv.FormatInt(time.Now().UnixMilli(), 16) + "acc" + hex.EncodeToString(b[:])
}

// prepare renders input.
func (s *Session) prepare(in agent.Input) (*prompt, error) {
	p := &prompt{metadata: map[string]any{}}
	var texts []string
	for _, part := range in.Parts {
		switch part.Type {
		case agent.PartText:
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		case agent.PartImage:
			uri, name, err := imageURI(part)
			if err != nil {
				return nil, err
			}
			file := map[string]any{"uri": uri}
			if name != "" {
				file["name"] = name
			}
			p.files = append(p.files, file)
			p.images = true
		case agent.PartFile:
			file := map[string]any{}
			switch {
			case len(part.Data) > 0:
				mediaType := cmp.Or(part.MediaType, providerutil.MediaType(part.Path), "application/octet-stream")
				file["uri"] = "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(part.Data)
			case part.Path != "":
				path := part.Path
				if !filepath.IsAbs(path) {
					path = filepath.Join(s.cwd, path)
				}
				file["uri"] = fileURI(filepath.ToSlash(path), runtime.GOOS == "windows")
			default:
				return nil, errors.New("opencode: file part has neither data nor path")
			}
			if part.Path != "" {
				file["name"] = filepath.Base(part.Path)
			}
			p.files = append(p.files, file)
		case agent.PartSkill:
			p.skills = append(p.skills, map[string]any{"id": part.Name})
		case agent.PartMention:
			p.agents = append(p.agents, map[string]any{"name": part.Name})
		}
	}
	p.text = strings.Join(texts, "\n\n")
	if o, err := providerkit.TurnOptions[TurnOptions](Name, in.ProviderOptions); err == nil {
		for _, a := range o.Agents {
			p.agents = append(p.agents, map[string]any{"name": a})
		}
		maps.Copy(p.metadata, o.Metadata)
	}
	if in.ClientID != "" {
		p.metadata["clientID"] = in.ClientID
	}
	if p.text == "" && len(p.files) == 0 && len(p.skills) == 0 {
		return nil, errors.New("opencode: input has no content")
	}
	if len(p.files) == 0 && len(p.skills) == 0 && len(p.agents) == 0 {
		if name, args, ok := slashCommand(strings.TrimSpace(p.text)); ok {
			s.mu.Lock()
			command, skill := s.commands[name], s.skills[name]
			s.mu.Unlock()
			switch {
			case command:
				p.kind, p.command, p.text = promptCommand, name, args
			case name == "compact" || name == "summarize":
				p.kind = promptCompact
			case skill:
				p.skills = []map[string]any{{"id": name}}
				p.text = cmp.Or(args, "Use the "+name+" skill.")
			}
		}
	}
	if p.images {
		s.mu.Lock()
		slug := s.model.slug()
		if in.Model != "" {
			if m, err := parseModel(in.Model); err == nil {
				slug = m.slug()
			}
		}
		m, known := s.models[slug]
		s.mu.Unlock()
		if known && len(m.Capabilities.Input) > 0 && !slices.Contains(m.Capabilities.Input, "image") {
			return nil, providerkit.Unsupported(Name, "image input for model "+slug)
		}
	}
	return p, nil
}

func slashCommand(text string) (name, args string, ok bool) {
	if !strings.HasPrefix(text, "/") || strings.HasPrefix(text, "//") {
		return "", "", false
	}
	head, rest, _ := strings.Cut(text[1:], " ")
	if head == "" || strings.ContainsAny(head, "/\n\t") {
		return "", "", false
	}
	return head, strings.TrimSpace(rest), true
}

// imageURI returns an image part as a data URI and its file name.
func imageURI(part agent.Part) (string, string, error) {
	if part.URL != "" {
		if strings.HasPrefix(part.URL, "data:") {
			return part.URL, "", nil
		}
		return "", "", providerkit.Unsupported(Name, "image URLs (OpenCode accepts inline images and file paths)")
	}
	data, mediaType, err := providerkit.ReadPart(part.Data, part.MediaType, part.Path)
	if err != nil {
		return "", "", fmt.Errorf("opencode: image %s: %w", part.Path, err)
	}
	if len(data) == 0 {
		return "", "", errors.New("opencode: image part has no data")
	}
	name := ""
	if part.Path != "" {
		name = filepath.Base(part.Path)
	}
	return "data:" + cmp.Or(mediaType, "image/png") + ";base64," + base64.StdEncoding.EncodeToString(data), name, nil
}

// Send submits input. While a turn runs, DeliverSteer sends the input into
// the running execution with OpenCode's steer delivery, DeliverInterrupt
// stops the execution and then runs the input, and DeliverQueue runs the
// input as the next turn. Commands and compaction always queue.
func (s *Session) Send(ctx context.Context, in agent.Input) (*agent.Turn, error) {
	if s.rt.Finished() {
		return nil, s.rt.ClosedErr()
	}
	if len(in.OutputSchema) > 0 {
		return nil, providerkit.Unsupported(Name, "output schema")
	}
	if err := providerkit.CheckInput(Name, in); err != nil {
		return nil, err
	}
	if _, err := providerkit.TurnOptions[TurnOptions](Name, in.ProviderOptions); err != nil {
		return nil, err
	}
	if in.Model != "" {
		// Applied when the input starts; refused now if it cannot work.
		if _, err := s.resolveModel(in.Model, in.Effort); err != nil {
			return nil, err
		}
	}
	p, err := s.prepare(in)
	if err != nil {
		return nil, err
	}
	id := newMessageID()
	turn, started, err := s.rt.StartIdle(ctx, id, func(ctx context.Context) error {
		if err := s.applyInputSettings(ctx, in); err != nil {
			return err
		}
		return s.start(ctx, id, p)
	})
	if started {
		return turn, err
	}
	current, _ := s.rt.Running()
	switch {
	case current == "":
		// Queued input is still waiting to start; keep the order.
		return s.rt.Enqueue(id, in)
	case in.Delivery == agent.DeliverSteer && p.kind == promptMessage:
		return s.steer(ctx, in, p, current)
	case in.Delivery == agent.DeliverInterrupt:
		turn, err := s.rt.Enqueue(id, in)
		if err != nil {
			return nil, err
		}
		return turn, s.interruptTurn(ctx, current)
	default:
		return s.rt.Enqueue(id, in)
	}
}

// startQueued starts queued input as turn id. A closing session starts
// nothing.
func (s *Session) startQueued(ctx context.Context, id string, in agent.Input) error {
	if s.closing.Load() {
		return agent.ErrClosed
	}
	if err := s.applyInputSettings(ctx, in); err != nil {
		return err
	}
	p, err := s.prepare(in)
	if err != nil {
		return err
	}
	return s.start(ctx, id, p)
}

// applyInputSettings applies an input's Model and Effort, which last from
// its turn on, when the input starts or joins a run.
func (s *Session) applyInputSettings(ctx context.Context, in agent.Input) error {
	if err := s.followDefaultAgent(ctx); err != nil {
		return err
	}
	if in.Model != "" {
		model := in.Model
		if in.Effort != "" && !strings.Contains(model, "#") {
			model += "#" + string(in.Effort)
		}
		return s.SetModel(ctx, model)
	}
	if in.Effort != "" {
		return s.SetEffort(ctx, in.Effort)
	}
	return nil
}

// start sends the input of turn id, which holds the running slot. The
// request survives the caller's cancellation, because OpenCode may have
// accepted it already.
func (s *Session) start(ctx context.Context, id string, p *prompt) error {
	// Close waits for starts in flight before it stops the session, and a
	// start after that sends nothing.
	s.startMu.RLock()
	defer s.startMu.RUnlock()
	if s.closing.Load() {
		return agent.ErrClosed
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	base := s.path("")
	if p.kind == promptCompact {
		s.mu.Lock()
		s.tr.compactTurns[id] = true
		s.mu.Unlock()
	}
	var err error
	switch p.kind {
	case promptCompact:
		err = s.c.Call(ctx, "POST", base+"/compact", map[string]any{"id": id}, nil)
	case promptCommand:
		err = s.c.Call(ctx, "POST", base+"/command", map[string]any{"name": p.command, "text": p.text}, nil)
	default:
		err = s.c.Call(ctx, "POST", base+"/prompt", p.body(id), nil)
	}
	if err != nil {
		s.mu.Lock()
		delete(s.tr.compactTurns, id)
		s.mu.Unlock()
		return err
	}
	if s.rt.Accepted(id, "") {
		// Interrupt asked for while the start was in flight.
		go func() { _ = s.sendInterrupt(context.Background(), id) }()
	}
	return nil
}

// steer sends input into the running turn current. OpenCode delivers it
// before the execution's next step; when the execution ends first, OpenCode
// runs another execution for it, which stays part of current, or, when
// current already ended, becomes a turn of its own. Input for a turn whose
// start is still in flight is queued instead.
//
// steerMu is held from BeginSteer until the steer's outcome is settled,
// and every interrupt takes it first, so an interrupt never sees a steer
// half done: it waits for the steer request, which is bounded by its own
// timeout. A turn that ends through an event while the steer is on its way
// marks the steer cancelled (see finishTurnLocked); the steer is then
// taken back, and its turn fails with agent.ErrCancelled or, when OpenCode
// already runs it, is interrupted.
func (s *Session) steer(ctx context.Context, in agent.Input, p *prompt, current string) (*agent.Turn, error) {
	id := newMessageID()
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	if !s.rt.BeginSteer(id, current) {
		return s.rt.Enqueue(id, in)
	}
	// Both handles and the steer's registration come before any request,
	// so neither turn can complete unseen and a turn that ends meanwhile
	// marks the steer cancelled.
	turn, err := s.rt.NewTurn(id)
	if err != nil {
		s.rt.EndSteer(id)
		return nil, err
	}
	joined, err := s.rt.NewTurn(current)
	if err != nil {
		s.rt.EndSteer(id)
		s.rt.AbandonTurn(id, nil)
		return nil, err
	}
	s.mu.Lock()
	if running, _ := s.rt.Running(); running != current {
		// current ended before the steer was registered, so its end did
		// not see the steer; nothing was sent.
		s.mu.Unlock()
		s.rt.EndSteer(id)
		s.rt.AbandonTurn(id, agent.ErrCancelled)
		return turn, nil
	}
	s.tr.steers[id] = current
	s.tr.inflight, s.tr.inflightCancelled, s.tr.inflightAdopted = id, false, false
	s.mu.Unlock()
	// drop forgets a steer that was not sent.
	drop := func(err error) {
		s.mu.Lock()
		delete(s.tr.steers, id)
		s.tr.inflight = ""
		var held *agent.TurnResult
		if len(s.tr.steersOfLocked(current)) == 0 {
			held = s.tr.releaseHeldLocked(current)
		}
		s.mu.Unlock()
		s.rt.EndSteer(id)
		s.rt.AbandonTurn(id, err)
		if held != nil {
			s.finishTurn(current, held)
		}
	}
	// A steer's model and effort apply to the run it joins.
	setCtx, cancelSet := context.WithTimeout(ctx, time.Minute)
	err = s.applyInputSettings(setCtx, in)
	cancelSet()
	if err != nil {
		drop(err)
		return nil, err
	}
	s.mu.Lock()
	cancelled := s.tr.inflightCancelled
	s.mu.Unlock()
	if cancelled {
		// current ended while the settings applied; nothing was sent.
		drop(agent.ErrCancelled)
		return turn, nil
	}
	body := p.body(id)
	body["delivery"] = "steer"
	// Like a start, the request survives the caller's cancellation, because
	// OpenCode may have accepted it already.
	postCtx, cancelPost := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	s.startMu.RLock()
	if s.closing.Load() {
		err = agent.ErrClosed
	} else {
		err = s.c.Call(postCtx, "POST", s.path("/prompt"), body, nil)
	}
	s.startMu.RUnlock()
	cancelPost()
	s.mu.Lock()
	cancelled, adoptedEver := s.tr.inflightCancelled, s.tr.inflightAdopted
	s.tr.inflight, s.tr.inflightCancelled, s.tr.inflightAdopted = "", false, false
	running, _ := s.rt.Running()
	// Adopted: OpenCode ran an execution for the steer as a turn of its
	// own, possibly already finished.
	adopted := running == id || adoptedEver
	joins := !adopted && err == nil && !cancelled && (s.tr.delivered[id] || s.rt.HasTurn(current))
	if !adopted && err == nil && !joins {
		// The steer waits for the execution OpenCode starts for it; a
		// cancelled one is interrupted if that execution starts.
		s.tr.awaiting = id
		if cancelled {
			s.tr.pendingInterrupt = id
		}
	}
	s.mu.Unlock()
	switch {
	case adopted && cancelled && running == id:
		s.rt.Interrupt(id)
		stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = s.sendInterruptLocked(stopCtx, id)
		cancelStop()
		return turn, nil
	case adopted:
		return turn, nil
	case err == nil && s.closing.Load():
		// Close may have looked for steers before this one was admitted;
		// it is taken back so nobody's next prompt delivers it.
		backCtx, cancelBack := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		s.cancelSteers(backCtx, []string{id})
		cancelBack()
		s.mu.Lock()
		delete(s.tr.steers, id)
		if s.tr.awaiting == id {
			s.tr.awaiting, s.tr.pendingInterrupt = "", ""
		}
		s.mu.Unlock()
		s.rt.EndSteer(id)
		s.rt.AbandonTurn(id, agent.ErrClosed)
		return nil, agent.ErrClosed
	case err != nil:
		drop(err)
		if t := errorTag(err); t == "ConflictError" || t == "SessionBusyError" {
			return nil, fmt.Errorf("%w: %w", agent.ErrSteerRejected, err)
		}
		return nil, err
	case cancelled:
		// current was interrupted or failed while the steer was on its
		// way; OpenCode parks undelivered input, so it is taken back.
		s.takeBackAwaiting(ctx, id)
		return turn, nil
	case joins:
		// The steer joins current: delivered already, or delivered before
		// current ends, which waits for it.
		s.rt.EndSteer(id)
		s.rt.AbandonTurn(id, nil)
		return joined, nil
	}
	// current succeeded without the steer, so OpenCode starts an execution
	// for it; the runtime adopts that run as the steer's turn.
	return turn, nil
}

// awaitingGrace is how long a taken-back steer waits for OpenCode to
// report the take-back or to start its execution before it is cancelled.
const awaitingGrace = 3 * time.Second

// takeBackAwaiting takes back steer id, which waits for its own execution.
// session.inbox.cancelled ends its turn with agent.ErrCancelled; when
// OpenCode started the execution instead, the pending interrupt stops it.
// The take-back survives the caller's cancellation. steerMu must be held.
func (s *Session) takeBackAwaiting(ctx context.Context, id string) {
	backCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	s.cancelSteers(backCtx, []string{id})
	cancel()
	// Whatever the take-back's outcome, the steer ends after the grace
	// period unless its execution started or OpenCode reported the
	// take-back; otherwise its steering mark would hold the queue forever.
	time.AfterFunc(awaitingGrace, func() { s.cancelAwaiting(id) })
}

// cancelAwaiting ends steer id, which waited for its own execution, with
// agent.ErrCancelled unless that execution started meanwhile.
func (s *Session) cancelAwaiting(id string) {
	s.mu.Lock()
	if s.tr.awaiting != id {
		s.mu.Unlock()
		return
	}
	s.tr.awaiting, s.tr.pendingInterrupt = "", ""
	s.mu.Unlock()
	s.rt.EndSteer(id)
	s.rt.AbandonTurn(id, agent.ErrCancelled)
}

// Interrupt stops the running turn and the background subagents still
// running. Steers OpenCode has not delivered yet are taken back; input
// queued by this library runs afterwards.
func (s *Session) Interrupt(ctx context.Context) error {
	err := s.interruptTurn(ctx, "")
	// The caller's context may have ended while the interrupt waited for a
	// steer; the subagents are stopped anyway.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	s.stopBackground(stopCtx)
	return err
}

// InterruptTurn stops turn id: queued input is dropped and fails with
// agent.ErrCancelled, and a running turn is interrupted. A turn that
// already ended is not an error.
func (s *Session) InterruptTurn(ctx context.Context, id string) error {
	if s.rt.CancelQueued(id) {
		return nil
	}
	return s.interruptTurn(ctx, id)
}

// stopBackground interrupts the subagent sessions still running in the
// background, which outlive the turn that started them.
func (s *Session) stopBackground(ctx context.Context) {
	s.mu.Lock()
	var ids []string
	for id, ch := range s.tr.children {
		if ch.task != nil && ch.task.Status == agent.TaskRunning && ch.task.Subagent != nil && ch.task.Subagent.Background {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	slices.Sort(ids)
	for _, id := range ids {
		_ = s.c.Call(ctx, "POST", sessionPath(id, "/interrupt"), nil, nil)
	}
}

// defaultModel returns the model the server picks for the session's
// directory when none is set.
func (s *Session) defaultModel(ctx context.Context) (*modelRef, error) {
	var out *modelInfo
	if err := s.c.data(ctx, "GET", withQuery("/api/model/default", locationQuery(s.cwd)), nil, &out); err != nil {
		return nil, err
	}
	if out == nil || out.ID == "" {
		return nil, errors.New("opencode: the server has no default model")
	}
	return &modelRef{ProviderID: out.ProviderID, ID: out.ID}, nil
}

// interruptTurn stops the running turn if it is want, or any running turn
// when want is empty, so an interrupt meant for one turn never stops the
// input that replaced it. It waits for a steer in flight; see steer.
func (s *Session) interruptTurn(ctx context.Context, want string) error {
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	current, starting := s.rt.Interrupt(want)
	if current == "" {
		// No turn runs. A steer waiting for the execution OpenCode starts
		// for it is taken back, and interrupted if that execution starts.
		s.mu.Lock()
		aw := s.tr.awaiting
		if aw != "" && (want == "" || want == aw) {
			s.tr.pendingInterrupt = aw
		} else {
			aw = ""
		}
		s.mu.Unlock()
		if aw != "" {
			s.takeBackAwaiting(ctx, aw)
		}
		return nil
	}
	if starting {
		// A start in flight sends the interrupt once OpenCode accepted it.
		return nil
	}
	return s.sendInterruptLocked(ctx, current)
}

// sendInterrupt interrupts turn turnID, which holds the slot.
// It is for interrupts recorded earlier, whose turn may have ended while
// they waited for steerMu: such an interrupt is dropped rather than sent,
// as it would stop the turn that replaced its target.
func (s *Session) sendInterrupt(ctx context.Context, turnID string) error {
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	if id, _ := s.rt.Interrupting(); id != turnID {
		return nil
	}
	return s.sendInterruptLocked(ctx, turnID)
}

// sendInterruptLocked interrupts turn turnID. steerMu must be held, so no
// steer is in flight and every steer of the turn is settled.
func (s *Session) sendInterruptLocked(ctx context.Context, turnID string) error {
	// The caller's context may have ended while it waited for steerMu; the
	// interrupt is sent anyway, as dropping it would leave the turn running.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	s.mu.Lock()
	held := s.tr.releaseHeldLocked(turnID)
	// The turn's steers are taken back here; its end must not take them
	// back again.
	steers := s.tr.steersOfLocked(turnID)
	for _, id := range steers {
		delete(s.tr.steers, id)
	}
	s.mu.Unlock()
	if held != nil {
		// The execution ended and the turn waited only for steers. They
		// are taken back first, so OpenCode cannot start their execution
		// after the interrupt, and the turn ends last, because ending it
		// frees the slot for queued input.
		s.cancelSteers(ctx, steers)
		var out struct {
			Interrupted bool `json:"interrupted"`
		}
		err := s.c.Call(ctx, "POST", s.path("/interrupt"), nil, &out)
		if err == nil && out.Interrupted {
			// OpenCode already ran the steers' execution; its end, which
			// the interrupt brings, finishes the turn.
			return nil
		}
		held.Status = agent.TurnInterrupted
		held.StopReason = "interrupted"
		s.endTurn(turnID, held)
		return nil
	}
	if err := s.c.Call(ctx, "POST", s.path("/interrupt"), nil, nil); err != nil {
		// The turn goes on, and so do its steers, which were not taken back,
		// unless the turn ended meanwhile: then they are taken back now, as
		// its end did not see them.
		s.rt.ClearInterrupt(turnID)
		s.mu.Lock()
		running, _ := s.rt.Running()
		if running == turnID {
			for _, id := range steers {
				s.tr.steers[id] = turnID
			}
		}
		s.mu.Unlock()
		if running != turnID {
			s.cancelSteers(ctx, steers)
		}
		return err
	}
	s.cancelSteers(ctx, steers)
	return nil
}

// cancelSteers takes back undelivered steers, which OpenCode would
// otherwise deliver with the next prompt.
func (s *Session) cancelSteers(ctx context.Context, ids []string) {
	for _, id := range ids {
		err := s.c.Call(ctx, "DELETE", s.path("/inbox/")+url.PathEscape(id), nil, nil)
		if err != nil && httpStatus(err) != 404 {
			s.rt.Logger().Warn("opencode: cannot take back a steer", "inbox", id, "error", err)
		}
	}
}

// resolveModel validates model against the server's list when it has one
// and returns the reference to send. A variant in the model string wins
// over effort; otherwise effort becomes the variant.
func (s *Session) resolveModel(model string, effort agent.Effort) (*modelRef, error) {
	m, err := parseModel(model)
	if err != nil {
		return nil, err
	}
	if m.Variant == "" && effort != "" {
		m.Variant = string(effort)
	}
	s.mu.Lock()
	info, known := s.models[m.slug()]
	listed := len(s.models) > 0
	s.mu.Unlock()
	if listed && !known {
		return nil, fmt.Errorf("opencode: model %s is not available on the server", m.slug())
	}
	if known && m.Variant != "" && m.Variant != "default" {
		if v := info.variants(); !slices.Contains(v, agent.Effort(m.Variant)) {
			return nil, providerkit.Unsupported(Name, fmt.Sprintf("effort %q for model %s (it offers %v)", m.Variant, m.slug(), v))
		}
	}
	return m, nil
}

func (s *Session) switchModel(ctx context.Context, m *modelRef) error {
	if err := s.Call(ctx, "POST", s.path("/model"), map[string]any{"model": m}, nil); err != nil {
		return err
	}
	s.mu.Lock()
	cp := *m
	s.model = &cp
	s.mu.Unlock()
	info := s.setInfo(func(i *agent.SessionInfo) { i.Model, i.Effort = m.slug(), m.effort() })
	s.rt.Emit(&agent.SessionUpdated{Info: info})
	return nil
}

// SetModel switches the model for later requests. model is
// "provider/model", optionally with "#variant"; without a variant the
// current effort is kept when the new model offers it.
func (s *Session) SetModel(ctx context.Context, model string) error {
	m, err := parseModel(model)
	if err != nil {
		return err
	}
	effort := agent.Effort(m.Variant)
	if effort == "" {
		s.mu.Lock()
		effort = s.model.effort()
		info, known := s.models[m.slug()]
		s.mu.Unlock()
		if known && !slices.Contains(info.variants(), effort) {
			effort = ""
		}
	}
	ref, err := s.resolveModel(m.ProviderID+"/"+m.ID, effort)
	if err != nil {
		return err
	}
	return s.switchModel(ctx, ref)
}

// SetEffort selects the current model's variant. An empty effort selects
// the model's default.
func (s *Session) SetEffort(ctx context.Context, effort agent.Effort) error {
	s.mu.Lock()
	cur := s.model
	s.mu.Unlock()
	if cur.slug() == "" {
		def, err := s.defaultModel(ctx)
		if err != nil {
			return fmt.Errorf("opencode: the session has no model to set the effort of: %w", err)
		}
		cur = def
	}
	m, err := s.resolveModel(cur.slug(), effort)
	if err != nil {
		return err
	}
	if effort == "" {
		m.Variant = ""
	}
	return s.switchModel(ctx, m)
}

// SetPermissionMode changes the permission rules and, for plan mode, the
// agent.
func (s *Session) SetPermissionMode(ctx context.Context, mode agent.PermissionMode) error {
	if err := providerkit.CheckPermissionMode(Name, mode, s.provider.Capabilities().PermissionModes); err != nil {
		return err
	}
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()
	// Read before the agent switch, whose event must not change it.
	previous := s.Info().PermissionMode
	previousAgent := s.currentAgent()
	s.mu.Lock()
	previousFollows := s.agentFollowsDefault
	s.mu.Unlock()
	// A session that follows the default agent keeps following it outside
	// plan mode; a switch would pin it.
	if a := s.wantAgent(mode); a != previousAgent && !(previousFollows && mode != agent.PermissionPlan) {
		if err := s.switchAgent(ctx, a); err != nil {
			return err
		}
	}
	// The mode is set before its rules are built, under rulesMu, so a grant
	// that follows builds the new mode's rules.
	s.setInfo(func(i *agent.SessionInfo) { i.PermissionMode = mode })
	if err := s.writeRulesLocked(ctx, mode); err != nil {
		s.setInfo(func(i *agent.SessionInfo) { i.PermissionMode = previous })
		back := context.WithoutCancel(ctx)
		if s.currentAgent() != previousAgent {
			// OpenCode cannot return a session to the default agent, so one
			// that followed it gets the default agent as it is now.
			if previousFollows {
				s.mu.Lock()
				previousAgent = cmp.Or(s.defaultAgent, previousAgent)
				s.mu.Unlock()
			}
			_ = s.switchAgent(back, previousAgent)
		}
		// A write that failed may have been applied.
		_ = s.writeRulesLocked(back, previous)
		return err
	}
	s.mu.Lock()
	if mode != agent.PermissionPlan {
		s.lastMode = mode
	}
	s.mu.Unlock()
	s.rt.Emit(&agent.SessionUpdated{Info: s.Info()})
	return nil
}

// wantAgent returns the agent for mode: plan, SessionOptions.Agent, or the
// server's default agent.
func (s *Session) wantAgent(mode agent.PermissionMode) string {
	if mode == agent.PermissionPlan {
		return "plan"
	}
	s.mu.Lock()
	def := s.defaultAgent
	s.mu.Unlock()
	return cmp.Or(s.opts.Agent, def, "build")
}

func (s *Session) currentAgent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentName
}

func (s *Session) switchAgent(ctx context.Context, name string) error {
	if err := s.c.Call(ctx, "POST", s.path("/agent"), map[string]any{"agent": name}, nil); err != nil {
		return err
	}
	s.mu.Lock()
	s.agentName, s.agentFollowsDefault = name, false
	s.agentChanges++
	s.mu.Unlock()
	return nil
}

// inBackground runs fn on its own goroutine unless the session closes;
// shutdown waits for it.
func (s *Session) inBackground(fn func()) {
	s.backgroundMu.Lock()
	defer s.backgroundMu.Unlock()
	if s.backgroundClosed {
		return
	}
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		fn()
	}()
}

// Close ends the session and stops a private server. Queued input is
// dropped and its turns fail with agent.ErrClosed. On a shared or attached server, which outlives the session,
// the running turn and background subagents are stopped and the MCP
// servers only this session used are removed.
func (s *Session) Close() error {
	s.shutdown(nil)
	return nil
}

// shutdown ends the session with cause; only the first call has an effect.
func (s *Session) shutdown(cause error) {
	s.closeOnce.Do(func() {
		s.closing.Store(true)
		// Nothing queued may start while the session closes, and a start
		// already sending finishes before the interrupt below.
		s.rt.AbandonQueue(agent.ErrClosed)
		s.startMu.Lock()
		//lint:ignore SA2001 the empty section waits for starts in flight
		s.startMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Background rule writes send nothing more and end before the
		// session does; they hold rulesMu, which shutdown does not take.
		s.backgroundMu.Lock()
		s.backgroundClosed = true
		s.backgroundMu.Unlock()
		written := make(chan struct{})
		go func() {
			s.background.Wait()
			close(written)
		}()
		select {
		case <-written:
		case <-ctx.Done():
		}
		id := s.rt.SessionID()
		alive := s.c != nil && s.srv != nil && id != "" && s.srv.exitError() == nil
		if alive {
			base := sessionPath(id, "")
			// Undelivered steers, of the running turn, a held turn, or one
			// waiting for an execution of its own, stay in OpenCode's inbox,
			// which is durable: the next prompt from any client, or after a
			// resume, would deliver them. They are taken back first, on
			// every server. A steer still on its way takes itself back.
			s.mu.Lock()
			steers := slices.Sorted(maps.Keys(s.tr.steers))
			clear(s.tr.steers)
			if aw := s.tr.awaiting; aw != "" {
				steers = append(steers, aw)
			}
			s.tr.awaiting, s.tr.pendingInterrupt = "", ""
			s.mu.Unlock()
			s.cancelSteers(ctx, steers)
			if s.srv.shared {
				if running, _ := s.rt.Running(); running != "" {
					// A server this session does not own would finish the
					// turn unseen.
					_ = s.c.Call(ctx, "POST", base+"/interrupt", nil, nil)
				}
				s.stopBackground(ctx)
			}
			if s.cfg.Ephemeral && s.created {
				if err := s.c.Call(ctx, "DELETE", base, nil, nil); err != nil {
					s.rt.Logger().Warn("opencode: cannot delete the ephemeral session", "session", id, "error", err)
				}
			}
		}
		if s.c != nil {
			s.releaseMCP(ctx, alive && s.srv.shared)
		}
		s.streamMu.Lock()
		stream := s.stream
		s.streamMu.Unlock()
		if stream != nil {
			stream.close()
		}
		if s.host != nil {
			s.host.close()
		}
		if s.srv != nil {
			s.srv.release()
		}
		s.rt.Finish(cause)
	})
}

// watch keeps the event stream open and ends the session when the server
// exits or the stream cannot be reopened.
func (s *Session) watch() {
	var backoff time.Duration
	for {
		s.streamMu.Lock()
		stream := s.stream
		s.streamMu.Unlock()
		opened := time.Now()
		select {
		case <-s.rt.Done():
			return
		case <-s.srv.Done():
			if !s.closing.Load() {
				s.shutdown(s.srv.exitError())
			}
			return
		case <-stream.done:
		}
		if s.closing.Load() || s.rt.Finished() {
			return
		}
		s.rt.Logger().Warn("opencode: event stream lost; reconnecting", "error", stream.Err())
		// A stream that ends soon after it opened waits longer each time.
		if time.Since(opened) < streamSettled {
			backoff = min(max(2*backoff, streamBackoff), maxStreamBackoff)
			select {
			case <-s.rt.Done():
				return
			case <-s.srv.Done(): // nil, so never ready, for an attached server
			case <-time.After(backoff):
			}
		} else {
			backoff = 0
		}
		if err := s.reconnect(); err != nil {
			if exitErr := s.srv.exitError(); exitErr != nil {
				err = exitErr
			}
			if !s.closing.Load() {
				s.shutdown(fmt.Errorf("opencode: event stream lost: %w", err))
			}
			return
		}
	}
}

// A stream that lasts streamSettled resets the reconnection backoff, which
// starts at streamBackoff and doubles up to maxStreamBackoff.
const (
	streamSettled    = 2 * time.Second
	streamBackoff    = 250 * time.Millisecond
	maxStreamBackoff = 30 * time.Second
)

// reconnect reopens the event stream and reconciles what happened while
// it was down: events sent meanwhile are lost.
func (s *Session) reconnect() error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			select {
			case <-s.rt.Done():
				return s.rt.ClosedErr()
			case <-time.After(time.Second):
			}
		}
		if s.srv.exitError() != nil {
			return s.srv.exitError()
		}
		var stream *eventStream
		stream, err = s.c.subscribe(s.rt.Context(), s.handleRaw)
		if err != nil {
			continue
		}
		s.streamMu.Lock()
		s.stream = stream
		s.streamMu.Unlock()
		ctx, cancel := context.WithTimeout(s.rt.Context(), 15*time.Second)
		// A restarted server forgot the session environment.
		if err := s.applyEnvironment(ctx); err != nil {
			s.rt.Logger().Warn("opencode: cannot set the session environment again", "error", err)
		}
		s.reconcile(ctx)
		// Lost events may have switched agents or reloaded the agents.
		s.resyncRules()
		cancel()
		return nil
	}
	return err
}

// reconcile brings the session in line with the server after a resume or
// a lost event stream: a turn whose execution ended unseen ends, a running
// execution and running subagents are adopted, and pending requests are
// shown or withdrawn to match the server's lists.
func (s *Session) reconcile(ctx context.Context) {
	// An execution that starts or ends while /active is read makes the
	// answer stale, so nothing is adopted from it.
	s.mu.Lock()
	seq := s.tr.execSeq
	s.mu.Unlock()
	var active map[string]json.RawMessage
	if err := s.c.data(ctx, "GET", "/api/session/active", nil, &active); err == nil {
		_, busy := active[s.ID()]
		running, starting := s.rt.Running()
		switch {
		case running != "" && !busy && !starting:
			s.endUnseen(ctx, running, seq)
		case running == "" && busy:
			s.adoptExecution(seq)
		}
		if children, err := listChildren(ctx, s.c, s.ID()); err == nil {
			s.adoptChildren(children, active, busy)
		}
	}
	for _, sid := range append([]string{s.ID()}, s.childIDs()...) {
		listed := map[string]bool{}
		var perms, forms []json.RawMessage
		permErr := s.c.data(ctx, "GET", sessionPath(sid, "/permission"), nil, &perms)
		formErr := s.c.data(ctx, "GET", sessionPath(sid, "/form"), nil, &forms)
		for _, raw := range perms {
			listed[requestID(raw)] = true
			s.permissionAsked(raw, raw)
		}
		for _, raw := range forms {
			listed[requestID(raw)] = true
			s.formCreated(raw, raw)
		}
		if permErr != nil || formErr != nil {
			continue
		}
		// Requests the server no longer lists were settled meanwhile.
		for _, req := range s.rt.PendingRequests() {
			s.mu.Lock()
			owner := s.tr.reqSession[req.RequestID()]
			s.mu.Unlock()
			if owner == sid && !listed[req.RequestID()] {
				s.rt.WithdrawRequest(req.RequestID())
			}
		}
	}
}

func requestID(raw json.RawMessage) string {
	var r struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.ID
}

// endUnseen ends turn running, whose execution is no longer active, when
// it ended while the stream was down: the session history holds an idle
// record after the turn's message, or this session saw the execution start
// and the session's latest outcome is its own. A turn whose execution this
// session never saw start and whose end is not recorded keeps running.
func (s *Session) endUnseen(ctx context.Context, running string, seq int) {
	s.mu.Lock()
	saw := s.tr.acc != nil && s.tr.acc.turn == running
	s.mu.Unlock()
	outcome, found := s.idleAfter(ctx, s.turnMessage(running))
	if !found {
		if !saw {
			return
		}
		var native nativeSession
		if s.c.data(ctx, "GET", s.path(""), nil, &native) != nil {
			return
		}
		outcome = native.Outcome
	}
	res := &agent.TurnResult{Status: agent.TurnInterrupted, StopReason: "connection_lost"}
	switch outcome {
	case "succeeded":
		res.Status, res.StopReason = agent.TurnSucceeded, "stop"
	case "failed":
		res.Status = agent.TurnFailed
		res.Err = &agent.TurnError{Code: "connection_lost", Message: "the execution failed while the event stream was down"}
	}
	// An execution that started or ended since /active was read makes its
	// answer stale: the turn may have started only now.
	s.mu.Lock()
	defer s.unlock()
	if current, _ := s.rt.Running(); current == running && s.tr.execSeq == seq {
		s.finishTurnLocked(running, res)
	}
}

// idleAfter returns the outcome of the newest idle record after message
// msg among the session's latest messages.
func (s *Session) idleAfter(ctx context.Context, msg string) (string, bool) {
	page, _, err := listMessages(ctx, s.c, s.ID(), "desc", "", 50)
	if err != nil {
		return "", false
	}
	outcome := ""
	for _, raw := range page {
		var m struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Outcome string `json:"outcome"`
		}
		_ = json.Unmarshal(raw, &m)
		if m.ID == msg {
			return outcome, outcome != ""
		}
		if m.Type == "idle" && outcome == "" {
			outcome = m.Outcome
		}
	}
	return "", false
}

// adoptExecution tracks an execution that runs without a turn of this
// session, such as one started before a resume, as a turn.
func (s *Session) adoptExecution(seq int) {
	s.mu.Lock()
	defer s.unlock()
	if running, _ := s.rt.Running(); running != "" || s.tr.execSeq != seq {
		return
	}
	running, _ := s.rt.RunStarted("")
	if running == "" {
		return
	}
	s.tr.acc = &turnAcc{turn: running}
	s.rt.Emit(&agent.TurnStarted{EventMeta: agent.EventMeta{TurnID: running}})
	s.rt.Emit(&agent.StatusChanged{EventMeta: agent.EventMeta{TurnID: running}, Status: agent.StatusRunning})
}

// adoptChildren registers the running subagent sessions this session does
// not know yet. One running while the session is idle runs in the
// background.
func (s *Session) adoptChildren(children []nativeSession, active map[string]json.RawMessage, busy bool) {
	s.mu.Lock()
	defer s.unlock()
	for _, n := range children {
		if _, running := active[n.ID]; !running || s.tr.children[n.ID] != nil {
			continue
		}
		ch := newChild(n.ID, s.ID(), n.Title, n.Agent, n.Model, 1)
		task := ch.task
		task.Subagent.Background = !busy
		s.tr.children[n.ID] = ch
		var rules *[]PermissionRule
		if n.Permissions != nil {
			rules = &n.Permissions
		}
		if id := n.ID; s.addSubagentLocked(id, n.Agent, rules) {
			s.afterUnlock(func() { s.writeSubagent(id) })
		}
		s.rt.Emit(&agent.TaskUpdated{Task: task.Clone()})
	}
}
