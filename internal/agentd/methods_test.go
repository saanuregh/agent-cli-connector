package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/agenttest"
)

// richProvider adds every provider-level optional interface to the fake,
// and starts sessions that implement every session-level one.
type richProvider struct {
	*agenttest.Provider
	mu    sync.Mutex
	calls []string
}

func (p *richProvider) record(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, fmt.Sprintf(format, args...))
}

func (p *richProvider) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *richProvider) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	sess, err := p.Provider.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &richSession{Session: sess, provider: p}, nil
}

func (p *richProvider) Check(context.Context) (*agent.ProviderStatus, error) {
	p.record("Check")
	return &agent.ProviderStatus{Installed: true, Version: "1.2.3", Err: agent.ErrClosed}, nil
}

func (p *richProvider) ListModels(context.Context) ([]agent.Model, error) {
	p.record("ListModels")
	return []agent.Model{{ID: "m1", IsDefault: true}}, nil
}

func (p *richProvider) ListSessions(_ context.Context, opts agent.ListSessionsOptions) ([]agent.SessionSummary, error) {
	p.record("ListSessions %s %d", opts.Cwd, opts.Limit)
	return []agent.SessionSummary{{ID: "old", Title: "t"}}, nil
}

func (p *richProvider) ReadHistory(_ context.Context, id string, opts agent.ReadHistoryOptions) ([]agent.Item, error) {
	p.record("ReadHistory %s %s", id, opts.SubagentID)
	return []agent.Item{{ID: "i1", Text: "hello"}}, nil
}

func (p *richProvider) DeleteSession(_ context.Context, id, cwd string) error {
	p.record("DeleteSession %s %s", id, cwd)
	return nil
}

func (p *richProvider) ListSubagents(_ context.Context, id string, opts agent.ListSubagentsOptions) ([]agent.Task, error) {
	p.record("ListSubagents %s %s", id, opts.Cwd)
	return []agent.Task{{ID: "task1"}}, nil
}

// richSession hides agenttest.Session's own extras behind the embedded
// interface and records every optional call.
type richSession struct {
	agent.Session
	provider *richProvider
}

func (s *richSession) SetModel(ctx context.Context, model string) error {
	s.provider.record("SetModel %s", model)
	return s.Session.SetModel(ctx, model)
}

func (s *richSession) SetEffort(ctx context.Context, effort agent.Effort) error {
	s.provider.record("SetEffort %s", effort)
	return s.Session.SetEffort(ctx, effort)
}

func (s *richSession) SetPermissionMode(ctx context.Context, mode agent.PermissionMode) error {
	s.provider.record("SetPermissionMode %s", mode)
	return s.Session.SetPermissionMode(ctx, mode)
}

func (s *richSession) SetTitle(_ context.Context, title string) error {
	s.provider.record("SetTitle %s", title)
	return nil
}

func (s *richSession) Compact(_ context.Context, instructions string) error {
	s.provider.record("Compact %s", instructions)
	return nil
}

func (s *richSession) Rewind(_ context.Context, turnID string) error {
	s.provider.record("Rewind %s", turnID)
	return nil
}

func (s *richSession) StopTask(_ context.Context, taskID string) error {
	s.provider.record("StopTask %s", taskID)
	return nil
}

func (s *richSession) InterruptTurn(_ context.Context, turnID string) error {
	s.provider.record("InterruptTurn %s", turnID)
	return nil
}

func (s *richSession) RateLimits(context.Context) ([]agent.RateLimit, error) {
	s.provider.record("RateLimits")
	return []agent.RateLimit{{Name: "five_hour", UsedPercent: 12.5}}, nil
}

func (s *richSession) ForkSession(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	s.provider.record("ForkSession")
	return s.provider.Start(ctx, cfg)
}

// methodCase is one method call with its expected result and recorded call.
type methodCase struct {
	method string
	params any
	result any // pointer to a zero value; compared with want
	want   any
	call   string
}

func runMethodCases(t *testing.T, c *client, p *richProvider, cases []methodCase) {
	t.Helper()
	for _, tc := range cases {
		before := len(p.recorded())
		result := tc.result
		if result == nil {
			result = &map[string]any{}
		}
		c.mustCall(tc.method, tc.params, result)
		if tc.want != nil {
			if got := reflect.ValueOf(result).Elem().Interface(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s = %+v, want %+v", tc.method, got, tc.want)
			}
		} else if m := *result.(*map[string]any); len(m) != 0 {
			t.Errorf("%s = %v, want {}", tc.method, m)
		}
		calls := p.recorded()[before:]
		if len(calls) != 1 || calls[0] != tc.call {
			t.Errorf("%s recorded %q, want %q", tc.method, calls, tc.call)
		}
	}
}

