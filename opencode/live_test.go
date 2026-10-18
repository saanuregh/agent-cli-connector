//go:build live

package opencode_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/livetest"
	"github.com/saanuregh/agent-cli-connector/opencode"
)

// These tests run the real OpenCode 2 CLI with a free model. Run them with
//
//	go test -tags live ./opencode -run Live -v
//
// OPENCODE_TEST_MODEL picks the model (default opencode/deepseek-v4.1-flash-free)
// and OPENCODE_BIN the executable (default "opencode" on PATH). Every server
// runs with XDG and HOME directories under the test's temporary directory,
// so the tests never touch the user's OpenCode data or start its
// background service.

const defaultModel = "opencode/deepseek-v4.1-flash-free"

func liveModel() string {
	if m := os.Getenv("OPENCODE_TEST_MODEL"); m != "" {
		return m
	}
	return defaultModel
}

func liveProvider(t *testing.T) *opencode.Provider {
	t.Helper()
	return opencode.New(liveOptions(t))
}

func liveOptions(t *testing.T) opencode.Options {
	t.Helper()
	bin := os.Getenv("OPENCODE_BIN")
	if bin == "" {
		path, err := exec.LookPath("opencode")
		if err != nil {
			t.Skip("opencode not found; set OPENCODE_BIN or PATH")
		}
		bin = path
	}
	home := t.TempDir()
	env := map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, "config"),
		"XDG_DATA_HOME":   filepath.Join(home, "data"),
		"XDG_STATE_HOME":  filepath.Join(home, "state"),
		"XDG_CACHE_HOME":  filepath.Join(home, "cache"),
	}
	// Provider calls start helper servers in a folder the test owns: the
	// default folder under the user cache directory is in the real home,
	// which the tests must not touch. Like the sessions' Cwd, it is under
	// the shared temporary directory, which these tests accept.
	return opencode.Options{Path: bin, Env: env, HelperDir: t.TempDir()}
}

func liveConfig(t *testing.T) agent.Config {
	return agent.Config{Cwd: t.TempDir(), Model: liveModel()}
}
func count[T agent.Event](events []agent.Event) int {
	n := 0
	for _, ev := range events {
		if _, ok := ev.(T); ok {
			n++
		}
	}
	return n
}

func start(t *testing.T, p *opencode.Provider, cfg agent.Config) (*opencode.Session, *livetest.Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sess, err := p.StartSession(ctx, cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	rec := &livetest.Recorder{}
	go rec.Drain(sess)
	return sess, rec
}

func run(t *testing.T, sess agent.Session, in agent.Input) *agent.TurnResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := sess.Run(ctx, in)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != agent.TurnSucceeded {
		t.Fatalf("turn %s: %v", res.Status, res.Err)
	}
	return res
}

func TestLiveCheckAndModels(t *testing.T) {
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := p.Check(ctx)
	if err != nil || st.Err != nil || !st.Installed {
		t.Fatalf("check = %+v, %v", st, err)
	}
	if !strings.HasPrefix(st.Version, "2.") || st.Auth == nil || !st.Auth.Authenticated {
		t.Fatalf("check = %+v auth %+v", st, st.Auth)
	}
	models, err := p.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range models {
		found = found || m.ID == liveModel()
	}
	if !found {
		t.Fatalf("model %s not listed among %d models", liveModel(), len(models))
	}
}

