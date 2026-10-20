package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/agenttest"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/jsonrpc"
)

// capturingProvider is agenttest that keeps the last session config, so a
// script can call the tool handlers and wire tap agentd installed.
type capturingProvider struct {
	*agenttest.Provider
	mu  sync.Mutex
	cfg agent.Config
}

func (p *capturingProvider) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	if cfg.WireTap != nil {
		cfg.WireTap(agent.WireSend, []byte(`{"native":"hello"}`))
	}
	return p.Provider.Start(ctx, cfg)
}

func (p *capturingProvider) config() agent.Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

func TestPermissionHandlerRoundTrip(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(askPermission)))
	got := make(chan PermissionCall, 1)
	c.onRequest = func(msg *jsonrpc.Message) (any, error) {
		if msg.Method != "handler/permission" {
			return nil, errors.New("unexpected " + msg.Method)
		}
		var call PermissionCall
		if err := json.Unmarshal(msg.Params, &call); err != nil {
			return nil, err
		}
		got <- call
		return agent.PermissionResponse{Decision: agent.DecisionAllow, Message: "ok"}, nil
	}
	c.mustCall("initialize", InitializeParams{}, nil)
	var s StartResult
	c.mustCall("session/start", map[string]any{"Provider": agenttest.Name, "Handlers": map[string]any{"Permission": true}}, &s)
	c.mustCall("session/send", SendParams{Session: s.Session, Input: agent.Text("go")}, nil)
	evs, res := c.untilResult()
	if res.Result == nil || res.Result.Text != "allow:ok" {
		t.Fatalf("turn/result = %+v", res.Result)
	}
	call := <-got
	if call.Session != s.Session || call.Request == nil || call.Request.Command != "ls" {
		t.Errorf("handler/permission params %+v", call)
	}
	for _, ev := range evs {
		if ev.Type == "PermissionRequest" {
			t.Error("a handled request was also sent as an event")
		}
	}
}

func TestUndeclaredRequestIsEvent(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(askPermission)))
	c.onRequest = func(msg *jsonrpc.Message) (any, error) {
		t.Errorf("unexpected reverse request %s", msg.Method)
		return nil, errors.New("no")
	}
	s := startFake(c)
	c.mustCall("session/send", SendParams{Session: s.Session, Input: agent.Text("go")}, nil)
	for {
		var ev EventNotification
		if err := json.Unmarshal(c.next("session/event").Params, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type == "PermissionRequest" {
			var req agent.PermissionRequest
			if err := json.Unmarshal(ev.Event, &req); err != nil || req.Command != "ls" || req.ID == "" {
				t.Errorf("PermissionRequest event %s, %v", ev.Event, err)
			}
			break
		}
	}
	c.mustCall("session/close", SessionParams{Session: s.Session}, nil)
}

func TestReverseCallFailureDenies(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(askPermission)))
	c.onRequest = func(*jsonrpc.Message) (any, error) { return nil, errors.New("client is confused") }
	c.mustCall("initialize", InitializeParams{}, nil)
	var s StartResult
	c.mustCall("session/start", map[string]any{"Provider": agenttest.Name, "Handlers": map[string]any{"Permission": true}}, &s)
	c.mustCall("session/send", SendParams{Session: s.Session, Input: agent.Text("go")}, nil)
	_, res := c.untilResult()
	if res.Result == nil || !strings.HasPrefix(res.Result.Text, "deny:") || !strings.Contains(res.Result.Text, "client is confused") {
		t.Fatalf("turn/result = %+v", res.Result)
	}
}

