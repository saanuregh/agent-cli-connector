// Package agent is the Go binding of agent-cli-connector: one API over
// coding-agent CLIs (Claude Code, Codex, Pi).
//
//	client, err := agent.Open(ctx)
//	sess, err := client.Start(ctx, "claude", agent.Config{Cwd: dir})
//	res, err := sess.Run(ctx, agent.Text("Add a test for parseConfig"))
//
// A [Client] is one connection to the native library, loaded at run time
// without cgo. It offers providers ([Client.Check], [Client.ListModels],
// ...) and starts [Session] values. A session streams [Event] values on
// [Session.Events] and runs turns ([Session.Send] returns a [Turn]).
// Permission, question, and elicitation requests arrive as events and are
// answered with [Session.Respond] and its helpers, or by [Handlers]. Host
// tools ([NewTool]) are Go functions the model can call.
//
// # Native library
//
// The first [Open] finds the shared library, in this order:
//
//  1. the path in AGENT_CLI_CONNECTOR_LIB;
//  2. the user cache directory, under
//     agent-cli-connector/<LibraryVersion>/<platform>/;
//  3. a download of the GitHub release v<LibraryVersion> into that cache,
//     checked against the release's SHA256SUMS.
//
// AGENT_CLI_CONNECTOR_NO_DOWNLOAD=1 turns the download off, and
// AGENT_CLI_CONNECTOR_RELEASE_URL replaces the release URL. To use a
// library built from this repository:
//
//	zig build -Doptimize=ReleaseFast
//	export AGENT_CLI_CONNECTOR_LIB=$PWD/zig-out/lib/libagent-cli-connector.so
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
)

const protocolVersion = 1

// Client is one connection to the library: its providers and sessions.
// All methods are safe for concurrent use.
type Client struct {
	conn *connection
	// connMu guards conn against use after destroy.
	connMu    sync.RWMutex
	destroyed bool

	// ctx ends when the client closes; tool and handler calls run under it.
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	nextID   int64
	pending  map[int64]*pendingCall
	sessions map[string]*Session
	// toolCancels stops running tool calls, by session handle and call ID.
	toolCancels map[[2]string]context.CancelFunc

	readerDone chan struct{}
	closeOnce  sync.Once
	init       InitializeResult

	// idPrefix and lastID make the session handles and turn IDs this
	// client chooses, so their state exists before the library uses them.
	idPrefix string
	lastID   atomic.Int64
}

type pendingCall struct {
	method string
	reply  chan callReply
}

type callReply struct {
	result json.RawMessage
	err    error
}

// Option configures Open.
type Option func(*openOptions)

type openOptions struct {
	providers map[string]any
}

// WithProviders selects the providers to offer, by name, with their
// options (any JSON value; nil uses the defaults). Without it every real
// provider is offered with default options. The "fake" provider is a
// scripted harness for tests.
func WithProviders(providers map[string]any) Option {
	return func(o *openOptions) { o.providers = providers }
}

// Open connects to the library and initializes the providers.
func Open(ctx context.Context, opts ...Option) (*Client, error) {
	var o openOptions
	for _, opt := range opts {
		opt(&o)
	}
	params := InitializeParams{ProtocolVersion: protocolVersion}
	if o.providers != nil {
		params.Providers = make(map[string]json.RawMessage, len(o.providers))
		for name, v := range o.providers {
			raw, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("agent: options of provider %q: %w", name, err)
			}
			params.Providers[name] = raw
		}
	}

	var prefix [8]byte
	if _, err := rand.Read(prefix[:]); err != nil {
		return nil, fmt.Errorf("agent: random ID prefix: %w", err)
	}
	conn, err := openConnection()
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:        conn,
		pending:     map[int64]*pendingCall{},
		sessions:    map[string]*Session{},
		toolCancels: map[[2]string]context.CancelFunc{},
		readerDone:  make(chan struct{}),
		idPrefix:    hex.EncodeToString(prefix[:]),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	go c.read()
	if err := c.call(ctx, MethodInitialize, params, &c.init); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Providers lists the offered providers with their capabilities.
