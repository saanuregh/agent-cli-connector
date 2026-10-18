//go:build live

package claude_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/internal/livetest"
)

// These tests run the real claude CLI and spend tokens. Run them with
//
//	go test -tags live ./claude -run Live -v
//
// CLAUDE_TEST_MODEL overrides the model (default "haiku").

func liveModel() string {
	if m := os.Getenv("CLAUDE_TEST_MODEL"); m != "" {
		return m
	}
	return "haiku"
}

// start starts a session, in a new temporary directory when cfg.Cwd is
// empty, and deletes what it stored when the test ends.
func start(ctx context.Context, t *testing.T, p *claude.Provider, cfg agent.Config) (*claude.Session, *livetest.Store) {
	t.Helper()
	if cfg.Cwd == "" {
		cfg.Cwd = t.TempDir()
	}
	store := livetest.New(t, p, cfg.Cwd)
	sess, err := p.StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store.Track(sess)
	return sess, store
}
func TestLiveToolsAndPermissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var permMu sync.Mutex
	var asked []string
	add := agent.Tool{
		Name:        "add",
		Description: "Adds two integers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
		Handler: func(ctx context.Context, call agent.ToolCallRequest) (agent.ToolResult, error) {
			var in struct{ A, B int }
			if err := json.Unmarshal(call.Arguments, &in); err != nil {
				return agent.ToolResult{}, err
			}
			return agent.JSONResult(map[string]int{"sum": in.A + in.B})
		},
	}
	dir := t.TempDir()
	sess, _ := start(ctx, t, claude.New(claude.Options{}), agent.Config{
		Cwd:       dir,
		Model:     liveModel(),
		Ephemeral: true,
		Tools:     []agent.Tool{add},
		Handlers: agent.Handlers{
			Permission: func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
				permMu.Lock()
				asked = append(asked, req.ToolName)
				permMu.Unlock()
				return agent.PermissionResponse{Decision: agent.DecisionAllow}
			},
		},
		ProviderOptions: &claude.SessionOptions{SettingSources: []string{}},
	})
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)

	res, err := sess.Run(ctx, agent.Text("Use the add tool to add 2 and 3. Then create a file named out.txt containing the sum using the Write tool. Reply with only the sum."))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnSucceeded {
		t.Fatalf("status %s: %v", res.Status, res.Err)
	}
	if !strings.Contains(res.Text, "5") {
		t.Errorf("reply %q does not contain 5", res.Text)
	}
	data, err := os.ReadFile(dir + "/out.txt")
	if err != nil || !strings.Contains(string(data), "5") {
		t.Errorf("out.txt = %q, %v", data, err)
	}
	if res.Usage.CostUSD <= 0 || res.Usage.OutputTokens <= 0 {
		t.Errorf("usage not reported: %+v", res.Usage)
	}
	var sawHost, sawWrite, sawDelta bool
	for _, it := range res.Items {
		if it.Tool != nil && it.Tool.Kind == agent.ToolHost && strings.Contains(it.Tool.Output, "5") {
			sawHost = true
		}
		if it.Tool != nil && it.Tool.Name == "Write" && it.Status == agent.ItemDone && len(it.Tool.Changes) == 1 {
			sawWrite = true
		}
	}
	time.Sleep(200 * time.Millisecond)
	for _, ev := range rec.All() {
		if _, ok := ev.(*agent.TextDelta); ok {
			sawDelta = true
		}
	}
	if !sawHost || !sawWrite || !sawDelta {
		t.Errorf("host tool %v, write %v, delta %v; items: %+v", sawHost, sawWrite, sawDelta, res.Items)
	}
	permMu.Lock()
	defer permMu.Unlock()
	if len(asked) == 0 || asked[0] != "Write" {
		t.Errorf("permission requests = %v, want Write (host tools are pre-approved)", asked)
	}
}

