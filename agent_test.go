package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/opencode"
	"github.com/saanuregh/agent-cli-connector/pi"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

func TestRequestAnswerOnce(t *testing.T) {
	calls := 0
	fail := true
	req := providerkit.NewPermissionRequest(agent.PermissionRequest{ID: "r"}, func(ctx context.Context, resp agent.PermissionResponse) error {
		calls++
		if fail {
			fail = false
			return errors.New("write failed")
		}
		return nil
	})
	if err := req.Allow(context.Background()); err == nil {
		t.Fatal("expected first answer to fail")
	}
	select {
	case <-req.Done():
		t.Fatal("failed answer closed the request")
	default:
	}
	if err := req.Deny(context.Background(), "no"); err != nil {
		t.Fatal(err)
	}
	if err := req.Allow(context.Background()); !errors.Is(err, agent.ErrRequestDone) {
		t.Errorf("third answer = %v", err)
	}
	if calls != 2 || req.Outcome() != agent.RequestAnswered {
		t.Errorf("calls %d outcome %q", calls, req.Outcome())
	}
}

func TestPermissionRespondRefusesDecisionNotOffered(t *testing.T) {
	var sent []agent.PermissionDecision
	newRequest := func() *agent.PermissionRequest {
		return providerkit.NewPermissionRequest(agent.PermissionRequest{
			ID:        "r",
			Decisions: []agent.PermissionDecision{agent.DecisionAllow},
		}, func(_ context.Context, resp agent.PermissionResponse) error {
			sent = append(sent, resp.Decision)
			return nil
		})
	}
	req := newRequest()
	for _, d := range []agent.PermissionDecision{agent.DecisionAllowSession, agent.DecisionAllowAlways} {
		if err := req.Respond(context.Background(), agent.PermissionResponse{Decision: d}); !errors.Is(err, agent.ErrInvalidAnswer) {
			t.Errorf("%s = %v, want ErrInvalidAnswer", d, err)
		}
	}
	if req.Outcome() != agent.RequestPending {
		t.Errorf("outcome after refused answers = %q", req.Outcome())
	}
	if err := req.Allow(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Refusing stays possible even when the harness did not list it.
	if err := newRequest().Deny(context.Background(), "no"); err != nil {
		t.Errorf("deny = %v", err)
	}
	if err := newRequest().Cancel(context.Background()); err != nil {
		t.Errorf("cancel = %v", err)
	}
	// A native answer bypasses the uniform decision check.
	if err := newRequest().Respond(context.Background(), agent.PermissionResponse{Decision: agent.DecisionAllowAlways, ProviderResponse: "native"}); err != nil {
		t.Errorf("provider response = %v", err)
	}
	want := []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionDeny, agent.DecisionCancel, agent.DecisionAllowAlways}
	if !slices.Equal(sent, want) {
		t.Errorf("sent %v, want %v", sent, want)
	}
}

func TestWithdrawnRequestRejectsAnswers(t *testing.T) {
	req := providerkit.NewElicitationRequest(agent.ElicitationRequest{ID: "e"}, func(context.Context, agent.ElicitationResponse) error { return nil })
	providerkit.WithdrawRequest(req)
	if err := req.Decline(context.Background()); !errors.Is(err, agent.ErrRequestDone) {
		t.Errorf("answer after withdraw = %v", err)
	}
	if req.Outcome() != agent.RequestWithdrawn {
		t.Errorf("outcome = %q", req.Outcome())
	}
}

func TestPlainText(t *testing.T) {
	in := agent.Prompt(agent.TextPart("hello"), agent.ImageFile("x.png"), agent.FileRef("main.go"), agent.SkillPart("review", "/s"))
	if got := in.PlainText(); got != "hello\n\n@main.go\n\n/review" {
		t.Errorf("PlainText = %q", got)
	}
}

func TestMergeEnv(t *testing.T) {
	got := providerkit.MergeEnv([]string{"A=1", "B=2"}, []string{"B=3", "C=4"})
	if !slices.Equal(got, []string{"A=1", "B=3", "C=4"}) {
		t.Errorf("MergeEnv = %v", got)
	}
	if got := providerkit.EnvList(map[string]string{"Z": "1", "A": "2"}); !slices.Equal(got, []string{"A=2", "Z=1"}) {
		t.Errorf("EnvList = %v", got)
	}
}

func TestUnsupportedError(t *testing.T) {
	err := providerkit.Unsupported("pi", "output schema")
	if !errors.Is(err, agent.ErrUnsupported) || err.Error() != "pi: output schema is not supported" {
		t.Errorf("err = %v", err)
	}
}

type fakeProvider struct{ name string }

func (f fakeProvider) Name() string                     { return f.name }
func (f fakeProvider) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (f fakeProvider) Start(context.Context, agent.Config) (agent.Session, error) {
	return nil, errors.New("started " + f.name)
}

