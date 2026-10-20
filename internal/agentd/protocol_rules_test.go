package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/agenttest"
	"github.com/saanuregh/agent-cli-connector/internal/jsonrpc"
)

// writeLine sends a raw line to agentd without waiting for a reply.
func (c *client) writeLine(line string) {
	c.t.Helper()
	if _, err := c.in.Write([]byte(line + "\n")); err != nil {
		c.t.Fatal(err)
	}
}

// replyTo waits for the raw reply to request id.
func (c *client) replyTo(id string) map[string]json.RawMessage {
	c.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case line := <-c.lines:
			var msg map[string]json.RawMessage
			if json.Unmarshal(line, &msg) == nil && string(msg["id"]) == id {
				return msg
			}
		case <-deadline:
			c.t.Fatalf("no reply to %s", id)
			return nil
		}
	}
}

func TestLocalOnlyValuesRejected(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	c.mustCall("initialize", InitializeParams{}, nil)
	code, info := errorInfoOf(t, c.call("session/start", map[string]any{"Provider": agenttest.Name, "Config": map[string]any{"Logger": map[string]any{}}}, nil))
	if code != codeInvalidParams || !strings.Contains(info.Message, "Logger") {
		t.Errorf("Config.Logger: code %d %+v", code, info)
	}
	// agentd is still serving.
	startAgenttest(c)
	if _, err := decodeOptions[struct{ Client struct{ HTTP *http.Client } }](json.RawMessage(`{"Client":{"HTTP":{"Timeout":5}}}`)); err == nil {
		t.Error("a nested HTTP client was accepted")
	}
}

func TestPanickingMethodFailsOnlyItsRequest(t *testing.T) {
	spec := ProviderSpec{Name: "boom", New: func(json.RawMessage, ProviderBridge) (agent.Provider, error) { panic("kaboom") }}
	c := newClient(t, spec, fakeSpec(agenttest.New(echo)))
	if err := c.call("initialize", InitializeParams{Providers: map[string]ProviderInit{"boom": {}}}, nil); err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("initialize = %v", err)
	}
	c.mustCall("initialize", InitializeParams{Providers: map[string]ProviderInit{agenttest.Name: {}}}, nil)
}

func TestEmptyResultIsEmptyObject(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	s := startFake(c)
	c.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":"q1","method":"session/interrupt","params":{"Session":%q}}`, s.Session))
	if res := c.replyTo(`"q1"`); string(res["result"]) != "{}" {
		t.Errorf("session/interrupt result %s, want {}", res["result"])
	}
}

func TestParseErrorAnswered(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	c.writeLine(`this is not json`)
	res := c.replyTo("null")
	var rpcErr jsonrpc.Error
	if err := json.Unmarshal(res["error"], &rpcErr); err != nil || rpcErr.Code != codeParseError {
		t.Errorf("parse error reply %s, %v", res["error"], err)
	}
}

func TestUnknownMethodHasErrorInfo(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	code, info := errorInfoOf(t, c.call("no/such", nil, nil))
	if code != codeUnknownMethod || info.Kind != "unknown_method" {
		t.Errorf("code %d %+v", code, info)
	}
}

func TestPipelinedRequestsKeepOrder(t *testing.T) {
	p := agenttest.New(echo)
	c := newClient(t, fakeSpec(p))
	// Without ordering, session/start could run before initialize.
	c.writeLine(`{"jsonrpc":"2.0","id":"i","method":"initialize","params":{}}`)
	c.writeLine(`{"jsonrpc":"2.0","id":"st","method":"session/start","params":{"Provider":"agenttest"}}`)
	if res := c.replyTo(`"st"`); res["error"] != nil {
		t.Fatalf("pipelined session/start: %s", res["error"])
	}
	const n = 20
	for i := range n {
		c.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":"send%d","method":"session/send","params":{"Session":"s1","Input":{"Parts":[{"Type":"text","Text":"%d"}]}}}`, i, i))
	}
	for range n {
		c.next("turn/result")
	}
	inputs := p.Sessions()[0].Inputs()
	if len(inputs) != n {
		t.Fatalf("%d inputs, want %d", len(inputs), n)
	}
	for i, in := range inputs {
		if got := in.PlainText(); got != fmt.Sprint(i) {
			t.Fatalf("input %d is %q: sends ran out of order", i, got)
		}
	}
}

func TestTurnResultBeforeSessionClosed(t *testing.T) {
	p := agenttest.New(func(t *agenttest.Turn) error {
		<-t.Context().Done()
		return t.Context().Err()
	})
	c := newClient(t, fakeSpec(p))
	s := startFake(c)
	c.mustCall("session/send", SendParams{Session: s.Session, Input: agent.Text("wait")}, nil)
	c.mustCall("session/close", SessionParams{Session: s.Session}, nil)
	deadline := time.After(10 * time.Second)
	sawResult := false
	for {
		select {
		case msg := <-c.notes:
			switch msg.Method {
			case "turn/result":
				sawResult = true
			case "session/closed":
				if !sawResult {
					t.Error("session/closed came before the turn's turn/result")
				}
				return
			}
		case <-deadline:
			t.Fatal("no session/closed")
		}
	}
}

func TestErrorInfoKeepsDetails(t *testing.T) {
	pe := &agent.ProtocolError{Provider: "codex", Method: "turn/start", Code: -32001, Message: "over quota", ErrorCode: "usageLimitExceeded"}
	info := errorInfo(fmt.Errorf("send: %w", pe))
	if info.Kind != "protocol" || info.Protocol == nil || info.Protocol.ErrorCode != "usageLimitExceeded" {
		t.Errorf("protocol error info %+v", info)
	}
	ee := &agent.ExitError{Provider: "claude", Err: errors.New("exit status 1"), Stderr: "boom"}
	info = errorInfo(ee)
	data, _ := json.Marshal(info)
	if info.Kind != "exit" || info.Exit == nil || !strings.Contains(string(data), `"Stderr":"boom"`) || !strings.Contains(string(data), "exit status 1") {
		t.Errorf("exit error info %s", data)
	}
}

// logBuffer is a goroutine-safe buffer for a test logger.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestSessionLoggerTagged(t *testing.T) {
	var logs logBuffer
	p := &capturingProvider{Provider: agenttest.New(echo)}
	c := serveConfig(t, Config{Providers: []ProviderSpec{fakeSpec(p)}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	s := startFake(c)
	p.config().Logger.Info("hello from the library")
	if out := logs.String(); !strings.Contains(out, "session="+s.Session) || !strings.Contains(out, "hello from the library") {
		t.Errorf("log output %q", out)
	}
}

func TestPingAnswersWhileInitializeRuns(t *testing.T) {
	release := make(chan struct{})
	slow := ProviderSpec{Name: agenttest.Name, New: func(json.RawMessage, ProviderBridge) (agent.Provider, error) {
		<-release
		return agenttest.New(echo), nil
	}}
	c := newClient(t, slow)
	c.mustCall(pingMethod, nil, nil) // before initialize
	c.writeLine(`{"jsonrpc":"2.0","id":"i","method":"initialize","params":{}}`)
	c.mustCall(pingMethod, nil, nil) // while it blocks
	close(release)
	c.replyTo(`"i"`)
}
