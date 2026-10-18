//go:build live

package codex_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/livetest"
)

// These tests run the real codex app-server and spend tokens. Run them with
//
//	go test -tags live ./codex -run Live -v
//
// CODEX_TEST_MODEL and CODEX_TEST_EFFORT pick the model and effort.

func liveConfig(t *testing.T) agent.Config {
	effort := agent.Effort(os.Getenv("CODEX_TEST_EFFORT"))
	if effort == "" {
		effort = agent.EffortLow
	}
	return agent.Config{
		Cwd:       t.TempDir(),
		Model:     os.Getenv("CODEX_TEST_MODEL"),
		Effort:    effort,
		Ephemeral: true,
	}
}
func TestLiveListModels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	models, err := codex.New(codex.Options{}).ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		t.Logf("%s default=%v efforts=%v", m.ID, m.IsDefault, m.Efforts)
	}
	if len(models) == 0 {
		t.Fatal("no models")
	}
}

func TestLiveToolsAndApprovals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.PermissionMode = "untrusted"
	var mu sync.Mutex
	var asked []agent.PermissionKind
	cfg.Handlers.Permission = func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
		mu.Lock()
		asked = append(asked, req.Kind)
		mu.Unlock()
		return agent.PermissionResponse{Decision: agent.DecisionAllow}
	}
	cfg.Tools = []agent.Tool{{
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
	}}
	sess, err := codex.New(codex.Options{}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)

	res, err := sess.Run(ctx, agent.Text("Call the add tool with a=2 and b=3. Then run the shell command `echo $((2+3)) > out.txt`. Reply with only the sum."))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnSucceeded {
		t.Fatalf("status %s: %v", res.Status, res.Err)
	}
	if !strings.Contains(res.Text, "5") {
		t.Errorf("reply %q does not contain 5", res.Text)
	}
	var sawHost, sawShell, sawDelta bool
	for _, it := range res.Items {
		if it.Tool != nil && it.Tool.Kind == agent.ToolHost && strings.Contains(it.Tool.Output, "5") {
			sawHost = true
		}
		if it.Tool != nil && it.Tool.Kind == agent.ToolShell && it.Status == agent.ItemDone {
			sawShell = true
		}
	}
	for _, ev := range rec.All() {
		if _, ok := ev.(*agent.TextDelta); ok {
			sawDelta = true
		}
	}
	if !sawHost || !sawShell || !sawDelta {
		t.Errorf("host %v shell %v delta %v; items %+v", sawHost, sawShell, sawDelta, res.Items)
	}
	if res.Usage.OutputTokens <= 0 {
		t.Errorf("usage not reported: %+v", res.Usage)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 {
		t.Error("no approval requested in untrusted mode")
	}
}

func TestLiveInterruptQueueAndStructuredOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.PermissionMode = agent.PermissionBypass
	sess, err := codex.New(codex.Options{}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)

	first, err := sess.Send(ctx, agent.Text("Run the shell command `sleep 30` and then say done."))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := sess.Send(ctx, agent.Input{
		Parts:        []agent.Part{agent.TextPart("Return a JSON object with field answer set to 42.")},
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(90 * time.Second)
	for started := false; !started; {
		select {
		case <-deadline:
			t.Fatal("command never started")
		case <-time.After(200 * time.Millisecond):
		}
		for _, ev := range rec.All() {
			if s, ok := ev.(*agent.ItemStarted); ok && s.Item.Kind == agent.ItemToolCall {
				started = true
			}
		}
	}
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := first.Wait(ctx)
	if res == nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnInterrupted {
		t.Errorf("first turn status = %s, want interrupted", res.Status)
	}
	res, err = queued.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnSucceeded {
		t.Fatalf("queued turn %s: %v", res.Status, res.Err)
	}
	var out struct{ Answer int }
	if err := json.Unmarshal(res.StructuredOutput, &out); err != nil || out.Answer != 42 {
		t.Errorf("structured output %s: %v", res.StructuredOutput, err)
	}
	if queued.ID() != res.TurnID || strings.HasPrefix(res.TurnID, "queued-") {
		t.Errorf("queued turn id %q vs result %q", queued.ID(), res.TurnID)
	}
}

func TestLiveSubagent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.PermissionMode = agent.PermissionBypass
	sess, err := codex.New(codex.Options{}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)
	res, err := sess.Run(ctx, agent.Text("Spawn exactly one subagent and ask it to reply with the word PONG. Wait for it, then reply with what it said."))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnSucceeded {
		t.Fatalf("turn %s: %v", res.Status, res.Err)
	}
	var final *agent.Task
	var nested, spawned bool
	for _, ev := range rec.All() {
		switch ev := ev.(type) {
		case *agent.TaskUpdated:
			if ev.Task.Kind == agent.TaskSubagent {
				task := ev.Task
				final = &task
			}
		case *agent.ItemStarted:
			if ev.Item.Tool != nil && ev.Item.Tool.Kind == agent.ToolSubagent {
				spawned = true
			}
		case *agent.ItemCompleted:
			if ev.Item.SubagentID != "" {
				nested = true
			}
			if ev.Item.Tool != nil && ev.Item.Tool.Kind == agent.ToolSubagent {
				spawned = true
			}
		}
	}
	if !spawned {
		t.Skipf("the model answered without spawning a subagent: %q", res.Text)
	}
	if final == nil || final.Status != agent.TaskCompleted || !strings.Contains(final.Output, "PONG") || final.Subagent == nil || final.Subagent.Model == "" || !nested {
		t.Errorf("subagent task %+v nested items %v", final, nested)
	}
}

func TestLiveCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, err := codex.New(codex.Options{}).Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || st.Version == "" || st.Err != nil || st.Auth == nil {
		t.Fatalf("status %+v", st)
	}
	t.Logf("version %s auth method=%s plan=%s authenticated=%v", st.Version, st.Auth.Method, st.Auth.Plan, st.Auth.Authenticated)
}

func TestLiveRewindAndForkAtTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.PermissionMode = agent.PermissionReadOnly
	// Ephemeral threads cannot list turns or revert; delete them instead.
	cfg.Ephemeral = false
	p := codex.New(codex.Options{})
	store := livetest.New(t, p, cfg.Cwd)
	sess, err := p.StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store.Track(sess)
	defer sess.Close()
	go (&livetest.Recorder{}).Drain(sess)
	var ids []string
	for _, word := range []string{"ONE", "TWO"} {
		res, err := sess.Run(ctx, agent.Text("Reply with exactly the word "+word+" and nothing else."))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != agent.TurnSucceeded {
			t.Fatalf("turn %s: %v", res.Status, res.Err)
		}
		ids = append(ids, res.TurnID)
	}
	turnIDs := func(s *codex.Session) []string {
		page, err := s.ListTurns(ctx, codex.ListTurnsOptions{Ascending: true, ItemsView: "notLoaded"})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, turn := range page.Turns {
			out = append(out, turn.ID)
		}
		return out
	}
	if got := turnIDs(sess); len(got) != 2 {
		t.Fatalf("turns before = %v, want %v", got, ids)
	}

	forked, err := sess.ForkSession(ctx, agent.Config{ForkAtTurn: ids[0]})
	if err != nil {
		t.Fatal(err)
	}
	store.Track(forked)
	defer forked.Close()
	go (&livetest.Recorder{}).Drain(forked)
	if got := turnIDs(forked.(*codex.Session)); len(got) != 1 {
		t.Errorf("forked turns = %v, want 1", got)
	}

	if err := sess.Rewind(ctx, ids[1]); err != nil {
		t.Fatal(err)
	}
	if got := turnIDs(sess); len(got) != 1 || got[0] != ids[0] {
		t.Errorf("turns after rewind = %v, want [%s]", got, ids[0])
	}
	all, err := p.ReadHistory(ctx, sess.ID(), agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	last, err := p.ReadHistory(ctx, sess.ID(), agent.ReadHistoryOptions{Limit: 1})
	if err != nil || len(all) < 2 || len(last) != 1 || last[0].ID != all[len(all)-1].ID {
		t.Errorf("history page %+v of %d items, %v", last, len(all), err)
	}
}

// TestLiveMissingThread spends no tokens: stored-session reads of an
// unknown thread report agent.ErrSessionNotFound.
func TestLiveMissingThread(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p := codex.New(codex.Options{})
	defer p.Close()
	const missing = "019a0000-0000-7000-8000-000000000000"
	if _, err := p.ReadHistory(ctx, missing, agent.ReadHistoryOptions{}); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Errorf("ReadHistory = %v", err)
	}
	if _, err := p.ListSubagents(ctx, missing, agent.ListSubagentsOptions{}); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Errorf("ListSubagents = %v", err)
	}
}

func TestLiveCompactWaits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p := codex.New(codex.Options{})
	defer p.Close()
	cfg := liveConfig(t)
	store := livetest.New(t, p, cfg.Cwd)
	sess, err := p.StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store.Track(sess)
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)
	if _, err := sess.Run(ctx, agent.Text("Reply with the word READY and nothing else.")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Compact(ctx, "keep it short"); err != nil {
		t.Fatal(err)
	}
	// Events are delivered asynchronously, so the finished Compaction event
	// may land just after Compact returns.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		for _, ev := range rec.All() {
			if c, ok := ev.(*agent.Compaction); ok && c.Done {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no finished Compaction event")
		}
	}
}
