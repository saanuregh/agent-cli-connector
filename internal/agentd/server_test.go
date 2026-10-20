package agentd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/agenttest"
	"github.com/saanuregh/agent-cli-connector/internal/jsonrpc"
)

// client is the test's side of an agentd connection.
type client struct {
	t      *testing.T
	conn   *jsonrpc.Conn
	in     *io.PipeWriter
	notes  chan *jsonrpc.Message
	served chan error
	// lines receives every line agentd writes, for tests that need replies
	// the connection would drop.
	lines    chan []byte
	stopOnce sync.Once
	// onRequest answers reverse requests; nil refuses them.
	onRequest func(msg *jsonrpc.Message) (any, error)
}

// newClient serves specs over pipes and returns the client side.
func newClient(t *testing.T, specs ...ProviderSpec) *client {
	t.Helper()
	return serveConfig(t, Config{Providers: specs})
}

// serveConfig serves cfg over pipes and returns the client side.
func serveConfig(t *testing.T, cfg Config) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{t: t, in: inW, notes: make(chan *jsonrpc.Message, 1024), lines: make(chan []byte, 1024), served: make(chan error, 1)}
	c.conn = jsonrpc.New(func(data []byte) error {
		_, err := inW.Write(append(data, '\n'))
		return err
	}, jsonrpc.Handler{
		Notify: func(msg *jsonrpc.Message, _ []byte) { c.notes <- msg },
		Request: func(_ context.Context, msg *jsonrpc.Message, _ []byte) (any, error) {
			if c.onRequest == nil {
				return nil, &jsonrpc.Error{Code: -32601, Message: "no handler"}
			}
			return c.onRequest(msg)
		},
	})
	go func() {
		c.served <- Serve(context.Background(), inR, outW, cfg)
		_ = outW.Close()
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(nil, 1<<24)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			if !bytes.HasPrefix(line, []byte(`{"jsonrpc":"2.0",`)) {
				t.Errorf("message without the jsonrpc member: %s", line)
			}
			select {
			case c.lines <- line:
			default:
			}
			_ = c.conn.Receive(line)
		}
		c.conn.Close(errors.New("agentd exited"))
	}()
	t.Cleanup(c.stop)
	return c
}

// stop closes agentd's input and waits for Serve to return.
func (c *client) stop() {
	c.stopOnce.Do(func() {
		_ = c.in.Close()
		select {
		case <-c.served:
		case <-time.After(15 * time.Second):
			c.t.Error("Serve did not return after its input closed")
		}
	})
}

func (c *client) call(method string, params, result any) error {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.conn.Call(ctx, method, params, result)
}

func (c *client) mustCall(method string, params, result any) {
	c.t.Helper()
	if err := c.call(method, params, result); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
}

// next returns the next notification of the given method, skipping others.
func (c *client) next(method string) *jsonrpc.Message {
	c.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-c.notes:
			if msg.Method == method {
				return msg
			}
		case <-deadline:
			c.t.Fatalf("no %s notification", method)
			return nil
		}
	}
}

// events collects session/event notifications until the turn/result.
func (c *client) untilResult() ([]EventNotification, TurnResultNotification) {
	c.t.Helper()
	var evs []EventNotification
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-c.notes:
			switch msg.Method {
			case "session/event":
				var ev EventNotification
				if err := json.Unmarshal(msg.Params, &ev); err != nil {
					c.t.Fatal(err)
				}
				evs = append(evs, ev)
			case "turn/result":
				var res TurnResultNotification
				if err := json.Unmarshal(msg.Params, &res); err != nil {
					c.t.Fatal(err)
				}
				return evs, res
			}
		case <-deadline:
			c.t.Fatal("no turn/result notification")
		}
	}
}

// fakeSpec offers p, usually agenttest or a wrapper of it, under
// agenttest's name.
func fakeSpec(p agent.Provider) ProviderSpec {
	return ProviderSpec{
		Name: agenttest.Name,
		New:  func(json.RawMessage, ProviderBridge) (agent.Provider, error) { return p, nil },
	}
}

func echo(t *agenttest.Turn) error {
	t.Text("echo: ")
	t.Text(t.Input().PlainText())
	return nil
}

