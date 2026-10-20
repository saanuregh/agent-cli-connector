// Package agentd serves the library over JSON-RPC 2.0, one message per
// line, so programs in any language can drive the harnesses through a
// child process. cmd/agentd runs it on standard input and output.
package agentd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/jsonrpc"
)

// closeTimeout bounds closing the sessions when the client goes away.
const closeTimeout = 10 * time.Second

// method handles one request.
type method func(ctx context.Context, params json.RawMessage) (any, error)

type server struct {
	ctx    context.Context
	logger *slog.Logger // nil discards
	conn   *jsonrpc.Conn
	specs  map[string]ProviderSpec

	writeMu sync.Mutex
	w       io.Writer

	methods map[string]method
	// methodNames are the keys of methods, sorted.
	methodNames []string
	// includeRaw keeps native Raw messages in what agentd sends.
	includeRaw atomic.Bool
	// inflight counts requests received and not yet answered.
	inflight sync.WaitGroup

	// Request ordering; see schedule.
	orderMu    sync.Mutex
	lastBySess map[string]chan struct{}
	// initGate is closed when the latest initialize received ends;
	// nil before the first one.
	initGate chan struct{}

	mu          sync.Mutex
	initialized bool
	providers   map[string]agent.Provider
	sessions    map[string]*hosted
	lastSession int
	closing     bool

	pumps sync.WaitGroup
}

// Config configures Serve.
type Config struct {
	// Providers are the providers agentd can offer, one per spec.
	Providers []ProviderSpec
	// Logger receives the library's diagnostics, tagged with the session
	// handle. Nil discards them.
	Logger *slog.Logger
}

// Serve runs the protocol on r and w until r ends or ctx is done, then
// closes every session and provider.
func Serve(ctx context.Context, r io.Reader, w io.Writer, cfg Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &server{
		ctx:        ctx,
		logger:     cfg.Logger,
		specs:      map[string]ProviderSpec{},
		w:          w,
		sessions:   map[string]*hosted{},
		lastBySess: map[string]chan struct{}{},
	}
	for _, spec := range cfg.Providers {
		s.specs[spec.Name] = spec
	}
	s.methods = s.methodTable()
	s.methodNames = slices.Sorted(maps.Keys(s.methods))
	s.conn = jsonrpc.NewStrict(s.write, jsonrpc.Handler{Request: s.handle, Schedule: s.schedule, CancelMethod: cancelMethod})

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				readErr <- err
				return
			}
		}
	}()

	var err error
loop:
	for {
		select {
		case line := <-lines:
			if rerr := s.conn.Receive(line); rerr != nil {
				s.parseError(rerr)
			}
		case err = <-readErr:
			break loop
		case <-ctx.Done():
			break loop
		}
	}
	s.shutdown()
	return err
}

// write sends one message as a line. Messages from different goroutines
// never interleave.
func (s *server) write(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.w.Write(append(data, '\n'))
	return err
}

// notify sends a notification, dropping native Raw messages from params
// unless the client asked for them.
func (s *server) notify(method string, params any) error {
	data, err := s.encode(params)
	if err != nil {
		return err
	}
	return s.conn.Notify(method, data)
}