func TestWithdrawnRequestCancelsCall(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(askPermission)))
	asked := make(chan json.RawMessage, 1)
	release := make(chan struct{})
	defer close(release)
	c.onRequest = func(msg *jsonrpc.Message) (any, error) {
		asked <- msg.ID
		<-release
		return agent.PermissionResponse{Decision: agent.DecisionAllow}, nil
	}
	c.mustCall("initialize", InitializeParams{}, nil)
	var s StartResult
	c.mustCall("session/start", map[string]any{"Provider": agenttest.Name, "Handlers": map[string]any{"Permission": true}}, &s)
	c.mustCall("session/send", SendParams{Session: s.Session, Input: agent.Text("go")}, nil)
	var id json.RawMessage
	select {
	case id = <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("no handler/permission request")
	}
	c.mustCall("session/interrupt", SessionParams{Session: s.Session}, nil)
	var cancel struct{ ID json.RawMessage }
	if err := json.Unmarshal(c.next(cancelMethod).Params, &cancel); err != nil || string(cancel.ID) != string(id) {
		t.Errorf("%s params id %s, want %s (%v)", cancelMethod, cancel.ID, id, err)
	}
	_, res := c.untilResult()
	if res.Result == nil || res.Result.Status != agent.TurnInterrupted {
		t.Errorf("turn/result = %+v", res.Result)
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	var p *capturingProvider
	p = &capturingProvider{Provider: agenttest.New(func(t *agenttest.Turn) error {
		tool := p.config().Tools[0]
		res, err := tool.Handler(t.Context(), agent.ToolCallRequest{ID: "call1", Name: tool.Name, Arguments: json.RawMessage(`{"city":"Oslo"}`)})
		if err != nil {
			return err
		}
		t.Text(res.Content[0].Text)
		return nil
	})}
	c := newClient(t, fakeSpec(p))
	c.onRequest = func(msg *jsonrpc.Message) (any, error) {
		var call ToolCall
		if err := json.Unmarshal(msg.Params, &call); err != nil {
			return nil, err
		}
		if msg.Method != "tool/call" || call.Tool != "weather" || string(call.Call.Arguments) != `{"city":"Oslo"}` {
			return nil, errors.New("unexpected call")
		}
		return agent.TextResult("Sunny in Oslo"), nil
	}
	c.mustCall("initialize", InitializeParams{}, nil)
	var s StartResult
	c.mustCall("session/start", map[string]any{"Provider": agenttest.Name, "Config": map[string]any{
		"Tools": []map[string]any{{"Name": "weather", "Description": "Weather for a city.", "ReadOnly": true}},
	}}, &s)
	c.mustCall("session/send", SendParams{Session: s.Session, Input: agent.Text("go")}, nil)
	_, res := c.untilResult()
	if res.Result == nil || res.Result.Text != "Sunny in Oslo" {
		t.Fatalf("turn/result = %+v", res.Result)
	}
	if tool := p.config().Tools[0]; !tool.ReadOnly || tool.Description != "Weather for a city." {
		t.Errorf("tool decoded as %+v", tool)
	}
}

func TestWireTapNotifies(t *testing.T) {
	p := &capturingProvider{Provider: agenttest.New(echo)}
	c := newClient(t, fakeSpec(p))
	c.mustCall("initialize", InitializeParams{}, nil)
	var s StartResult
	c.mustCall("session/start", map[string]any{"Provider": agenttest.Name, "WireTap": true}, &s)
	var tap WireTapNotification
	if err := json.Unmarshal(c.next("wire/tap").Params, &tap); err != nil {
		t.Fatal(err)
	}
	if tap.Session != s.Session || tap.Provider != agenttest.Name || tap.Direction != agent.WireSend.String() || tap.Line != `{"native":"hello"}` {
		t.Errorf("wire/tap = %+v", tap)
	}
}

func TestCallbackOptionsRefusedWhereAbsent(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	c.mustCall("initialize", InitializeParams{}, nil)
	_, info := errorInfoOf(t, c.call("session/start", map[string]any{"Provider": agenttest.Name, "OnUserDialog": true}, nil))
	if info.Kind != "invalid_params" {
		t.Errorf("OnUserDialog on agenttest: %+v", info)
	}
	for _, spec := range Defaults() {
		if spec.Name == codex.Name {
			continue
		}
		if _, err := spec.New(nil, ProviderBridge{Init: ProviderInit{WireTap: true}}); errorKind(err) != "invalid_params" {
			t.Errorf("%s provider WireTap: %v", spec.Name, err)
		}
		if spec.Name == claude.Name {
			continue
		}
		if _, err := spec.SessionOptions(nil, SessionBridge{Start: StartParams{OnControlRequest: true}}); errorKind(err) != "invalid_params" {
			t.Errorf("%s OnControlRequest: %v", spec.Name, err)
		}
	}
}

// recordingCall is a Call that records reverse requests and answers them.
type recordingCall struct {
	method string
	params any
}

func (r *recordingCall) call(_ context.Context, method string, params any) (json.RawMessage, error) {
	r.method, r.params = method, params
	return json.RawMessage(`{"answer":42}`), nil
}