// errorInfoOf returns the ErrorInfo of a JSON-RPC error.
func errorInfoOf(t *testing.T, err error) (int, ErrorInfo) {
	t.Helper()
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error %v is not a JSON-RPC error", err)
	}
	var info ErrorInfo
	if err := json.Unmarshal(rpcErr.Data, &info); err != nil {
		t.Fatalf("error data %s: %v", rpcErr.Data, err)
	}
	return rpcErr.Code, info
}

// StartParams cannot be marshaled in Go, because agent.Config holds
// functions, so tests send session/start params as maps, as a client in
// another language would.

// startFake initializes agentd and starts an agenttest session.
func startFake(c *client) StartResult {
	c.t.Helper()
	c.mustCall("initialize", InitializeParams{ProtocolVersion: ProtocolVersion}, nil)
	return startAgenttest(c)
}

// startAgenttest starts an agenttest session on an initialized agentd.
func startAgenttest(c *client) StartResult {
	c.t.Helper()
	var started StartResult
	c.mustCall("session/start", map[string]any{"Provider": agenttest.Name}, &started)
	return started
}

// askPermission asks once for a shell command and writes the answer as
// "decision:message".
func askPermission(t *agenttest.Turn) error {
	resp := t.Permission(agent.PermissionRequest{ID: "perm1", Kind: agent.PermissionCommand, ToolName: "bash", Command: "ls"})
	t.Text(string(resp.Decision) + ":" + resp.Message)
	return nil
}

func TestInitializeAndRun(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	var init InitializeResult
	c.mustCall("initialize", InitializeParams{ProtocolVersion: ProtocolVersion}, &init)
	if init.ProtocolVersion != ProtocolVersion || len(init.Providers) != 1 || init.Providers[0].Name != agenttest.Name || !init.Providers[0].Capabilities.Streaming {
		t.Fatalf("initialize = %+v", init)
	}
	var started StartResult
	c.mustCall("session/start", map[string]any{"Provider": agenttest.Name, "Config": map[string]any{"Model": "m1"}}, &started)
	if started.Session != "s1" || started.Info.Model != "m1" {
		t.Fatalf("start = %+v", started)
	}
	var sent SendResult
	c.mustCall("session/send", SendParams{Session: started.Session, Input: agent.Text("hi")}, &sent)
	if sent.TurnID == "" {
		t.Fatal("send returned no turn ID")
	}
	evs, res := c.untilResult()
	if res.Session != "s1" || res.TurnID != sent.TurnID || res.Result == nil || res.Result.Text != "echo: hi" || res.Result.Status != agent.TurnSucceeded {
		t.Fatalf("turn/result = %+v", res)
	}
	var types []string
	for i, ev := range evs {
		if ev.Seq != int64(i+1) || ev.Session != "s1" {
			t.Errorf("event %d: seq %d session %q", i, ev.Seq, ev.Session)
		}
		types = append(types, ev.Type)
	}
	joined := strings.Join(types, ",")
	if !strings.HasPrefix(joined, "SessionStarted,") || !strings.Contains(joined, "TextDelta") {
		t.Errorf("event types %s", joined)
	}
	var info agent.SessionInfo
	c.mustCall("session/info", SessionParams{Session: "s1"}, &info)
	if info.ID != started.Info.ID {
		t.Errorf("info ID %q, want %q", info.ID, started.Info.ID)
	}
	c.mustCall("session/close", SessionParams{Session: "s1"}, nil)
	var closed SessionClosedNotification
	if err := json.Unmarshal(c.next("session/closed").Params, &closed); err != nil || closed.Session != "s1" || closed.Error != nil {
		t.Errorf("session/closed = %+v, %v", closed, err)
	}
	if err := c.call("session/info", SessionParams{Session: "s1"}, nil); err == nil {
		t.Error("a closed session is still served")
	}
}

func TestTurnCompletedEventCarriesResult(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	s := startFake(c)
	c.mustCall("session/send", SendParams{Session: s.Session, Input: agent.Text("x")}, nil)
	evs, _ := c.untilResult()
	for _, ev := range evs {
		if ev.Type != "TurnCompleted" {
			continue
		}
		var tc struct{ Result struct{ Text string } }
		if err := json.Unmarshal(ev.Event, &tc); err != nil || tc.Result.Text != "echo: x" {
			t.Errorf("TurnCompleted = %s, %v", ev.Event, err)
		}
		return
	}
	t.Error("no TurnCompleted event before turn/result")
}