func (c *Client) Providers() []ProviderInfo { return c.init.Providers }

// Version returns the library version reported by initialize.
func (c *Client) Version() string { return c.init.LibraryVersion }

// Close closes every session and the connection. It is idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.conn.shutdown()
		<-c.readerDone
		c.cancel()
		c.connMu.Lock()
		c.destroyed = true
		c.conn.destroy()
		c.connMu.Unlock()
	})
	return nil
}

// ---------------------------------------------------------------- sessions

// SessionOption configures Start and Fork.
type SessionOption func(*sessionOptions)

type sessionOptions struct {
	tools       []Tool
	toolsSet    bool
	handlers    Handlers
	handlersSet bool
}

// WithTools registers host tools for the session. Their definitions are
// added to Config.Tools.
func WithTools(tools ...Tool) SessionOption {
	return func(o *sessionOptions) { o.tools, o.toolsSet = append(o.tools, tools...), true }
}

// WithHandlers answers requests with callbacks. The requests are still
// delivered as events; Session.Handled reports which ones a handler
// answers.
func WithHandlers(h Handlers) SessionOption {
	return func(o *sessionOptions) { o.handlers, o.handlersSet = h, true }
}

// Start starts a new or resumed session on provider. It returns once the
// harness is ready for input.
func (c *Client) Start(ctx context.Context, provider string, cfg Config, opts ...SessionOption) (*Session, error) {
	var o sessionOptions
	for _, opt := range opts {
		opt(&o)
	}
	return c.startSession(ctx, MethodSessionStart, func(cfg Config, handle string) any {
		return StartParams{Provider: provider, Config: cfg, Handle: handle}
	}, cfg, o)
}

// startSession registers the session under a handle it chooses before
// asking the library for it, so every notification finds the session.
func (c *Client) startSession(ctx context.Context, method string, params func(cfg Config, handle string) any, cfg Config, o sessionOptions) (*Session, error) {
	cfg.Tools = append([]ToolDef(nil), cfg.Tools...)
	for _, t := range o.tools {
		cfg.Tools = append(cfg.Tools, t.ToolDef)
	}
	sess := newSession(c, c.newID("s"), o)
	c.mu.Lock()
	c.sessions[sess.handle] = sess
	c.mu.Unlock()
	var res StartResult
	err := c.callWith(ctx, method, params(cfg, sess.handle), &res, &pendingCall{})
	if err == nil && res.Session != sess.handle {
		err = &Error{ErrorInfo: ErrorInfo{Kind: ErrorKindProtocol, Message: "invalid start result"}, Method: method}
	}
	if err != nil {
		c.unregister(sess.handle)
		// The library may have started the session before the caller gave
		// up; nobody owns it now. Closing an unknown handle fails harmlessly.
		go c.call(context.Background(), MethodSessionClose, SessionParams{Session: sess.handle}, nil)
		return nil, err
	}
	sess.started(res)
	return sess, nil
}

// newUUID returns a random RFC 4122 version 4 UUID. Claude requires turn
// IDs to be UUIDs.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error.
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// newID returns a session handle unique to this client.
func (c *Client) newID(kind string) string {
	return c.idPrefix + "-" + kind + strconv.FormatInt(c.lastID.Add(1), 10)
}

func (c *Client) unregister(handle string) {
	c.mu.Lock()
	delete(c.sessions, handle)
	c.mu.Unlock()
}

// ---------------------------------------------------------------- transport

// call sends a request and decodes its result into result (nil discards it).
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	return c.callWith(ctx, method, params, result, &pendingCall{})
}