func specNamed(t *testing.T, name string) ProviderSpec {
	t.Helper()
	for _, spec := range Defaults() {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("no %s spec", name)
	return ProviderSpec{}
}

func TestClaudeSessionCallbacks(t *testing.T) {
	var rec recordingCall
	b := SessionBridge{Start: StartParams{OnUserDialog: true, OnControlRequest: true}, Session: "s7", Call: rec.call}
	v, err := specNamed(t, claude.Name).SessionOptions(json.RawMessage(`{"MaxTurns":3}`), b)
	if err != nil {
		t.Fatal(err)
	}
	opts := v.(*claude.SessionOptions)
	if opts.MaxTurns != 3 || opts.OnUserDialog == nil || opts.OnControlRequest == nil {
		t.Fatalf("options %+v", opts)
	}
	answer, err := opts.OnUserDialog(context.Background(), claude.UserDialog{Kind: "confirm", RequestID: "r1"})
	if err != nil || string(answer) != `{"answer":42}` || rec.method != "claude/userDialog" {
		t.Errorf("OnUserDialog = %s, %v via %s", answer, err, rec.method)
	}
	if call, ok := rec.params.(UserDialogCall); !ok || call.Session != "s7" || call.Dialog.RequestID != "r1" {
		t.Errorf("claude/userDialog params %+v", rec.params)
	}
	if _, err := opts.OnControlRequest(context.Background(), "mystery", json.RawMessage(`{}`)); err != nil || rec.method != "claude/controlRequest" {
		t.Errorf("OnControlRequest: %v via %s", err, rec.method)
	}
	plain, err := specNamed(t, claude.Name).SessionOptions(nil, SessionBridge{})
	if err != nil || plain.(*claude.SessionOptions).OnUserDialog != nil {
		t.Errorf("callbacks set without being asked for: %v", err)
	}
}

func TestCodexProviderCallbacks(t *testing.T) {
	var rec recordingCall
	var tapped []string
	opts, err := decodeOptions[codex.Options](json.RawMessage(`{"SharedServer":true}`))
	if err != nil {
		t.Fatal(err)
	}
	bindCodexProvider(&opts, ProviderBridge{
		Init:    ProviderInit{OnAuthTokensRefresh: true, WireTap: true},
		Call:    rec.call,
		WireTap: func(_ agent.WireDirection, line []byte) { tapped = append(tapped, string(line)) },
	})
	if !opts.SharedServer || opts.OnAuthTokensRefresh == nil || opts.WireTap == nil {
		t.Fatalf("options %+v", opts)
	}
	if _, err := opts.OnAuthTokensRefresh(context.Background(), json.RawMessage(`{"reason":"expired"}`)); err != nil || rec.method != "codex/authTokensRefresh" {
		t.Errorf("OnAuthTokensRefresh: %v via %s", err, rec.method)
	}
	opts.WireTap(agent.WireReceive, []byte("line"))
	if len(tapped) != 1 || tapped[0] != "line" {
		t.Errorf("tapped %q", tapped)
	}
}

func TestOptionsRejectUnknownAndFuncFields(t *testing.T) {
	if _, err := decodeOptions[codex.Options](json.RawMessage(`{"SharedServr":true}`)); errorKind(err) != "invalid_params" {
		t.Errorf("misspelled option: %v", err)
	}
	if _, err := decodeOptions[codex.Options](json.RawMessage(`{"WireTap":"x"}`)); errorKind(err) != "invalid_params" {
		t.Errorf("function option: %v", err)
	}
}

func TestClaudeHooksCallClient(t *testing.T) {
	var got HookCall
	var method string
	answer := json.RawMessage(`{"decision":"block","reason":"no rm"}`)
	b := SessionBridge{Session: "s3", Call: func(_ context.Context, m string, params any) (json.RawMessage, error) {
		method, got = m, params.(HookCall)
		return answer, nil
	}}
	v, err := specNamed(t, claude.Name).SessionOptions(json.RawMessage(`{"Hooks":{"PreToolUse":[{"Matcher":"Read"},{"Matcher":"Bash","Timeout":5000000000}]}}`), b)
	if err != nil {
		t.Fatal(err)
	}
	matchers := v.(*claude.SessionOptions).Hooks[claude.HookPreToolUse]
	if len(matchers) != 2 || len(matchers[1].Hooks) != 1 || matchers[1].Timeout != 5*time.Second {
		t.Fatalf("matchers %+v", matchers)
	}
	in := claude.HookInput{Event: claude.HookPreToolUse, ToolName: "Bash", Raw: json.RawMessage(`{"tool_name":"Bash","extra":1}`), ToolUseIDArg: "tu1"}
	out, err := matchers[1].Hooks[0](context.Background(), in)
	if err != nil || out.Decision != "block" || out.Reason != "no rm" {
		t.Errorf("hook output %+v, %v", out, err)
	}
	if method != "claude/hook" || got.Session != "s3" || got.Event != claude.HookPreToolUse || got.Matcher != "Bash" || got.Index != 1 ||
		string(got.Payload) != `{"tool_name":"Bash","extra":1}` || got.ToolUseID != "tu1" {
		t.Errorf("claude/hook %s %+v", method, got)
	}

	answer = json.RawMessage(`"not an object"`)
	if out, err := matchers[1].Hooks[0](context.Background(), in); err != nil || out.Decision != "" {
		t.Errorf("malformed reply gave %+v, %v; want no decision", out, err)
	}

	b.Call = func(context.Context, string, any) (json.RawMessage, error) { return nil, errors.New("client gone") }
	v, _ = specNamed(t, claude.Name).SessionOptions(json.RawMessage(`{"Hooks":{"Stop":[{}]}}`), b)
	out, err = v.(*claude.SessionOptions).Hooks[claude.HookStop][0].Hooks[0](context.Background(), claude.HookInput{})
	if err != nil || out.Decision != "" || out.Continue != nil {
		t.Errorf("failed hook call gave %+v, %v; want no decision", out, err)
	}
}