func TestProviderMethods(t *testing.T) {
	p := &richProvider{Provider: agenttest.New(echo)}
	c := newClient(t, fakeSpec(p))
	c.mustCall("initialize", InitializeParams{}, nil)
	name := agenttest.Name

	var status struct {
		Installed bool
		Version   string
		Err       *ErrorInfo
	}
	c.mustCall("provider/check", ProviderParams{Provider: name}, &status)
	if !status.Installed || status.Version != "1.2.3" || status.Err == nil || status.Err.Kind != "closed" {
		t.Errorf("provider/check = %+v", status)
	}

	runMethodCases(t, c, p, []methodCase{
		{"provider/listModels", ProviderParams{Provider: name}, &[]agent.Model{}, []agent.Model{{ID: "m1", IsDefault: true}}, "ListModels"},
		{"provider/listSessions", ListSessionsParams{Provider: name, Options: agent.ListSessionsOptions{Cwd: "/w", Limit: 3}},
			&[]agent.SessionSummary{}, []agent.SessionSummary{{ID: "old", Title: "t"}}, "ListSessions /w 3"},
		{"provider/readHistory", ReadHistoryParams{Provider: name, SessionID: "old", Options: agent.ReadHistoryOptions{SubagentID: "sub"}},
			&[]agent.Item{}, []agent.Item{{ID: "i1", Text: "hello"}}, "ReadHistory old sub"},
		{"provider/deleteSession", DeleteSessionParams{Provider: name, SessionID: "old", Cwd: "/w"}, nil, nil, "DeleteSession old /w"},
		{"provider/listSubagents", ListSubagentsParams{Provider: name, SessionID: "old", Options: agent.ListSubagentsOptions{Cwd: "/w"}},
			&[]agent.Task{}, []agent.Task{{ID: "task1"}}, "ListSubagents old /w"},
	})

	if _, info := errorInfoOf(t, c.call("provider/listModels", ProviderParams{Provider: "nope"}, nil)); info.Kind != "not_found" {
		t.Errorf("unknown provider: %+v", info)
	}
}

func TestSessionMethods(t *testing.T) {
	p := &richProvider{Provider: agenttest.New(echo)}
	c := newClient(t, fakeSpec(p))
	s := startFake(c).Session

	runMethodCases(t, c, p, []methodCase{
		{"session/setModel", ValueParams{Session: s, Value: "m2"}, nil, nil, "SetModel m2"},
		{"session/setEffort", ValueParams{Session: s, Value: "high"}, nil, nil, "SetEffort high"},
		{"session/setPermissionMode", ValueParams{Session: s, Value: "plan"}, nil, nil, "SetPermissionMode plan"},
		{"session/setTitle", ValueParams{Session: s, Value: "My title"}, nil, nil, "SetTitle My title"},
		{"session/compact", CompactParams{Session: s, Instructions: "short"}, nil, nil, "Compact short"},
		{"session/rewind", TurnParams{Session: s, TurnID: "t1"}, nil, nil, "Rewind t1"},
		{"session/stopTask", StopTaskParams{Session: s, TaskID: "task1"}, nil, nil, "StopTask task1"},
		{"session/interruptTurn", TurnParams{Session: s, TurnID: "t2"}, nil, nil, "InterruptTurn t2"},
		{"session/rateLimits", SessionParams{Session: s}, &[]agent.RateLimit{}, []agent.RateLimit{{Name: "five_hour", UsedPercent: 12.5}}, "RateLimits"},
	})

	var info agent.SessionInfo
	c.mustCall("session/info", SessionParams{Session: s}, &info)
	if info.Model != "m2" {
		t.Errorf("model after setModel = %q", info.Model)
	}
	if _, info := errorInfoOf(t, c.call("session/setModel", ValueParams{Session: "s9"}, nil)); info.Kind != "not_found" {
		t.Errorf("unknown session: %+v", info)
	}
}