func TestLiveInterruptAndControls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, _ := start(ctx, t, claude.New(claude.Options{}), agent.Config{
		Model:           liveModel(),
		Ephemeral:       true,
		PermissionMode:  agent.PermissionBypass,
		ProviderOptions: &claude.SessionOptions{SettingSources: []string{}},
	})
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)

	if len(sess.SupportedModels()) == 0 {
		t.Error("no models in initialize response")
	}
	if err := sess.SetPermissionMode(ctx, agent.PermissionAcceptEdits); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.MCPServerStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.ContextUsage(ctx, false); err != nil {
		t.Fatal(err)
	}

	turn, err := sess.Send(ctx, agent.Text("Run the shell command `sleep 30 && echo done` with Bash and report its output."))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(60 * time.Second)
	for started := false; !started; {
		select {
		case <-deadline:
			t.Fatal("tool call never started")
		case <-time.After(200 * time.Millisecond):
		}
		for _, ev := range rec.All() {
			if s, ok := ev.(*agent.ItemStarted); ok && s.Item.Kind == agent.ItemToolCall {
				started = true
			}
		}
	}
	dropped, err := sess.Send(ctx, agent.Text("Reply with the word DROPPED and nothing else."))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.InterruptTurn(ctx, dropped.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := dropped.Wait(ctx); !errors.Is(err, agent.ErrCancelled) {
		t.Errorf("queued turn after InterruptTurn = %v, want ErrCancelled", err)
	}
	queued, err := sess.Send(ctx, agent.Text("Reply with the word READY and nothing else."))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := turn.Wait(ctx)
	if res == nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnInterrupted {
		t.Errorf("status = %s, want interrupted (%v)", res.Status, res.Err)
	}

	// Interrupt leaves queued input to run.
	res, err = queued.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnSucceeded || !strings.Contains(res.Text, "READY") {
		t.Errorf("after interrupt: %s %q %v", res.Status, res.Text, res.Err)
	}
}

// TestLiveResumeMissingAndBypass spends no tokens: it checks the missing
// session error, the idle Interrupt, and the runtime bypass switch.
func TestLiveResumeMissingAndBypass(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := claude.New(claude.Options{})
	missingCwd := t.TempDir()
	livetest.New(t, p, missingCwd)
	_, err := p.StartSession(ctx, agent.Config{Cwd: missingCwd, Resume: "0badc0de-0000-4000-8000-000000000000", Ephemeral: true,
		ProviderOptions: &claude.SessionOptions{SettingSources: []string{}}})
	if !errors.Is(err, agent.ErrSessionNotFound) {
		t.Errorf("resume missing = %v, want ErrSessionNotFound", err)
	}
	sess, _ := start(ctx, t, p, agent.Config{Model: liveModel(), Ephemeral: true,
		ProviderOptions: &claude.SessionOptions{SettingSources: []string{}, AllowBypassPermissions: true}})
	defer sess.Close()
	if err := sess.Interrupt(ctx); err != nil {
		t.Errorf("idle interrupt = %v", err)
	}
	if err := sess.StopTask(ctx, "no-such-task"); err == nil {
		t.Error("StopTask of an unknown task succeeded")
	}
	if err := sess.SetPermissionMode(ctx, agent.PermissionBypass); err != nil {
		t.Errorf("switch to bypass = %v", err)
	}
}

func TestLiveListModelsAndSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p := claude.New(claude.Options{})
	models, err := p.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatal("no models")
	}
	if _, err := p.ListSessions(ctx, agent.ListSessionsOptions{Limit: 3}); err != nil {
		t.Fatal(err)
	}
}

func TestLiveSubagent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sess, _ := start(ctx, t, claude.New(claude.Options{}), agent.Config{
		Model:           liveModel(),
		Ephemeral:       true,
		PermissionMode:  agent.PermissionBypass,
		ProviderOptions: &claude.SessionOptions{SettingSources: []string{}},
	})
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)
	if _, err := sess.Run(ctx, agent.Text(`Use the Agent tool exactly once with subagent_type "general-purpose" and the prompt "Run the shell command echo PONG and report its output." Then reply briefly.`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Minute)
	for {
		var final *agent.Task
		var nested bool
		for _, ev := range rec.All() {
			switch ev := ev.(type) {
			case *agent.TaskUpdated:
				if ev.Task.Kind == agent.TaskSubagent && ev.Task.Status != agent.TaskRunning {
					task := ev.Task
					final = &task
				}
			case *agent.ItemCompleted:
				if ev.Item.SubagentID != "" && ev.Item.ParentID != "" {
					nested = true
				}
			}
		}
		if final != nil {
			if final.Subagent == nil || final.Subagent.Type != "general-purpose" || final.ToolCallID == "" || final.Status != agent.TaskCompleted || !nested {
				t.Errorf("subagent task %+v subagent %+v nested items %v", final, final.Subagent, nested)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("subagent never finished")
		case <-time.After(time.Second):
		}
	}
}

func TestLiveRewindAndForkAtTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	p := claude.New(claude.Options{})
	cfg := agent.Config{
		Cwd:             t.TempDir(),
		Model:           liveModel(),
		ProviderOptions: &claude.SessionOptions{SettingSources: []string{}, BuiltinTools: []string{}},
	}
	sess, store := start(ctx, t, p, cfg)
	defer sess.Close()
	go (&livetest.Recorder{}).Drain(sess)
	run := func(s agent.Session, prompt string) *agent.TurnResult {
		t.Helper()
		res, err := s.Run(ctx, agent.Text(prompt))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != agent.TurnSucceeded {
			t.Fatalf("status %s: %v", res.Status, res.Err)
		}
		return res
	}
	const ask = "List every word I asked you to remember, in upper case, separated by spaces, and nothing else."
	first := run(sess, "Remember the word APPLE. Reply with OK only.")
	second := run(sess, "Remember the word BANANA. Reply with OK only.")

	fork, err := sess.ForkSession(ctx, agent.Config{ForkAtTurn: first.TurnID})
	if err != nil {
		t.Fatal(err)
	}
	store.Track(fork)
	defer fork.Close()
	go (&livetest.Recorder{}).Drain(fork)
	if got := run(fork, ask).Text; !strings.Contains(got, "APPLE") || strings.Contains(got, "BANANA") {
		t.Errorf("fork at the first turn remembers %q", got)
	}

	id := sess.ID()
	if err := sess.Rewind(ctx, second.TurnID); err != nil {
		t.Fatal(err)
	}
	if sess.ID() != id {
		t.Errorf("rewind changed the session ID from %s to %s", id, sess.ID())
	}
	if got := run(sess, ask).Text; !strings.Contains(got, "APPLE") || strings.Contains(got, "BANANA") {
		t.Errorf("after rewinding the second turn the session remembers %q", got)
	}
	items, err := p.ReadHistory(ctx, id, agent.ReadHistoryOptions{Cwd: cfg.Cwd})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range items {
		texts = append(texts, it.Text)
	}
	if joined := strings.Join(texts, "|"); !strings.Contains(joined, "APPLE") || strings.Contains(joined, "BANANA") {
		t.Errorf("history after rewind = %q", texts)
	}
}