func TestRegistry(t *testing.T) {
	r := agent.NewRegistry(fakeProvider{"b"}, fakeProvider{"a"})
	if !slices.Equal(r.Names(), []string{"a", "b"}) {
		t.Errorf("names = %v", r.Names())
	}
	if _, err := r.Start(context.Background(), "a", agent.Config{}); err == nil || err.Error() != "started a" {
		t.Errorf("start = %v", err)
	}
	if _, err := r.Start(context.Background(), "zzz", agent.Config{}); err == nil {
		t.Error("unknown provider started")
	}
}

func TestToolResultHelpers(t *testing.T) {
	res, err := agent.JSONResult(map[string]int{"n": 1})
	if err != nil || res.Text() != `{"n":1}` || string(res.Structured) != `{"n":1}` {
		t.Errorf("JSONResult = %+v %v", res, err)
	}
	got := providerkit.CallTool(context.Background(), agent.Tool{Name: "x", Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) {
		return agent.ToolResult{}, errors.New("bad")
	}}, agent.ToolCallRequest{})
	if !got.IsError || got.Text() != "bad" {
		t.Errorf("CallTool = %+v", got)
	}
}

func TestTranscript(t *testing.T) {
	var tr agent.Transcript
	tr.Apply(&agent.ItemStarted{Item: agent.Item{ID: "m", Kind: agent.ItemAssistantMessage, Status: agent.ItemRunning}})
	tr.Apply(&agent.TextDelta{ItemID: "m", Delta: "Hel"})
	tr.Apply(&agent.TextDelta{ItemID: "m", Delta: "lo"})
	tr.Apply(&agent.ItemStarted{Item: agent.Item{ID: "t", Kind: agent.ItemToolCall, Status: agent.ItemRunning, Tool: &agent.ToolCall{Name: "Bash"}}})
	tr.Apply(&agent.ToolOutputDelta{ItemID: "t", Delta: "out"})
	tr.Apply(&agent.ItemUpdated{Item: agent.Item{ID: "t", Kind: agent.ItemToolCall, Status: agent.ItemRunning, Tool: &agent.ToolCall{Name: "Bash", Command: "ls"}}})
	if it, _ := tr.Item("t"); it.Tool.Output != "out" || it.Tool.Command != "ls" {
		t.Errorf("tool after update = %+v", it.Tool)
	}
	if it, _ := tr.Item("m"); it.Text != "Hello" {
		t.Errorf("text = %q", it.Text)
	}
	tr.Apply(&agent.ItemCompleted{Item: agent.Item{ID: "m", Kind: agent.ItemAssistantMessage, Status: agent.ItemDone, Text: "Hello!"}})
	items := tr.Items()
	if len(items) != 2 || items[0].Text != "Hello!" || items[0].Status != agent.ItemDone {
		t.Errorf("items = %+v", items)
	}
}

func TestTranscriptPlanDeltaAndRemoval(t *testing.T) {
	var tr agent.Transcript
	tr.Apply(&agent.PlanDelta{ItemID: "p", Delta: "1. a"})
	tr.Apply(&agent.PlanDelta{ItemID: "p", Delta: "\n2. b"})
	tr.Apply(&agent.ItemCompleted{Item: agent.Item{ID: "m", Kind: agent.ItemAssistantMessage, Text: "hi"}})
	if it, _ := tr.Item("p"); it.Kind != agent.ItemPlan || it.Text != "1. a\n2. b" {
		t.Errorf("plan item %+v", it)
	}
	if _, ok := tr.Apply(&agent.ItemsRemoved{IDs: []string{"p"}}); !ok {
		t.Error("removal not reported")
	}
	if items := tr.Items(); len(items) != 1 || items[0].ID != "m" {
		t.Errorf("items %+v", items)
	}
	if it, ok := tr.Item("m"); !ok || it.Text != "hi" {
		t.Errorf("index broken after removal: %+v %v", it, ok)
	}
}

func TestUnsetEnv(t *testing.T) {
	got := providerkit.UnsetEnv([]string{"A=1", "CLAUDECODE=1", "B=2"}, []string{"CLAUDECODE"})
	if strings.Join(got, ",") != "A=1,B=2" {
		t.Errorf("UnsetEnv = %v", got)
	}
}

func TestProviderOptionsOfAnotherProviderRefused(t *testing.T) {
	ctx := context.Background()
	for _, p := range []agent.Provider{claude.New(claude.Options{}), codex.New(codex.Options{}), opencode.New(opencode.Options{})} {
		_, err := p.Start(ctx, agent.Config{ProviderOptions: &pi.SessionOptions{}})
		if err == nil || !strings.Contains(err.Error(), "ProviderOptions holds *pi.SessionOptions") {
			t.Errorf("%s: start with Pi options = %v", p.Name(), err)
		}
	}
	if _, err := pi.New(pi.Options{}).Start(ctx, agent.Config{ProviderOptions: claude.SessionOptions{}}); err == nil || !strings.Contains(err.Error(), "want *pi.SessionOptions") {
		t.Errorf("pi: start with Claude options = %v", err)
	}
}