func (c *Client) callWith(ctx context.Context, method string, params, result any, p *pendingCall) error {
	p.method = method
	p.reply = make(chan callReply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return closedError(method)
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = p
	c.mu.Unlock()

	msg, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
	if err == nil {
		err = c.send(msg)
	}
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case r := <-p.reply:
		if r.err != nil {
			return r.err
		}
		if result != nil && len(r.result) > 0 && string(r.result) != "null" {
			if err := json.Unmarshal(r.result, result); err != nil {
				return &Error{ErrorInfo: ErrorInfo{Kind: ErrorKindProtocol, Message: "cannot decode result: " + err.Error()}, Method: method}
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) send(msg []byte) error {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	if c.destroyed {
		return closedError("")
	}
	return c.conn.send(msg)
}

// read is the only goroutine that calls agent_recv_batch. It never blocks on a
// consumer: events are queued per session, and tools and handlers run on
// their own goroutines.
func (c *Client) read() {
	defer close(c.readerDone)
	for {
		msgs, ok := c.conn.recv(1000)
		if !ok {
			break
		}
		for msg := range bytes.SplitSeq(msgs, []byte{'\n'}) {
			if len(msg) > 0 {
				c.dispatch(msg)
			}
		}
	}

	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = map[int64]*pendingCall{}
	sessions := make([]*Session, 0, len(c.sessions))
	for _, s := range c.sessions {
		sessions = append(sessions, s)
	}
	c.mu.Unlock()
	for _, p := range pending {
		p.reply <- callReply{err: closedError(p.method)}
	}
	for _, s := range sessions {
		s.finish(&ErrorInfo{Kind: ErrorKindClosed, Message: "client closed"})
	}
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int64      `json:"code"`
		Message string     `json:"message"`
		Data    *ErrorInfo `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

func (c *Client) dispatch(raw json.RawMessage) {
	var msg rpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	switch msg.Method {
	case "":
		c.answer(msg)
	case NotificationSessionEvent:
		var n struct {
			Session string `json:"session"`
			Event   Event  `json:"event"`
		}
		if json.Unmarshal(msg.Params, &n) != nil {
			return
		}
		if s := c.session(n.Session); s != nil {
			s.deliver(n.Event)
		}
	case NotificationSessionClosed:
		var n ClosedNotification
		if json.Unmarshal(msg.Params, &n) != nil {
			return
		}
		if s := c.session(n.Session); s != nil {
			c.unregister(n.Session)
			s.finish(n.Error)
		}
	case NotificationToolCancel:
		var p ToolCancelParams
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		c.mu.Lock()
		cancel := c.toolCancels[[2]string{p.Session, p.CallID}]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	case RequestToolCall:
		var p ToolCallParams
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		s := c.session(p.Session)
		go c.runToolCall(msg.ID, p.Session, s, p.Call)
	default:
		if len(msg.ID) > 0 {
			c.reply(msg.ID, nil, &ErrorInfo{Kind: ErrorKindUnsupported, Message: "unknown method " + msg.Method})
		}
	}
}

func (c *Client) session(handle string) *Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[handle]
}

func (c *Client) answer(msg rpcMessage) {
	var id int64
	if json.Unmarshal(msg.ID, &id) != nil {
		return
	}
	c.mu.Lock()
	p := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if p == nil {
		return
	}
	if msg.Error != nil {
		info := ErrorInfo{Kind: ErrorKindInternal, Message: msg.Error.Message}
		if msg.Error.Data != nil {
			info = *msg.Error.Data
		}
		p.reply <- callReply{err: &Error{ErrorInfo: info, Method: p.method}}
		return
	}
	p.reply <- callReply{result: msg.Result}
}

// runToolCall runs a tool call. Its context is cancelled when the library
// sends tool/cancel, because the turn was interrupted or the session closed.
func (c *Client) runToolCall(id json.RawMessage, handle string, s *Session, call ToolCallRequest) {
	var tool *Tool
	if s != nil {
		tool = s.tools[call.Name]
	}
	ctx, cancel := context.WithCancel(c.ctx)
	key := [2]string{handle, call.ID}
	c.mu.Lock()
	c.toolCancels[key] = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.toolCancels, key)
		c.mu.Unlock()
		cancel()
	}()
	res := runTool(ctx, tool, call)
	c.reply(id, res, nil)
}

// reply answers a request from the library.
func (c *Client) reply(id json.RawMessage, result any, errInfo *ErrorInfo) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if errInfo != nil {
		msg["error"] = map[string]any{"code": -32000, "message": errInfo.Message, "data": errInfo}
	} else {
		msg["result"] = result
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_ = c.send(data)
}
