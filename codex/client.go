package codex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/jsonrpc"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// Notifications for threads that are not registered yet, such as the
// warnings Codex sends while thread/start runs, are kept within these
// bounds until the thread registers.
const (
	maxOrphans       = 256
	maxOrphanThreads = 32
)

// originatorEnv names the client to Codex before initialize runs.
const originatorEnv = "CODEX_INTERNAL_ORIGINATOR_OVERRIDE"

// daemonClientName is the client name Codex reserves for hosts that must
// not become the originator of model requests.
const daemonClientName = "codex_app_server_daemon"

// Client is a connection to one app-server process.
type Client struct {
	opts    Options
	proc    *providerkit.Process
	conn    *jsonrpc.Conn
	logger  *slog.Logger
	init    InitializeResult
	closing atomic.Bool
	done    chan struct{}

	mu      sync.Mutex
	threads map[string]*Session
	// children maps subagent threads to the session that spawned them.
	children map[string]*Session
	orphans  map[string][]orphan
	err      error
	// ended is set once the app-server exited; a session registered after
	// that ends at once, as the registered ones did.
	ended bool
}

type orphan struct {
	msg  *jsonrpc.Message
	line []byte
}

// InitializeResult is the app-server's answer to initialize.
type InitializeResult struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
}

// ClientConfig holds per-process settings for StartClient.
type ClientConfig struct {
	// Cwd is the app-server's working directory.
	Cwd string
	// Env is added to the process environment on top of Options.Env, and
	// UnsetEnv removes inherited variables on top of Options.UnsetEnv.
	Env      map[string]string
	UnsetEnv []string
	// Logger receives diagnostics; nil uses Options.Logger.
	Logger *slog.Logger
	// WireTap receives every protocol line.
	WireTap func(dir agent.WireDirection, line []byte)
}

// StartClient starts an app-server and completes the initialize handshake.
func StartClient(ctx context.Context, opts Options, cc ClientConfig) (*Client, error) {
	logger := cc.Logger
	if logger == nil {
		logger = opts.Logger
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	env := map[string]string{}
	maps.Copy(env, opts.Env)
	maps.Copy(env, cc.Env)
	if _, set := env[originatorEnv]; !set && opts.ClientName != "" && opts.ClientName != daemonClientName {
		env[originatorEnv] = opts.ClientName
	}
	path := cmp.Or(opts.Path, "codex")
	proc, err := providerkit.StartProcess(ctx, providerkit.ProcessOptions{
		Provider: Name,
		Spawner:  opts.Spawner,
		Command: agent.Command{Path: path, Args: appServerArgs(opts), Env: providerkit.EnvList(env), Dir: cc.Cwd,
			UnsetEnv: append(slices.Clone(opts.UnsetEnv), cc.UnsetEnv...)},
		Logger:  logger,
		WireTap: cc.WireTap,
	})
	if err != nil {
		return nil, err
	}
	c := &Client{
		opts:     opts,
		proc:     proc,
		logger:   logger,
		done:     make(chan struct{}),
		threads:  make(map[string]*Session),
		children: make(map[string]*Session),
		orphans:  make(map[string][]orphan),
	}
	c.conn = jsonrpc.New(proc.WriteLine, jsonrpc.Handler{Notify: c.notify, Request: c.request})
	go c.readLoop()

	timeout := opts.StartupTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	startCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	name, version := opts.ClientName, opts.ClientVersion
	if name == "" {
		name = "agent-cli-connector"
	}
	if version == "" {
		version = "0.1.0"
	}
	capabilities := map[string]any{
		"experimentalApi":    !opts.DisableExperimentalAPI,
		"requestAttestation": false,
	}
	if len(opts.OptOutNotifications) > 0 {
		capabilities["optOutNotificationMethods"] = opts.OptOutNotifications
	}
	extensions := map[string]any{"openai/form": map[string]any{}}
	for k, v := range opts.InitializeCapabilities {
		if k == "extensions" {
			if m, ok := v.(map[string]any); ok {
				maps.Copy(extensions, m)
				continue
			}
		}
		capabilities[k] = v
	}
	if _, replaced := capabilities["extensions"]; !replaced {
		for name, ext := range extensions {
			if ext == nil {
				delete(extensions, name)
			}
		}
		capabilities["extensions"] = extensions
	}
	var title any
	if opts.ClientTitle != "" {
		title = opts.ClientTitle
	}
	err = c.conn.Call(startCtx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": name, "title": title, "version": version},
		"capabilities": capabilities,
	}, &c.init)
	if err == nil {
		err = c.conn.Notify("initialized", nil)
	}
	if err != nil {
		c.Close()
		if exitErr := c.proc.ExitError(); exitErr != nil {
			return nil, fmt.Errorf("codex: initialize: %w", exitErr)
		}
		return nil, fmt.Errorf("codex: initialize: %w", err)
	}
	return c, nil
}