func TestOptionalMethodsUnsupported(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	s := startFake(c).Session
	name := agenttest.Name
	// session/interruptTurn is absent: agenttest.Session implements
	// agent.TurnInterrupter itself.
	calls := []struct {
		method string
		params any
	}{
		{"provider/check", ProviderParams{Provider: name}},
		{"provider/listModels", ProviderParams{Provider: name}},
		{"provider/listSessions", ListSessionsParams{Provider: name}},
		{"provider/readHistory", ReadHistoryParams{Provider: name, SessionID: "x"}},
		{"provider/deleteSession", DeleteSessionParams{Provider: name, SessionID: "x"}},
		{"provider/listSubagents", ListSubagentsParams{Provider: name, SessionID: "x"}},
		{"session/setTitle", ValueParams{Session: s, Value: "t"}},
		{"session/compact", CompactParams{Session: s}},
		{"session/rewind", TurnParams{Session: s, TurnID: "t"}},
		{"session/stopTask", StopTaskParams{Session: s, TaskID: "t"}},
		{"session/rateLimits", SessionParams{Session: s}},
		{"session/fork", map[string]any{"Session": s, "Start": map[string]any{}}},
	}
	for _, tc := range calls {
		if _, info := errorInfoOf(t, c.call(tc.method, tc.params, nil)); info.Kind != "unsupported" {
			t.Errorf("%s: %+v", tc.method, info)
		}
	}
}

func TestFork(t *testing.T) {
	p := &richProvider{Provider: agenttest.New(echo)}
	c := newClient(t, fakeSpec(p))
	s := startFake(c).Session
	var forked StartResult
	c.mustCall("session/fork", map[string]any{"Session": s, "Start": map[string]any{"Config": map[string]any{"Model": "m3"}}}, &forked)
	if forked.Session != "s2" || forked.Info.Model != "m3" {
		t.Fatalf("fork = %+v", forked)
	}
	if calls := p.recorded(); len(calls) != 1 || calls[0] != "ForkSession" {
		t.Errorf("recorded %q", calls)
	}
	c.mustCall("session/send", SendParams{Session: forked.Session, Input: agent.Text("hi")}, nil)
	_, res := c.untilResult()
	if res.Session != "s2" || res.Result == nil || res.Result.Text != "echo: hi" {
		t.Errorf("forked turn/result = %+v", res)
	}
}

// awaitPermission starts a turn and returns the PermissionRequest event.
func awaitPermission(t *testing.T, c *client) (string, agent.PermissionRequest) {
	t.Helper()
	s := startFake(c).Session
	c.mustCall("session/send", SendParams{Session: s, Input: agent.Text("go")}, nil)
	for {
		var ev EventNotification
		if err := json.Unmarshal(c.next("session/event").Params, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != "PermissionRequest" {
			continue
		}
		var req agent.PermissionRequest
		if err := json.Unmarshal(ev.Event, &req); err != nil {
			t.Fatal(err)
		}
		return s, req
	}
}

func TestRequestRespond(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(askPermission)))
	s, req := awaitPermission(t, c)
	if req.ID != "perm1" {
		t.Fatalf("request = %+v", req)
	}

	var pending []PendingRequest
	c.mustCall("session/pendingRequests", SessionParams{Session: s}, &pending)
	if len(pending) != 1 || pending[0].Type != "PermissionRequest" || !strings.Contains(string(pending[0].Request), `"perm1"`) {
		t.Fatalf("pendingRequests = %+v", pending)
	}

	_, info := errorInfoOf(t, c.call("request/respond", RespondParams{Session: s, RequestID: "perm1", UserInput: &agent.UserInputResponse{}}, nil))
	if info.Kind != "invalid_params" {
		t.Errorf("wrong response type: %+v", info)
	}
	_, info = errorInfoOf(t, c.call("request/respond", RespondParams{Session: s, RequestID: "perm1"}, nil))
	if info.Kind != "invalid_params" {
		t.Errorf("no response: %+v", info)
	}
	_, info = errorInfoOf(t, c.call("request/respond", RespondParams{Session: s, RequestID: "perm1",
		Permission: &agent.PermissionResponse{Decision: agent.DecisionAllow}, UserInput: &agent.UserInputResponse{}}, nil))
	if info.Kind != "invalid_params" {
		t.Errorf("two responses: %+v", info)
	}
	_, info = errorInfoOf(t, c.call("request/respond", RespondParams{Session: s, RequestID: "nope",
		Permission: &agent.PermissionResponse{Decision: agent.DecisionAllow}}, nil))
	if info.Kind != "not_found" {
		t.Errorf("unknown request: %+v", info)
	}

	allow := RespondParams{Session: s, RequestID: "perm1", Permission: &agent.PermissionResponse{Decision: agent.DecisionAllow}}
	c.mustCall("request/respond", allow, nil)
	_, res := c.untilResult()
	if res.Result == nil || !strings.HasPrefix(res.Result.Text, "allow:") {
		t.Fatalf("turn/result = %+v", res.Result)
	}
	// An answered request leaves PendingRequests, so agentd no longer finds it.
	if _, info := errorInfoOf(t, c.call("request/respond", allow, nil)); info.Kind != "not_found" {
		t.Errorf("second answer: %+v", info)
	}
	c.mustCall("session/pendingRequests", SessionParams{Session: s}, &pending)
	if len(pending) != 0 {
		t.Errorf("pending after answer = %+v", pending)
	}
}