func TestStrictRefusesIgnoredConfig(t *testing.T) {
	ctx := context.Background()
	fail := agent.SpawnFunc(func(context.Context, agent.Command) (agent.Process, error) {
		t.Error("started despite Config.Strict")
		return nil, errors.New("not started")
	})
	for _, c := range []struct {
		p   agent.Provider
		cfg agent.Config
	}{
		{claude.New(claude.Options{Spawner: fail}), agent.Config{MCPServers: map[string]agent.MCPServer{"docs": {Command: "x", EnabledTools: []string{"a"}}}}},
		{codex.New(codex.Options{Spawner: fail}), agent.Config{AllowedTools: []string{"shell"}}},
		{pi.New(pi.Options{Spawner: fail}), agent.Config{AdditionalDirectories: []string{"/tmp"}}},
	} {
		c.cfg.Strict = true
		if _, err := c.p.Start(ctx, c.cfg); !errors.Is(err, agent.ErrUnsupported) {
			t.Errorf("%s: strict start = %v", c.p.Name(), err)
		}
	}
}

func TestNewToolDecodesArguments(t *testing.T) {
	type args struct {
		City string `json:"city"`
	}
	tool := agent.NewTool("weather", "Weather.", nil, func(_ context.Context, a args) (agent.ToolResult, error) {
		return agent.TextResult("Sunny in " + a.City), nil
	})
	res, err := tool.Handler(context.Background(), agent.ToolCallRequest{Arguments: json.RawMessage(`{"city":"Oslo"}`)})
	if err != nil || res.Text() != "Sunny in Oslo" || tool.Name != "weather" {
		t.Errorf("result %+v, %v", res, err)
	}
	if _, err := tool.Handler(context.Background(), agent.ToolCallRequest{Arguments: json.RawMessage(`{"city":1}`)}); err == nil || !strings.Contains(err.Error(), "invalid arguments for weather") {
		t.Errorf("bad arguments error = %v", err)
	}
}

type nativeEvent struct{ agent.EventMeta }

func TestEventType(t *testing.T) {
	events := []agent.Event{
		&agent.SessionStarted{}, &agent.SessionUpdated{}, &agent.StatusChanged{}, &agent.TurnStarted{},
		&agent.TurnCompleted{}, &agent.ItemStarted{}, &agent.ItemUpdated{}, &agent.ItemCompleted{},
		&agent.TextDelta{}, &agent.ReasoningDelta{}, &agent.ToolInputDelta{}, &agent.ToolOutputDelta{},
		&agent.PlanUpdated{}, &agent.UsageUpdated{}, &agent.TaskUpdated{}, &agent.Compaction{},
		&agent.Retry{}, &agent.Notice{}, &agent.RequestResolved{}, &agent.RateLimitUpdated{},
		&agent.HookRun{}, &agent.GoalUpdated{}, &agent.ConversationReset{}, &agent.PlanDelta{},
		&agent.ItemsRemoved{}, &agent.EventsDropped{}, &agent.RawEvent{},
		&agent.PermissionRequest{}, &agent.UserInputRequest{}, &agent.ElicitationRequest{},
	}
	covered := map[string]bool{}
	for _, ev := range events {
		want := reflect.TypeOf(ev).Elem().Name()
		if got := agent.EventType(ev); got != want {
			t.Errorf("EventType(%T) = %q; want %q", ev, got, want)
		}
		covered[want] = true
	}
	if got := agent.EventType(&nativeEvent{}); got != "nativeEvent" {
		t.Errorf("EventType of a provider event = %q", got)
	}
	if got := agent.EventType(nil); got != "" {
		t.Errorf("EventType(nil) = %q", got)
	}

	// Every struct in package agent that embeds EventMeta must be listed above.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return false
			}
			for _, field := range st.Fields.List {
				if id, ok := field.Type.(*ast.Ident); ok && len(field.Names) == 0 && id.Name == "EventMeta" && !covered[spec.Name.Name] {
					t.Errorf("event type %s is missing from TestEventType", spec.Name.Name)
				}
			}
			return false
		})
	}
}

func TestUsageAdd(t *testing.T) {
	u := agent.Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, ReasoningTokens: 5, TotalTokens: 6, CostUSD: 0.5, ContextWindow: 100, ContextTokens: 10}
	u.Add(agent.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40, ReasoningTokens: 50, TotalTokens: 60, CostUSD: 0.25, ContextTokens: 70})
	want := agent.Usage{InputTokens: 11, OutputTokens: 22, CacheReadTokens: 33, CacheWriteTokens: 44, ReasoningTokens: 55, TotalTokens: 66, CostUSD: 0.75, ContextWindow: 100, ContextTokens: 70}
	if u != want {
		t.Errorf("Add = %+v; want %+v", u, want)
	}
	u.Add(agent.Usage{ContextWindow: 200})
	if u.ContextWindow != 200 || u.ContextTokens != 70 {
		t.Errorf("context after Add = %d/%d; want 70/200", u.ContextTokens, u.ContextWindow)
	}
}