func appServerArgs(opts Options) []string {
	args := []string{"app-server"}
	for _, k := range slices.Sorted(maps.Keys(opts.Config)) {
		args = append(args, "-c", k+"="+opts.Config[k])
	}
	for _, f := range opts.Enable {
		args = append(args, "--enable", f)
	}
	for _, f := range opts.Disable {
		args = append(args, "--disable", f)
	}
	return append(args, opts.ExtraArgs...)
}

// Initialization returns the initialize result.
func (c *Client) Initialization() InitializeResult { return c.init }

// Version returns the Codex version parsed from the initialize user agent,
// such as "0.163.0" in "codex_cli_rs/0.163.0 (Linux)", or "" when absent.
func (c *Client) Version() string {
	ua := c.init.UserAgent
	_, after, ok := strings.Cut(ua, "/")
	if !ok {
		return ""
	}
	rest, _, _ := strings.Cut(after, " ")
	return providerutil.ParseVersion(rest)
}

// Done is closed after the app-server exited.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the app-server ended, or nil after Close.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Call sends any app-server request and decodes the result into result,
// which may be nil. Use it for methods without a typed wrapper.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	return protocolError(method, c.conn.Call(ctx, method, params, result))
}

// callOrdered sends a request whose result is not needed and runs reply on
// the reading goroutine before any later message is handled.
func (c *Client) callOrdered(ctx context.Context, method string, params any, reply func(*jsonrpc.Message)) error {
	_, err := c.conn.CallOrdered(ctx, method, params, reply)
	return protocolError(method, err)
}

// protocolError converts a JSON-RPC error answer to an *agent.ProtocolError.
func protocolError(method string, err error) error {
	if rpcErr, ok := errors.AsType[*jsonrpc.Error](err); ok {
		var data struct {
			CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
		}
		_ = json.Unmarshal(rpcErr.Data, &data)
		return &agent.ProtocolError{Provider: Name, Method: method, Code: rpcErr.Code, Message: rpcErr.Message, ErrorCode: errorCode(data.CodexErrorInfo), Data: rpcErr.Data}
	}
	return err
}

// CallRaw sends any app-server request and returns the raw result.
func (c *Client) CallRaw(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.Call(ctx, method, params, &raw)
	return raw, err
}

// Notify sends a client notification.
func (c *Client) Notify(method string, params any) error {
	return c.conn.Notify(method, params)
}

// Close stops the app-server and ends every session on it.
func (c *Client) Close() error {
	if !c.closing.CompareAndSwap(false, true) {
		<-c.done
		return nil
	}
	c.proc.Stop(c.shutdownGrace())
	<-c.done
	return nil
}

func (c *Client) shutdownGrace() time.Duration {
	if c.opts.ShutdownGrace > 0 {
		return c.opts.ShutdownGrace
	}
	return 5 * time.Second
}

func (c *Client) readLoop() {
	err := c.proc.ReadLines(func(line []byte) {
		if err := c.conn.Receive(line); err != nil {
			c.logger.Warn("ignoring malformed codex output", "error", err)
		}
	})
	if err != nil {
		c.logger.Warn("reading codex output failed", "error", err)
	}
	<-c.proc.Exited()
	var exitErr error
	if !c.closing.Load() {
		exitErr = c.proc.ExitError()
	}
	closeErr := exitErr
	if closeErr == nil {
		closeErr = agent.ErrClosed
	}
	c.conn.Close(closeErr)
	c.mu.Lock()
	c.ended = true
	c.err = exitErr
	threads := slices.Collect(maps.Values(c.threads))
	c.mu.Unlock()
	for _, s := range threads {
		s.rt.Finish(exitErr)
	}
	close(c.done)
}

func (c *Client) register(s *Session) {
	c.publish(s.ID(), s, false)
}