func TestRequestCancel(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(askPermission)))
	s, req := awaitPermission(t, c)
	c.mustCall("request/cancel", RequestParams{Session: s, RequestID: req.ID}, nil)
	_, res := c.untilResult()
	if res.Result == nil || !strings.HasPrefix(res.Result.Text, string(agent.DecisionCancel)+":") {
		t.Fatalf("turn/result = %+v", res.Result)
	}
	if _, info := errorInfoOf(t, c.call("request/cancel", RequestParams{Session: s, RequestID: req.ID}, nil)); info.Kind != "not_found" {
		t.Errorf("cancel after cancel: %+v", info)
	}
}

func TestMethodsRejectUnknownFields(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	s := startFake(c).Session
	for _, method := range []string{"provider/listModels", "session/setModel", "request/respond", "session/pendingRequests"} {
		code, info := errorInfoOf(t, c.call(method, json.RawMessage(`{"Session":"`+s+`","Provider":"agenttest","Bogus":1}`), nil))
		if code != codeInvalidParams || info.Kind != "invalid_params" {
			t.Errorf("%s: code %d %+v", method, code, info)
		}
	}
}

func TestMethodsListed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		provider  agent.Provider
		providers []string
		hasSess   []string
		notSess   []string
	}{
		{"plain", agenttest.New(echo), []string{}, []string{"request/respond", "session/interruptTurn", "session/send"}, []string{"session/fork", "session/setTitle", "session/start"}},
		{"rich", &richProvider{Provider: agenttest.New(echo)}, []string{"provider/check", "provider/deleteSession", "provider/listModels",
			"provider/listSessions", "provider/listSubagents", "provider/readHistory"}, []string{"session/fork", "session/rateLimits", "session/setTitle"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, fakeSpec(tc.provider))
			var init InitializeResult
			c.mustCall("initialize", InitializeParams{}, &init)
			if got := init.Providers[0].Methods; !slices.Equal(got, tc.providers) {
				t.Errorf("provider methods %v, want %v", got, tc.providers)
			}
			s := startAgenttest(c)
			for _, m := range tc.hasSess {
				if !slices.Contains(s.Methods, m) {
					t.Errorf("session methods %v lack %s", s.Methods, m)
				}
			}
			for _, m := range tc.notSess {
				if slices.Contains(s.Methods, m) {
					t.Errorf("session methods %v include %s", s.Methods, m)
				}
			}
		})
	}
}

// TestUnlistedMethodsAreUnsupported checks optionalMethods against the
// handlers: every method a plain agenttest provider or session does not
// list must fail with unsupported.
func TestUnlistedMethodsAreUnsupported(t *testing.T) {
	c := newClient(t, fakeSpec(agenttest.New(echo)))
	var init InitializeResult
	c.mustCall("initialize", InitializeParams{}, &init)
	s := startAgenttest(c)
	listed := append(slices.Clone(init.Providers[0].Methods), s.Methods...)
	for name := range optionalMethods {
		if slices.Contains(listed, name) {
			continue
		}
		params := map[string]any{"Session": s.Session}
		if strings.HasPrefix(name, "provider/") {
			params = map[string]any{"Provider": agenttest.Name}
		}
		if _, info := errorInfoOf(t, c.call(name, params, nil)); info.Kind != "unsupported" {
			t.Errorf("%s is not listed but gave %+v", name, info)
		}
	}
}