func TestLiveTurnHistoryAndSessions(t *testing.T) {
	p := liveProvider(t)
	cfg := liveConfig(t)
	cfg.AppendSystemPrompt = "When asked for a codeword, the codeword is MARMALADE."
	sess, rec := start(t, p, cfg)
	res := run(t, sess, agent.Text("What is the codeword? Reply with the codeword only."))
	if !strings.Contains(strings.ToUpper(res.Text), "MARMALADE") {
		t.Errorf("text = %q", res.Text)
	}
	if res.Usage.TotalTokens == 0 || res.Steps == 0 {
		t.Errorf("usage = %+v steps %d", res.Usage, res.Steps)
	}
	if !strings.HasPrefix(res.TurnID, "msg_") {
		t.Errorf("turn ID = %q", res.TurnID)
	}
	res2 := run(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("Now reply with the word: ok")}, ClientID: "client-2"})
	events := rec.All()
	if count[*agent.TextDelta](events) == 0 || count[*agent.TurnStarted](events) < 2 {
		t.Errorf("events: %d deltas, %d turn starts", count[*agent.TextDelta](events), count[*agent.TurnStarted](events))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	items, err := sess.Messages(ctx)
	if err != nil || len(items) == 0 {
		t.Fatalf("messages: %d, %v", len(items), err)
	}
	id := sess.ID()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	history, err := p.ReadHistory(ctx, id, agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var users []agent.Item
	for _, it := range history {
		if it.Kind == agent.ItemUserMessage {
			users = append(users, it)
		}
	}
	if len(users) != 2 || users[0].ID != res.TurnID || users[1].ID != res2.TurnID || users[1].ClientID != "client-2" {
		t.Fatalf("user items = %+v", users)
	}
	last, err := p.ReadHistory(ctx, id, agent.ReadHistoryOptions{Limit: 1})
	if err != nil || len(last) != 1 || last[0].TurnID != res2.TurnID {
		t.Fatalf("last page = %+v, %v", last, err)
	}
	list, err := p.ListSessions(ctx, agent.ListSessionsOptions{Cwd: cfg.Cwd})
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("sessions = %+v, %v", list, err)
	}
	if err := p.DeleteSession(ctx, id, cfg.Cwd); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteSession(ctx, id, cfg.Cwd); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

func TestLivePermissionQuestionAndHostTool(t *testing.T) {
	p := liveProvider(t)
	cfg := liveConfig(t)
	cfg.PermissionMode = agent.PermissionDefault
	var mu sync.Mutex
	var asked []string
	var questions int
	cfg.Handlers.Permission = func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
		mu.Lock()
		asked = append(asked, req.ToolName+":"+req.Command)
		mu.Unlock()
		return agent.PermissionResponse{Decision: agent.DecisionAllow}
	}
	cfg.Handlers.UserInput = func(ctx context.Context, req *agent.UserInputRequest) agent.UserInputResponse {
		mu.Lock()
		questions++
		mu.Unlock()
		answers := map[string][]string{}
		for _, q := range req.Questions {
			if len(q.Options) > 0 {
				answers[q.ID] = []string{q.Options[len(q.Options)-1].Label}
			}
		}
		return agent.UserInputResponse{Answers: answers}
	}
	var calls int
	cfg.Tools = []agent.Tool{{
		Name:        "add",
		Description: "Adds two integers and returns their sum.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
		Handler: func(ctx context.Context, call agent.ToolCallRequest) (agent.ToolResult, error) {
			var in struct{ A, B int }
			if err := json.Unmarshal(call.Arguments, &in); err != nil {
				return agent.ToolResult{}, err
			}
			mu.Lock()
			calls++
			mu.Unlock()
			return agent.JSONResult(map[string]int{"sum": in.A + in.B})
		},
	}}
	sess, rec := start(t, p, cfg)
	res := run(t, sess, agent.Text("Use your shell tool to run exactly this command: echo live-check-42 . Then reply with its output."))
	if !strings.Contains(res.Text, "live-check-42") {
		t.Errorf("shell text = %q", res.Text)
	}
	mu.Lock()
	if len(asked) == 0 || !strings.Contains(asked[0], "echo live-check-42") {
		t.Errorf("asked = %v", asked)
	}
	mu.Unlock()
	// The free model sometimes looks for host tools in OpenCode's code
	// mode catalog, where they deliberately are not (code mode would bypass
	// their permission rules), and reports them missing: one retry, then
	// the host tool checks are skipped rather than failed.
	gotCalls := 0
	for attempt := 0; attempt < 2 && gotCalls == 0; attempt++ {
		res = run(t, sess, agent.Text("Call the tool whose name ends with _add with a=20 and b=22, then reply with the sum only."))
		mu.Lock()
		gotCalls = calls
		mu.Unlock()
		t.Logf("host tool answer: %q", res.Text)
	}
	hostSkipped := gotCalls == 0
	res = run(t, sess, agent.Text("Use your question tool to ask me to pick red or blue (two options). Then reply with my choice only."))
	mu.Lock()
	gotQuestions := questions
	mu.Unlock()
	if gotQuestions == 0 || !strings.Contains(strings.ToLower(res.Text), "blue") {
		t.Errorf("questions = %d, text = %q", gotQuestions, res.Text)
	}
	var hostItem bool
	for _, ev := range rec.All() {
		if c, ok := ev.(*agent.ItemCompleted); ok && c.Item.Tool != nil && c.Item.Tool.Kind == agent.ToolHost {
			hostItem = true
		}
	}
	switch {
	case hostSkipped:
		t.Skip("the model did not call the host tool; host tool checks skipped")
	case !hostItem:
		t.Error("the host tool ran but no host tool item was reported")
	}
}