// publish replays a thread's held notifications to s, then routes new ones
// to it. Until the held ones are drained, new notifications keep queueing
// behind them, so s sees them in arrival order. On an app-server that
// already exited, s ends with the app-server's error instead.
func (c *Client) publish(threadID string, s *Session, child bool) {
	for {
		c.mu.Lock()
		pending := c.orphans[threadID]
		delete(c.orphans, threadID)
		if len(pending) == 0 {
			if c.ended {
				err := c.err
				c.mu.Unlock()
				s.rt.Finish(err)
				return
			}
			if child {
				c.children[threadID] = s
			} else {
				c.threads[threadID] = s
			}
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		for _, o := range pending {
			s.notify(o.msg, o.line)
		}
	}
}

func (c *Client) unregister(threadID string) {
	c.mu.Lock()
	delete(c.threads, threadID)
	for child, s := range c.children {
		if s.ID() == threadID {
			delete(c.children, child)
		}
	}
	c.mu.Unlock()
}

// forget drops the held notifications of a thread no session will
// register, such as those that arrive while a closed thread unsubscribes,
// so they do not use up the bounds kept for threads that will.
func (c *Client) forget(threadID string) {
	c.mu.Lock()
	delete(c.orphans, threadID)
	c.mu.Unlock()
}

// rekey routes the messages of thread id to s instead of thread old, after
// s moved to a fork of old.
func (c *Client) rekey(old, id string, s *Session) {
	c.mu.Lock()
	if c.threads[old] == s {
		delete(c.threads, old)
	}
	c.mu.Unlock()
	c.publish(id, s, false)
}

// registerChild routes a subagent thread's messages to its parent session.
func (c *Client) registerChild(threadID string, parent *Session) {
	c.mu.Lock()
	_, own := c.threads[threadID]
	_, known := c.children[threadID]
	c.mu.Unlock()
	if own || known {
		return
	}
	c.publish(threadID, parent, true)
}

// thread returns the session that handles messages of a thread: the
// thread's own session or the parent of a subagent thread.
func (c *Client) thread(id string) *Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.threads[id]; s != nil {
		return s
	}
	return c.children[id]
}

type threadRef struct {
	ThreadID       *string `json:"threadId"`
	ConversationID *string `json:"conversationId"`
}

func rootTurnOf(params json.RawMessage) string {
	var p struct {
		Turn struct {
			RootTurnID *string `json:"rootTurnId"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &p) != nil || p.Turn.RootTurnID == nil {
		return ""
	}
	return *p.Turn.RootTurnID
}

func threadOf(params json.RawMessage) string {
	var ref threadRef
	if json.Unmarshal(params, &ref) != nil {
		return ""
	}
	if ref.ThreadID != nil {
		return *ref.ThreadID
	}
	if ref.ConversationID != nil {
		return *ref.ConversationID
	}
	return ""
}

func (c *Client) notify(msg *jsonrpc.Message, line []byte) {
	id := threadOf(msg.Params)
	if id == "" {
		c.mu.Lock()
		threads := slices.Collect(maps.Values(c.threads))
		c.mu.Unlock()
		for _, s := range threads {
			s.notify(msg, line)
		}
		return
	}
	c.mu.Lock()
	s := c.threads[id]
	if s == nil {
		s = c.children[id]
	}
	var owner *Session
	if s == nil {
		if len(c.orphans) < maxOrphanThreads && len(c.orphans[id]) < maxOrphans {
			c.orphans[id] = append(c.orphans[id], orphan{msg, line})
		}
		if msg.Method == "turn/started" {
			// A thread whose turn descends from one of our turns is a
			// subagent of that session, even before an item names it.
			root := rootTurnOf(msg.Params)
			for _, t := range c.threads {
				if t.ownsTurn(root) {
					owner = t
					break
				}
			}
		}
	}
	c.mu.Unlock()
	if s != nil {
		s.notify(msg, line)
	}
	if owner != nil {
		owner.tr.addChild(id, "")
	}
}

func (c *Client) request(ctx context.Context, msg *jsonrpc.Message, line []byte) (any, error) {
	if id := threadOf(msg.Params); id != "" {
		if s := c.thread(id); s != nil {
			return s.serverRequest(msg, line)
		}
	}
	switch msg.Method {
	case "currentTime/read":
		return map[string]any{"currentTimeAt": time.Now().UnixMilli()}, nil
	case "account/chatgptAuthTokens/refresh":
		if c.opts.OnAuthTokensRefresh != nil {
			return c.opts.OnAuthTokensRefresh(ctx, msg.Params)
		}
	}
	return nil, &jsonrpc.Error{Code: -32601, Message: "unsupported request: " + msg.Method}
}

// reply answers a server request that a session answers later.
func (c *Client) reply(id json.RawMessage, result any, err error) error {
	return c.conn.Reply(id, result, err)
}