func TestLiveNativeRewind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, _ := start(ctx, t, claude.New(claude.Options{}), agent.Config{
		Model:           liveModel(),
		Ephemeral:       true,
		ProviderOptions: &claude.SessionOptions{SettingSources: []string{}, BuiltinTools: []string{}},
	})
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)
	run := func(prompt string) *agent.TurnResult {
		t.Helper()
		res, err := sess.Run(ctx, agent.Text(prompt))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != agent.TurnSucceeded {
			t.Fatalf("status %s: %v", res.Status, res.Err)
		}
		return res
	}
	run("Remember the word APPLE. Reply with OK only.")
	const second = "Remember the word BANANA. Reply with OK only."
	res := run(second)
	id := sess.ID()
	rw, err := sess.RewindWithResult(ctx, res.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if !rw.Native || rw.PrefillText != second || rw.TargetMessageID != res.TurnID || sess.ID() != id {
		t.Errorf("rewind = %+v, session %s (was %s)", rw, sess.ID(), id)
	}
	removed := false
	for deadline := time.Now().Add(2 * time.Second); !removed && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, ev := range rec.All() {
			if r, ok := ev.(*agent.ItemsRemoved); ok && slices.Contains(r.IDs, res.TurnID) {
				removed = true
			}
		}
	}
	if !removed || !slices.Contains(rw.RemovedItemIDs, res.TurnID) {
		t.Errorf("no ItemsRemoved event for the dropped turn; result lists %v", rw.RemovedItemIDs)
	}
	got := run("List every word I asked you to remember, in upper case, separated by spaces, and nothing else.").Text
	if !strings.Contains(got, "APPLE") || strings.Contains(got, "BANANA") {
		t.Errorf("after the native rewind the session remembers %q", got)
	}
}

func TestLivePrewarmClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p := claude.New(claude.Options{})
	spare, err := p.Prewarm(ctx, agent.Config{
		Model:           liveModel(),
		ProviderOptions: &claude.SessionOptions{SettingSources: []string{}, BuiltinTools: []string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer spare.Close()
	cwd := t.TempDir()
	store := livetest.New(t, p, cwd)
	sess, err := spare.Claim(ctx, agent.Config{Cwd: cwd, PermissionMode: agent.PermissionDefault})
	if err != nil {
		t.Fatal(err)
	}
	store.Track(sess)
	defer sess.Close()
	ev := <-sess.Events()
	st, ok := ev.(*agent.SessionStarted)
	if !ok || st.Info.ID == "" || st.Info.ID != sess.ID() || sess.Info().Cwd == "" {
		t.Errorf("first event %#v, info %+v", ev, sess.Info())
	}
	t.Logf("claimed session %s in %s, capabilities %v", sess.ID(), sess.Info().Cwd, sess.CLICapabilities())
}

func TestLiveCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, err := claude.New(claude.Options{}).Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || st.Version == "" || st.Err != nil || st.Auth == nil {
		t.Fatalf("status = %+v", st)
	}
	t.Logf("version %s auth method %q plan %q authenticated %v", st.Version, st.Auth.Method, st.Auth.Plan, st.Auth.Authenticated)
}