func TestLiveInterruptAndQueue(t *testing.T) {
	p := liveProvider(t)
	sess, _ := start(t, p, liveConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	first, err := sess.Send(ctx, agent.Text("Write a 1500 word essay about lighthouses."))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := sess.Send(ctx, agent.Text("Reply with the word: queued-ok"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := first.Wait(ctx)
	if res == nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnInterrupted {
		t.Errorf("first status = %s", res.Status)
	}
	res, err = queued.Wait(ctx)
	if err != nil || res.Status != agent.TurnSucceeded || !strings.Contains(res.Text, "queued-ok") {
		t.Fatalf("queued = %+v, %v", res, err)
	}
}

func TestLiveForkAndRewind(t *testing.T) {
	p := liveProvider(t)
	sess, _ := start(t, p, liveConfig(t))
	first := run(t, sess, agent.Text("Remember the number 7. Reply with: noted"))
	second := run(t, sess, agent.Text("Reply with the word: second"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	forked, err := sess.ForkSession(ctx, agent.Config{ForkAtTurn: first.TurnID})
	if err != nil {
		t.Fatal(err)
	}
	defer forked.Close()
	if forked.ID() == sess.ID() {
		t.Fatal("fork kept the session ID")
	}
	users := func(id string) []string {
		items, err := p.ReadHistory(ctx, id, agent.ReadHistoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			if it.Kind == agent.ItemUserMessage {
				out = append(out, it.ID)
			}
		}
		return out
	}
	if got := users(forked.ID()); len(got) != 1 {
		t.Errorf("fork user messages = %v", got)
	}
	if err := sess.Rewind(ctx, second.TurnID); err != nil {
		t.Fatal(err)
	}
	if got := users(sess.ID()); len(got) != 1 || got[0] != first.TurnID {
		t.Errorf("rewound user messages = %v", got)
	}
	res := run(t, forked, agent.Text("What number did I ask you to remember? Reply with the digit only."))
	if !strings.Contains(res.Text, "7") {
		t.Errorf("fork answer = %q", res.Text)
	}
}

func TestLiveSubagent(t *testing.T) {
	p := liveProvider(t)
	sess, rec := start(t, p, liveConfig(t))
	run(t, sess, agent.Text("Use your subagent tool to launch a general subagent whose prompt is: reply with exactly the word kiwi. Then tell me what it answered."))
	var done *agent.Task
	for _, ev := range rec.All() {
		if u, ok := ev.(*agent.TaskUpdated); ok && u.Task.Kind == agent.TaskSubagent && u.Task.Status == agent.TaskCompleted {
			task := u.Task
			done = &task
		}
	}
	if done == nil {
		t.Skip("the model answered without starting a subagent")
	}
	if !strings.Contains(strings.ToLower(done.Output), "kiwi") || done.ToolCallID == "" {
		t.Errorf("task = %+v", done)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tasks, err := p.ListSubagents(ctx, sess.ID(), agent.ListSubagentsOptions{})
	if err != nil || len(tasks) == 0 || tasks[0].ID != done.ID {
		t.Fatalf("subagents = %+v, %v", tasks, err)
	}
	items, err := p.ReadHistory(ctx, sess.ID(), agent.ReadHistoryOptions{SubagentID: done.ID})
	if err != nil || len(items) == 0 {
		t.Fatalf("subagent history = %d items, %v", len(items), err)
	}
}

// TestLiveApprovalPlanAndSteer checks protocol behavior, not model
// wording: a model that does not call the tool after a retry skips the
// tool checks, and its answers are logged rather than asserted.
func TestLiveApprovalPlanAndSteer(t *testing.T) {
	p := liveProvider(t)
	cfg := liveConfig(t)
	var mu sync.Mutex
	var asked []*agent.PermissionRequest
	calls := 0
	cfg.Handlers.Permission = func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
		mu.Lock()
		asked = append(asked, req)
		mu.Unlock()
		return agent.PermissionResponse{Decision: agent.DecisionAllowSession}
	}
	cfg.Tools = []agent.Tool{{
		Name:          "secret",
		Description:   "Returns the secret word.",
		NeedsApproval: true,
		Handler: func(ctx context.Context, call agent.ToolCallRequest) (agent.ToolResult, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return agent.TextResult("PAPAYA"), nil
		},
	}}
	counts := func() (approvals, handled int) {
		mu.Lock()
		defer mu.Unlock()
		return len(asked), calls
	}
	sess, rec := start(t, p, cfg)
	// callTool asks for one more tool call, once more if the model does not
	// make it, and reports whether the handler ran.
	// Each attempt is bounded: a model that stalls is skipped, but a turn
	// stuck on an unanswered request would be a library failure.
	callTool := func(want int) bool {
		for attempt := 0; attempt < 2; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			res, err := sess.Run(ctx, agent.Text("Call the tool whose name ends with _secret (it takes no arguments), then reply with the word it returned only."))
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				if pending := sess.PendingRequests(); len(pending) > 0 {
					t.Fatalf("turn stuck on %d unanswered requests, the first %T %s", len(pending), pending[0], pending[0].RequestID())
				}
				t.Skip("the model stalled on the host tool call")
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			t.Logf("tool answer (%s): %q", res.Status, res.Text)
			if _, handled := counts(); handled >= want {
				return true
			}
		}
		return false
	}
	if !callTool(1) {
		t.Skip("the model did not call the host tool")
	}
	approvals, _ := counts()
	if approvals != 1 {
		t.Fatalf("approvals after the first call = %d", approvals)
	}
	mu.Lock()
	t.Logf("host tool permission: tool %q title %q", asked[0].ToolName, asked[0].Title)
	mu.Unlock()
	// The session grant covers the next call.
	if callTool(2) {
		if approvals, _ := counts(); approvals != 1 {
			t.Errorf("after the session grant: approvals = %d", approvals)
		}
	} else {
		t.Log("the model did not call the host tool again; grant check skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := sess.SetPermissionMode(ctx, agent.PermissionPlan); err != nil {
		t.Fatal(err)
	}
	if sess.Info().PermissionMode != agent.PermissionPlan {
		t.Errorf("mode = %q", sess.Info().PermissionMode)
	}
	run(t, sess, agent.Text("Reply with the word: planned"))
	if err := sess.SetPermissionMode(ctx, agent.PermissionBypass); err != nil {
		t.Fatal(err)
	}

	turn, err := sess.Send(ctx, agent.Text("Count slowly from 1 to 200, one number per line."))
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, rec, func(ev agent.Event) bool {
		d, ok := ev.(*agent.TextDelta)
		return ok && d.TurnID == turn.ID()
	})
	steered, err := sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("Stop counting and reply with only the word: steered")}, Delivery: agent.DeliverSteer})
	if err != nil {
		t.Fatal(err)
	}
	res, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := steered.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("steer: turn %s %q; steered turn %s %q same=%v", res.Status, res.Text, res2.Status, res2.Text, steered == turn)
	if res.Status != agent.TurnSucceeded || res2.Status != agent.TurnSucceeded {
		t.Errorf("steer statuses = %s, %s", res.Status, res2.Status)
	}
	var steerItem bool
	for _, ev := range rec.All() {
		if c, ok := ev.(*agent.ItemCompleted); ok && c.Item.Kind == agent.ItemUserMessage && strings.Contains(c.Item.Text, "Stop counting") {
			steerItem = true
		}
	}
	if !steerItem {
		t.Error("no user item for the steer")
	}
	// In history, a steer delivered into a turn belongs to that turn:
	// OpenCode records one idle per execution, after the steer.
	if steered == turn {
		items, err := p.ReadHistory(ctx, sess.ID(), agent.ReadHistoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Kind == agent.ItemUserMessage && strings.Contains(it.Text, "Stop counting") && it.TurnID != turn.ID() {
				t.Errorf("history puts the steer in turn %s, not %s", it.TurnID, turn.ID())
			}
		}
	}
}

// waitEvent waits up to two minutes for an event match accepts.
func waitEvent(t *testing.T, rec *livetest.Recorder, match func(agent.Event) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		for _, ev := range rec.All() {
			if match(ev) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("event not seen")
}

// TestLiveSharedServer runs two sessions on one shared server. The session
// environment replaces the whole environment of the session's tools, so a
// shell command sees both the inherited PATH and Config.Env.
func TestLiveSharedServer(t *testing.T) {
	opts := liveOptions(t)
	opts.SharedServer = true
	p := opencode.New(opts)
	cfg := liveConfig(t)
	cfg.PermissionMode = agent.PermissionBypass
	cfg.Env = map[string]string{"ACC_LIVE_MARK": "shared-42"}
	a, _ := start(t, p, cfg)
	b, _ := start(t, p, liveConfig(t))
	res := run(t, a, agent.Text("Use your shell tool to run exactly: echo mark=$ACC_LIVE_MARK path=$(command -v ls) . Then reply with its output only."))
	if !strings.Contains(res.Text, "shared-42") || !strings.Contains(res.Text, "/ls") {
		t.Errorf("shell output = %q", res.Text)
	}
	res = run(t, b, agent.Text("Reply with the word: ok"))
	if !strings.Contains(strings.ToLower(res.Text), "ok") {
		t.Errorf("second session = %q", res.Text)
	}
	if a.Server() != b.Server() {
		t.Error("the sessions run on different servers")
	}
}

// TestLiveHelperIgnoresProjectConfig plants a config with an MCP server and
// a plugin above the helper folder: provider calls load neither, while a
// session in a project folder loads that project's config.
func TestLiveHelperIgnoresProjectConfig(t *testing.T) {
	opts := liveOptions(t)
	base := t.TempDir()
	markers := filepath.Join(base, "markers")
	if err := os.MkdirAll(filepath.Join(base, ".opencode", "plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(markers, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mcp := func(name, marker string) string {
		return `{"mcp":{"servers":{"` + name + `":{"type":"local","command":["sh","-c","touch ` + filepath.Join(markers, marker) + `; sleep 30"]}}}}`
	}
	write(filepath.Join(base, "opencode.json"), mcp("planted", "parent-config"))
	write(filepath.Join(base, ".opencode", "plugin", "x.js"), `import fs from "node:fs"; fs.writeFileSync("`+filepath.Join(markers, "parent-plugin")+`", "x"); export default { id: "planted" };`)
	opts.HelperDir = filepath.Join(base, "a", "helper")
	p := opencode.New(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := p.ListModels(ctx); err != nil {
		t.Fatal(err)
	}
	var mcps struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := p.Call(ctx, "GET", "/api/mcp", nil, &mcps); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if entries, _ := os.ReadDir(markers); len(entries) > 0 || len(mcps.Data) > 0 {
		t.Fatalf("helper loaded project config: markers %v, MCP servers %v", entries, mcps.Data)
	}
	// Positive control: a session-style server in the same folder, which
	// loads project config, runs the planted plugin and MCP server.
	srv, err := p.Connect(ctx, opts.HelperDir)
	if err != nil {
		t.Fatal(err)
	}
	loc := "?location%5Bdirectory%5D=" + url.QueryEscape(opts.HelperDir)
	_ = srv.Client().Call(ctx, "GET", "/api/agent"+loc, nil, nil)
	_ = srv.Client().Call(ctx, "GET", "/api/mcp"+loc, nil, nil)
	for _, marker := range []string{"parent-plugin", "parent-config"} {
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(markers, marker)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				_ = srv.Close()
				t.Fatalf("control: %s did not load without the variables, so the check above proves nothing", marker)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	_ = srv.Close()
	// A session server loads its project's config.
	project := t.TempDir()
	write(filepath.Join(project, "opencode.json"), mcp("projectmcp", "project-config"))
	cfg := liveConfig(t)
	cfg.Cwd = project
	sess, _ := start(t, p, cfg)
	found := false
	for _, st := range sess.Info().MCPServers {
		found = found || st.Name == "projectmcp"
	}
	if !found {
		t.Fatalf("session did not load its project config: MCP servers %+v", sess.Info().MCPServers)
	}
}