// parseError answers a line that is not JSON, as JSON-RPC 2.0 asks: an
// error response with a null ID.
func (s *server) parseError(err error) {
	data, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		Error   jsonrpc.Error   `json:"error"`
		ID      json.RawMessage `json:"id"`
	}{
		JSONRPC: "2.0",
		Error:   jsonrpc.Error{Code: codeParseError, Message: "parse error: " + err.Error(), Data: mustJSON(ErrorInfo{Message: err.Error(), Kind: "parse_error"})},
		ID:      json.RawMessage("null"),
	})
	_ = s.write(data)
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// handle runs a request on its own context, which ends when the client
// cancels it or Serve returns; at shutdown, requests already received get
// closeTimeout to finish first. A panicking method fails its request
// instead of agentd.
func (s *server) handle(ctx context.Context, msg *jsonrpc.Message, _ []byte) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, rpcError(fmt.Errorf("agentd: %s failed: %v", msg.Method, r))
		}
	}()
	m, ok := s.methods[msg.Method]
	if !ok {
		return nil, &jsonrpc.Error{Code: codeUnknownMethod, Message: "method not found: " + msg.Method,
			Data: mustJSON(ErrorInfo{Message: "method not found: " + msg.Method, Kind: "unknown_method"})}
	}
	if ctx.Err() != nil {
		return nil, rpcError(newError("cancelled", "request cancelled"))
	}
	s.mu.Lock()
	ready := s.initialized
	s.mu.Unlock()
	if !ready && msg.Method != "initialize" && msg.Method != pingMethod {
		return nil, rpcError(newError("not_initialized", "initialize must be the first request"))
	}
	res, err := m(ctx, msg.Params)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			err = newError("cancelled", err.Error())
		}
		return nil, rpcError(err)
	}
	if res == nil {
		return nil, nil // the connection sends an empty object
	}
	data, err := s.encode(res)
	if err != nil {
		return nil, rpcError(fmt.Errorf("encode result: %w", err))
	}
	return data, nil
}

// pingMethod is the health check: it answers at once, even before
// initialize and while one runs.
const pingMethod = "$/ping"

// unordered lists the methods that run as soon as they arrive: answers to
// the harness's requests and controls that stop work. A long method, such
// as compacting, may be waiting on one of them.
var unordered = map[string]bool{
	"request/respond":       true,
	"request/cancel":        true,
	"session/interrupt":     true,
	"session/interruptTurn": true,
	"session/close":         true,
}

// schedule starts a request once the requests it is ordered after are
// done. It runs on the read goroutine, in arrival order. Requests run
// concurrently, except that every request received after initialize waits
// for it, and requests naming the same session run one at a time in
// arrival order unless they are unordered. ctx ends when the client
// cancels the request; Serve ending cancels it too.
func (s *server) schedule(ctx context.Context, msg *jsonrpc.Message, serve func(context.Context)) {
	s.inflight.Add(1)
	after, done := s.order(msg)
	ctx, stop := context.WithCancel(ctx)
	unhook := context.AfterFunc(s.ctx, stop)
	go func() {
		defer s.inflight.Done()
		defer done()
		defer stop()
		defer unhook()
		waitAll(after, ctx.Done())
		serve(ctx)
		// A request cancelled while queued has answered already, but it
		// releases the requests after it only once those before it are
		// done, so cancelling never lets a later request jump the queue.
		waitAll(after, s.ctx.Done())
	}()
}

// waitAll waits until every channel is closed or stop is.
func waitAll(chans []chan struct{}, stop <-chan struct{}) {
	for _, ch := range chans {
		select {
		case <-ch:
		case <-stop:
			return
		}
	}
}

// order returns what msg waits for and the function that releases the
// requests waiting for it.
func (s *server) order(msg *jsonrpc.Message) (after []chan struct{}, done func()) {
	if msg.Method == pingMethod {
		return nil, func() {} // a health check waits for nothing
	}
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	if s.initGate != nil {
		after = append(after, s.initGate)
	}
	if msg.Method == "initialize" {
		// Each initialize waits for the one before it and opens a new
		// gate, so a rejected second initialize cannot let requests run
		// before the first finishes.
		gate := make(chan struct{})
		s.initGate = gate
		return after, func() { close(gate) }
	}
	// Only session/* and request/* requests name a session.
	if unordered[msg.Method] || !strings.HasPrefix(msg.Method, "session/") && !strings.HasPrefix(msg.Method, "request/") {
		return after, func() {}
	}
	var params struct{ Session string }
	_ = json.Unmarshal(msg.Params, &params)
	sess := params.Session
	if sess == "" {
		return after, func() {}
	}
	if prev, ok := s.lastBySess[sess]; ok {
		after = append(after, prev)
	}
	release := make(chan struct{})
	s.lastBySess[sess] = release
	return after, func() {
		close(release)
		s.orderMu.Lock()
		if s.lastBySess[sess] == release {
			delete(s.lastBySess, sess)
		}
		s.orderMu.Unlock()
	}
}