func TestUnknownSession(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	c.mustCall("initialize", InitializeParams{}, nil)
	code, info := errorInfoOf(t, c.call("session/info", SessionParams{Session: "s9"}, nil))
	if code != codeLibrary || info.Kind != "not_found" {
		t.Errorf("code %d info %+v", code, info)
	}
}

func TestInitializeRules(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	if _, info := errorInfoOf(t, c.call("session/info", SessionParams{Session: "s1"}, nil)); info.Kind != "not_initialized" {
		t.Errorf("before initialize: %+v", info)
	}
	if _, info := errorInfoOf(t, c.call("initialize", InitializeParams{ProtocolVersion: 99}, nil)); info.Kind != "invalid_params" {
		t.Errorf("wrong version: %+v", info)
	}
	if _, info := errorInfoOf(t, c.call("initialize", InitializeParams{Providers: map[string]ProviderInit{"nope": {}}}, nil)); info.Kind != "invalid_params" {
		t.Errorf("unknown provider: %+v", info)
	}
	c.mustCall("initialize", InitializeParams{}, nil)
	if _, info := errorInfoOf(t, c.call("initialize", InitializeParams{}, nil)); info.Kind != "invalid_params" {
		t.Errorf("second initialize: %+v", info)
	}
	code, info := errorInfoOf(t, c.call("session/start", json.RawMessage(`{"Provider":"agenttest","Bogus":1}`), nil))
	if code != codeInvalidParams || info.Kind != "invalid_params" {
		t.Errorf("unknown field: code %d %+v", code, info)
	}
	var rpcErr *jsonrpc.Error
	if err := c.call("no/such", nil, nil); !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Errorf("unknown method: %v", err)
	}
}

func TestStdinCloseClosesSessions(t *testing.T) {
	p := agenttest.New(func(t *agenttest.Turn) error {
		<-t.Context().Done()
		return t.Context().Err()
	})
	c := newClient(t, fakeSpec(p))
	s := startFake(c)
	c.mustCall("session/send", SendParams{Session: s.Session, Input: agent.Text("wait")}, nil)
	c.stop()
	for _, sess := range p.Sessions() {
		select {
		case <-sess.Done():
		default:
			t.Error("session still open after agentd's input closed")
		}
	}
}

func TestTurnResultError(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(func(*agenttest.Turn) error { return errors.New("model fell over") })))
	s := startFake(c)
	c.mustCall("session/send", SendParams{Session: s.Session, Input: agent.Text("x")}, nil)
	_, res := c.untilResult()
	if res.Result == nil || res.Result.Status != agent.TurnFailed || res.Result.Err == nil || !strings.Contains(res.Result.Err.Message, "model fell over") {
		t.Fatalf("turn/result = %+v", res.Result)
	}
	if res.Result.Err.Turn == nil {
		t.Errorf("turn error details missing: %+v", res.Result.Err)
	}
}

func TestRawOmittedUnlessAsked(t *testing.T) {
	script := func(t *agenttest.Turn) error {
		t.Emit(&agent.RawEvent{Type: "native/thing", EventMeta: agent.EventMeta{Raw: json.RawMessage(`{"payload":1}`)}})
		t.Emit(&agent.TextDelta{EventMeta: agent.EventMeta{Raw: json.RawMessage(`{"native":true}`)}, ItemID: "i1",
			Delta: "hi"})
		return nil
	}
	for _, includeRaw := range []bool{false, true} {
		c := newClient(t, fakeSpec(agenttest.New(script)))
		c.mustCall("initialize", InitializeParams{IncludeRaw: includeRaw}, nil)
		var s StartResult
		c.mustCall("session/start", map[string]any{"Provider": agenttest.Name}, &s)
		c.mustCall("session/send", SendParams{Session: s.Session, Input: agent.Text("x")}, nil)
		evs, _ := c.untilResult()
		seen := 0
		for _, ev := range evs {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(ev.Event, &fields); err != nil {
				t.Fatal(err)
			}
			_, hasRaw := fields["Raw"]
			switch ev.Type {
			case "RawEvent":
				seen++
				if !hasRaw {
					t.Errorf("includeRaw=%v: RawEvent lost its payload", includeRaw)
				}
			case "TextDelta":
				seen++
				if hasRaw != includeRaw {
					t.Errorf("includeRaw=%v: TextDelta has Raw = %v", includeRaw, hasRaw)
				}
			}
		}
		if seen != 2 {
			t.Errorf("includeRaw=%v: saw %d of the 2 events", includeRaw, seen)
		}
		c.stop()
	}
}
