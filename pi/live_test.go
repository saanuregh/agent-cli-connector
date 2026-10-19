//go:build live

package pi_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/livetest"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// These tests run the real pi CLI and spend tokens. Run them with
//
//	go test -tags live ./pi -run Live -v
//
// PI_TEST_MODEL picks the model; the Pi default is used otherwise.

func liveConfig(t *testing.T) agent.Config {
	return agent.Config{
		Cwd:       t.TempDir(),
		Model:     os.Getenv("PI_TEST_MODEL"),
		Effort:    agent.EffortLow,
		Ephemeral: true,
		// Subagent extensions persist their sessions even when the parent
		// is ephemeral.
		Env: livetest.PiEnv(t),
	}
}
func TestLiveToolsAndGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.PermissionMode = agent.PermissionDefault
	var mu sync.Mutex
	var asked []string
	cfg.Handlers.Permission = func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
		mu.Lock()
		asked = append(asked, req.ToolName)
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
	sess, err := pi.New(pi.Options{}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)

	res, err := sess.Run(ctx, agent.Text("Call the add tool with a=2 and b=3. Then run the bash command `echo 5 > out.txt`. Reply with only the sum."))
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
	// The model may run bash more than once; only bash is gated, never the
	// host tool.
	if len(asked) == 0 || slices.ContainsFunc(asked, func(name string) bool { return name != "bash" }) {
		t.Errorf("permission requests %v, want only bash", asked)
	}
}

func TestLiveDenyInterruptAndQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.PermissionMode = agent.PermissionReadOnly
	sess, err := pi.New(pi.Options{}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)

	res, err := sess.Run(ctx, agent.Text("Run the bash command `touch denied.txt`. If it is refused, reply with the word REFUSED."))
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(cfg.Cwd + "/denied.txt"); statErr == nil {
		t.Error("read-only mode let bash run")
	}
	declined := false
	for _, it := range res.Items {
		if it.Tool != nil && it.Tool.Name == "bash" && it.Status == agent.ItemDeclined {
			declined = true
		}
	}
	if !declined {
		t.Errorf("no declined bash item: %+v", res.Items)
	}

	if err := sess.SetPermissionMode(ctx, agent.PermissionBypass); err != nil {
		t.Fatal(err)
	}
	first, err := sess.Send(ctx, agent.Text("Run the bash command `sleep 30` and then say done."))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := sess.Send(ctx, agent.Text("Reply with the word QUEUED and nothing else."))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(90 * time.Second)
	for started := false; !started; {
		select {
		case <-deadline:
			t.Fatal("bash never started")
		case <-time.After(200 * time.Millisecond):
		}
		for _, ev := range rec.All() {
			if s, ok := ev.(*agent.ItemUpdated); ok && s.Item.Tool != nil && strings.Contains(s.Item.Tool.Command, "sleep") {
				started = true
			}
		}
	}
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	res, err = first.Wait(ctx)
	if res == nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnInterrupted {
		t.Errorf("first turn %s, want interrupted", res.Status)
	}
	res, err = queued.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnSucceeded || !strings.Contains(res.Text, "QUEUED") {
		t.Errorf("queued turn %s %q %v", res.Status, res.Text, res.Err)
	}
	stats, err := sess.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UserMessages < 3 {
		t.Errorf("stats user messages = %d", stats.UserMessages)
	}
}

// TestLiveSubagent needs a subagent extension such as
// @tintinweb/pi-subagents installed in Pi.
func TestLiveSubagent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.PermissionMode = agent.PermissionBypass
	sess, err := pi.New(pi.Options{}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	hasAgent := false
	for _, c := range sess.Info().Commands {
		if strings.Contains(c.Name, "agent") {
			hasAgent = true
		}
	}
	rec := &livetest.Recorder{}
	go rec.Drain(sess)
	if _, err := sess.Run(ctx, agent.Text("Use the Agent tool exactly once, in the foreground, to have a subagent reply with the word PONG. Then reply with what it said.")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Minute)
	for {
		for _, ev := range rec.All() {
			if tu, ok := ev.(*agent.TaskUpdated); ok && tu.Task.Kind == agent.TaskSubagent && tu.Task.Status != agent.TaskRunning {
				if tu.Task.Status != agent.TaskCompleted || tu.Task.Subagent == nil || tu.Task.Subagent.Type == "" {
					t.Errorf("subagent task %+v", tu.Task)
				}
				return
			}
		}
		select {
		case <-deadline:
			if !hasAgent {
				t.Skip("no subagent extension installed in pi")
			}
			t.Fatal("no finished subagent task")
		case <-time.After(time.Second):
		}
	}
}

func TestLiveRewindAndForkAtTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.Ephemeral = false
	opts := &pi.SessionOptions{SessionDir: t.TempDir(), DisableBridge: true}
	cfg.ProviderOptions = opts
	p := pi.New(pi.Options{})
	sess, err := p.StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go (&livetest.Recorder{}).Drain(sess)
	first, err := sess.Run(ctx, agent.Text("Reply with the word ALPHA and nothing else."))
	if err != nil {
		t.Fatal(err)
	}
	second, err := sess.Run(ctx, agent.Text("Reply with the word BETA and nothing else."))
	if err != nil {
		t.Fatal(err)
	}

	fork, err := sess.ForkSession(ctx, agent.Config{ForkAtTurn: first.TurnID, ProviderOptions: opts})
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	go (&livetest.Recorder{}).Drain(fork)
	res, err := fork.Run(ctx, agent.Text("List every word you replied with so far in this conversation, comma separated."))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "ALPHA") || strings.Contains(res.Text, "BETA") {
		t.Errorf("fork at first turn remembers %q (%s %v)", res.Text, res.Status, res.Err)
	}

	if err := sess.Rewind(ctx, second.TurnID); err != nil {
		t.Fatal(err)
	}
	res, err = sess.Run(ctx, agent.Text("List every word you replied with so far in this conversation, comma separated."))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "ALPHA") || strings.Contains(res.Text, "BETA") {
		t.Errorf("after rewind the session remembers %q (%s %v)", res.Text, res.Status, res.Err)
	}
}

func TestLiveRewindInPlace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.Ephemeral = false
	dir := t.TempDir()
	cfg.ProviderOptions = &pi.SessionOptions{SessionDir: dir}
	p := pi.New(pi.Options{})
	sess, err := p.StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go (&livetest.Recorder{}).Drain(sess)
	first, err := sess.Run(ctx, agent.Text("Reply with the word ALPHA and nothing else."))
	if err != nil {
		t.Fatal(err)
	}
	second, err := sess.Run(ctx, agent.Text("Reply with the word BETA and nothing else."))
	if err != nil {
		t.Fatal(err)
	}
	var entry string
	for _, it := range second.Items {
		if it.Kind == agent.ItemUserMessage {
			entry = it.ID
		}
	}
	if entry == "" || strings.HasPrefix(entry, "user-") {
		t.Fatalf("user item has no Pi entry ID: %q", entry)
	}
	id := sess.ID()
	if err := sess.Rewind(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if sess.ID() != id {
		t.Errorf("session ID changed from %s to %s", id, sess.ID())
	}
	res, err := sess.Run(ctx, agent.Text("List every word you replied with so far in this conversation, comma separated."))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "ALPHA") || strings.Contains(res.Text, "BETA") {
		t.Errorf("after rewind the session remembers %q (%s %v)", res.Text, res.Status, res.Err)
	}
	items, err := pi.New(pi.Options{SessionDir: dir}).ReadHistory(ctx, id, agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Kind == agent.ItemUserMessage && strings.Contains(it.Text, "BETA") {
			t.Errorf("persisted branch still holds the rewound prompt: %+v", it)
		}
	}

	// A fork at the first turn truncates in place: it gets the requested
	// ID, and no untruncated copy is left behind.
	const forkID = "01a1aaaa-0000-7000-8000-000000000001"
	fork, err := sess.ForkSession(ctx, agent.Config{ForkAtTurn: first.TurnID, SessionID: forkID})
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	go (&livetest.Recorder{}).Drain(fork)
	if fork.ID() != forkID {
		t.Errorf("fork ID %s, want %s", fork.ID(), forkID)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 2 {
		t.Errorf("session files after the fork = %v, want the original and the fork", files)
	}
	res, err = fork.Run(ctx, agent.Text("List every word you replied with so far in this conversation, comma separated."))
	if err != nil || !strings.Contains(res.Text, "ALPHA") || strings.Contains(res.Text, "BETA") {
		t.Errorf("fork remembers %q, %v", res.Text, err)
	}
}

func TestLiveStructuredOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	// Default mode with an allowlist: the answer tool needs neither a
	// permission nor a place in the list.
	cfg.PermissionMode = agent.PermissionDefault
	cfg.Handlers.Permission = func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
		if req.ToolName == "structured_output" {
			t.Errorf("asked permission for %s", req.ToolName)
		}
		return agent.PermissionResponse{Decision: agent.DecisionDeny}
	}
	cfg.Ephemeral = false
	dir := t.TempDir()
	cfg.ProviderOptions = &pi.SessionOptions{SessionDir: dir, Tools: []string{"read"}}
	sess, err := pi.New(pi.Options{}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go (&livetest.Recorder{}).Drain(sess)
	res, err := sess.Run(ctx, agent.Input{
		Parts:        []agent.Part{{Type: agent.PartText, Text: "What is the capital of France, and its population in millions (a rough integer)?"}},
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"capital":{"type":"string"},"population":{"type":"integer"}},"required":["capital","population"],"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Capital    string
		Population int
	}
	if res.Status != agent.TurnSucceeded || json.Unmarshal(res.StructuredOutput, &out) != nil || !strings.Contains(out.Capital, "Paris") || out.Population <= 0 {
		t.Fatalf("result %s %v, output %s", res.Status, res.Err, res.StructuredOutput)
	}
	res, err = sess.Run(ctx, agent.Input{
		Parts:        []agent.Part{{Type: agent.PartText, Text: "List the first three prime numbers."}},
		OutputSchema: json.RawMessage(`{"type":"array","items":{"type":"integer"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var primes []int
	if res.Status != agent.TurnSucceeded || json.Unmarshal(res.StructuredOutput, &primes) != nil || len(primes) != 3 || primes[0] != 2 {
		t.Fatalf("result %s %v, output %s", res.Status, res.Err, res.StructuredOutput)
	}
	// A plain prompt afterwards runs without the tool.
	res, err = sess.Run(ctx, agent.Text("Reply with OK."))
	if err != nil || res.Status != agent.TurnSucceeded || len(res.StructuredOutput) != 0 {
		t.Fatalf("plain turn %+v, %v", res, err)
	}
	items, err := pi.New(pi.Options{SessionDir: dir}).ReadHistory(ctx, sess.ID(), agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Kind == agent.ItemUserMessage && strings.Contains(it.Text, ":output:") {
			t.Errorf("the output marker reached the transcript: %q", it.Text)
		}
	}
	// Resuming from another directory opens the session without Pi's
	// fork question.
	sess.Close()
	resumed, err := pi.New(pi.Options{}).StartSession(ctx, agent.Config{Cwd: t.TempDir(), Resume: sess.ID(), Env: cfg.Env,
		ProviderOptions: &pi.SessionOptions{SessionDir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.ID() != sess.ID() {
		t.Errorf("resumed %s, want %s", resumed.ID(), sess.ID())
	}
}

// TestLiveBridgeSurvivesNewSession checks that the permission gate and host
// tools still apply after Pi replaced the session, which reloads
// extensions.
func TestLiveBridgeSurvivesNewSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.PermissionMode = agent.PermissionReadOnly
	cfg.Tools = []agent.Tool{{
		Name:        "secret_number",
		Description: "Returns the secret number.",
		// Read-only mode refuses host tools that are not marked read-only.
		ReadOnly: true,
		Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) {
			return agent.TextResult("4242"), nil
		},
	}}
	sess, err := pi.New(pi.Options{}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go (&livetest.Recorder{}).Drain(sess)
	if cancelled, err := sess.NewSession(ctx, ""); err != nil || cancelled {
		t.Fatalf("new session: cancelled %v, %v", cancelled, err)
	}
	res, err := sess.Run(ctx, agent.Text("First call the secret_number tool. Then run the shell command `touch written.txt` with bash. Reply with the secret number."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Cwd, "written.txt")); err == nil {
		t.Error("bash ran in read-only mode after the session was replaced")
	}
	if !strings.Contains(res.Text, "4242") {
		t.Errorf("host tool unavailable after the session was replaced: %q", res.Text)
	}
}