// handler adapts a typed method, decoding its params strictly.
func handler[P any](f func(ctx context.Context, p P) (any, error)) method {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		p, err := decodeStrict[P](raw)
		if err != nil {
			return nil, newError("invalid_params", "params: "+err.Error())
		}
		return f(ctx, p)
	}
}

// initialize builds the providers. It does so without holding s.mu,
// because a provider may take a while to build and requests that do not
// wait for initialize must not block on it. Initialize requests never
// overlap: each waits for the one before it (see order), and one cancelled
// while waiting does not run.
func (s *server) initialize(_ context.Context, p InitializeParams) (any, error) {
	if p.ProtocolVersion != 0 && p.ProtocolVersion != ProtocolVersion {
		return nil, newError("invalid_params", fmt.Sprintf("protocol version %d is not supported; agentd speaks %d", p.ProtocolVersion, ProtocolVersion))
	}
	s.mu.Lock()
	done := s.initialized
	s.mu.Unlock()
	if done {
		return nil, newError("invalid_params", "already initialized")
	}
	inits := p.Providers
	if len(inits) == 0 {
		inits = map[string]ProviderInit{}
		for name := range s.specs {
			inits[name] = ProviderInit{}
		}
	}
	providers := map[string]agent.Provider{}
	for _, name := range slices.Sorted(maps.Keys(inits)) {
		spec, ok := s.specs[name]
		if !ok {
			closeProviders(providers)
			return nil, newError("invalid_params", "unknown provider "+strconv.Quote(name))
		}
		init := inits[name]
		sp, err := init.Spawner.spawner()
		if err != nil {
			closeProviders(providers)
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		bridge := ProviderBridge{Init: init, Call: s.call, Logger: s.logger, Spawner: sp}
		if init.WireTap {
			bridge.WireTap = s.wireTap("", name)
		}
		prov, err := spec.New(init.Options, bridge)
		if err != nil {
			closeProviders(providers)
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		providers[name] = prov
	}
	s.includeRaw.Store(p.IncludeRaw)
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		closeProviders(providers)
		return nil, agent.ErrClosed
	}
	s.providers = providers
	s.initialized = true
	s.mu.Unlock()
	res := InitializeResult{ProtocolVersion: ProtocolVersion}
	for _, name := range slices.Sorted(maps.Keys(providers)) {
		res.Providers = append(res.Providers, ProviderInfo{Name: name, Capabilities: providers[name].Capabilities(),
			Methods: s.supported(providers[name], "provider/")})
	}
	return res, nil
}

// wireTap returns a WireTap that sends wire/tap notifications.
func (s *server) wireTap(session, provider string) func(agent.WireDirection, []byte) {
	return func(dir agent.WireDirection, line []byte) {
		// The notification has no Raw field to drop, so it skips notify's
		// second pass; wire lines are the largest messages agentd sends.
		_ = s.conn.Notify("wire/tap", WireTapNotification{Session: session, Provider: provider, Direction: dir.String(), Line: string(line)})
	}
}

func (s *server) provider(name string) (agent.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.providers[name]
	if !ok {
		return nil, newError("not_found", "unknown provider "+strconv.Quote(name))
	}
	return p, nil
}

// shutdown lets requests already received finish, then closes every
// session in parallel and the providers, and fails reverse calls still
// waiting on the client. Each wait is bounded by closeTimeout.
func (s *server) shutdown() {
	waitTimeout(&s.inflight, closeTimeout)
	s.mu.Lock()
	s.closing = true
	hosted := slices.Collect(maps.Values(s.sessions))
	providers := s.providers
	s.mu.Unlock()

	s.conn.Close(errors.New("agentd: client disconnected"))
	var wg sync.WaitGroup
	for _, h := range hosted {
		wg.Go(func() { _ = h.sess.Close() })
	}
	wg.Go(s.pumps.Wait)
	waitTimeout(&wg, closeTimeout)
	closeProviders(providers)
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

func closeProviders(providers map[string]agent.Provider) {
	for _, p := range providers {
		if c, ok := p.(io.Closer); ok {
			_ = c.Close()
		}
	}
}
