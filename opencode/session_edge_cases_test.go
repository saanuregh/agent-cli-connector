package opencode_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/opencode"
)

// Regression tests for the review of the opencode package. The first six
// are the reviewer's reproductions, asserting the fixed behavior.

// A steer whose turn is interrupted while its request is in flight is
// taken back, fails with ErrCancelled, and leaves later input free to run.
func TestSteerCancelledWithItsTurn(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	first := send(t, sess, agent.Text("first"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	gate := make(chan struct{})
	f.on("POST /api/session/"+fakeSession+"/prompt", func(r *fakeRequest) (int, any) {
		if r.Body["delivery"] == "steer" {
			<-gate
		}
		return 200, data(map[string]any{"id": r.Body["id"]})
	})
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) { return 204, nil })
	done := make(chan struct{})
	var steered *agent.Turn
	var steerErr error
	go func() {
		defer close(done)
		steered, steerErr = sess.Send(context.Background(), agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer})
	}()
	steerReq := f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	waitTurn(t, first)
	close(gate)
	<-done
	if steerErr != nil {
		t.Fatal(steerErr)
	}
	if del := f.next("DELETE", "/api/session/"+fakeSession+"/inbox/*"); !strings.HasSuffix(del.Path, steerReq.Body["id"].(string)) {
		t.Fatalf("took back %s", del.Path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := steered.Wait(ctx); !errors.Is(err, agent.ErrCancelled) {
		t.Fatalf("steer turn = %v", err)
	}
	later := send(t, sess, agent.Text("later"))
	if r := f.next("POST", "/api/session/"+fakeSession+"/prompt"); r.Body["text"] != "later" {
		t.Fatalf("later prompt = %v", r.Body)
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, later)
	if err := sess.Rewind(context.Background(), later.ID()); err != nil {
		t.Fatalf("rewind after the steer: %v", err)
	}
}

// A background subagent's request survives the parent turn's end and can
// still be answered.
func TestBackgroundRequestOutlivesTurn(t *testing.T) {
	f := newFakeServer(t)
	f.on("POST /api/session/ses_child/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("go"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.tool.input.started", map[string]any{"assistantMessageID": "m", "id": "call1", "name": "subagent"})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession})
	f.emit("session.tool.progress", map[string]any{"assistantMessageID": "m", "id": "call1", "metadata": map[string]any{"sessionID": "ses_child"}})
	f.emit("session.tool.success", map[string]any{"assistantMessageID": "m", "id": "call1", "content": []any{map[string]any{"type": "text", "text": "started"}}, "metadata": map[string]any{"sessionID": "ses_child", "status": "running"}})
	f.emit("session.tool.input.started", map[string]any{"sessionID": "ses_child", "assistantMessageID": "k", "id": "call_k", "name": "shell"})
	f.emit("permission.asked", map[string]any{"id": "per_1", "sessionID": "ses_child", "action": "shell", "resources": []any{"ls"}})
	req := waitFor[*agent.PermissionRequest](t, ev)
	f.emit("session.execution.succeeded", map[string]any{})
	res := waitTurn(t, turn)
	select {
	case <-req.Done():
		t.Fatal("background subagent request withdrawn at the parent turn's end")
	case <-time.After(100 * time.Millisecond):
	}
	for _, it := range res.Items {
		if it.ID == "ses_child/call_k" {
			t.Fatal("background subagent tool item completed with the parent turn")
		}
	}
	if err := req.Allow(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.next("POST", "/api/session/ses_child/permission/per_1/reply")
}

// Ephemeral never deletes a session the library resumed.
func TestEphemeralKeepsResumedSession(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{Resume: fakeSession, Ephemeral: true})
	_ = sess.Close()
	if n := f.count("DELETE", "/api/session/"+fakeSession); n != 0 {
		t.Fatalf("deleted the resumed session %d times", n)
	}
}

// Run's context ending while its input is queued drops that input and
// leaves the running turn alone.
func TestRunCancelDropsQueuedInput(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	first := send(t, sess, agent.Text("first"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := sess.Run(ctx, agent.Text("queued")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run = %v", err)
	}
	if n := f.count("POST", "/api/session/"+fakeSession+"/interrupt"); n != 0 {
		t.Fatalf("interrupted the running turn %d times", n)
	}
	f.emit("session.execution.succeeded", map[string]any{})
	if res := waitTurn(t, first); res.Status != agent.TurnSucceeded {
		t.Fatalf("first = %+v", res)
	}
	time.Sleep(100 * time.Millisecond)
	if n := f.count("POST", "/api/session/"+fakeSession+"/prompt"); n != 1 {
		t.Fatalf("prompts = %d; the cancelled input started", n)
	}
}

// A queued input's model switches when the input starts, not under the
// running turn.
func TestQueuedModelAppliedAtStart(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	send(t, sess, agent.Text("first"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	queued := send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("second")}, Model: "opencode/text-only"})
	time.Sleep(50 * time.Millisecond)
	if n := f.count("POST", "/api/session/"+fakeSession+"/model"); n != 0 {
		t.Fatalf("model switched %d times under the running turn", n)
	}
	if _, err := sess.Send(context.Background(), agent.Input{Parts: []agent.Part{agent.TextPart("x")}, Model: "opencode/nope"}); err == nil {
		t.Fatal("an unknown model was accepted for queued input")
	}
	f.emit("session.execution.succeeded", map[string]any{})
	if r := f.next("POST", "/api/session/"+fakeSession+"/model"); fakeproc.Str(r.Body, "model", "id") != "text-only" {
		t.Fatalf("model = %v", r.Body)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/prompt"); r.Body["id"] != queued.ID() {
		t.Fatalf("prompt = %v", r.Body)
	}
}

// Close drops queued input instead of starting it while closing.
func TestCloseDropsQueuedInput(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	send(t, sess, agent.Text("first"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	queued := send(t, sess, agent.Text("queued"))
	f.on("POST /api/session/"+fakeSession+"/interrupt", func(*fakeRequest) (int, any) {
		f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
		time.Sleep(100 * time.Millisecond)
		return 200, map[string]any{"interrupted": true}
	})
	_ = sess.Close()
	time.Sleep(200 * time.Millisecond)
	if n := f.count("POST", "/api/session/"+fakeSession+"/prompt"); n != 1 {
		t.Fatalf("prompts = %d; queued input started during Close", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := queued.Wait(ctx); !errors.Is(err, agent.ErrClosed) {
		t.Fatalf("queued turn = %v", err)
	}
}

func TestInterruptTurn(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	var _ agent.TurnInterrupter = sess
	first := send(t, sess, agent.Text("first"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	queued := send(t, sess, agent.Text("queued"))
	ctx := context.Background()
	if err := sess.InterruptTurn(ctx, queued.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := queued.Wait(ctx); !errors.Is(err, agent.ErrCancelled) {
		t.Fatalf("queued = %v", err)
	}
	if err := sess.InterruptTurn(ctx, first.ID()); err != nil {
		t.Fatal(err)
	}
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	waitTurn(t, first)
	if err := sess.InterruptTurn(ctx, first.ID()); err != nil {
		t.Fatalf("ended turn: %v", err)
	}
}

func ruleList(t *testing.T, v any) []string {
	t.Helper()
	var out []string
	list, _ := v.([]any)
	for _, r := range list {
		out = append(out, fakeproc.Str(r, "action")+":"+fakeproc.Str(r, "resource")+"="+fakeproc.Str(r, "effect"))
	}
	return out
}

// Session grants never lift the denies of readOnly and plan mode.
func TestGrantsKeepModeDenies(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/agent", func(*fakeRequest) (int, any) {
		return 200, map[string]any{"data": []any{map[string]any{"id": "plan", "name": "Plan", "mode": "primary", "permissions": []any{
			map[string]any{"action": "edit", "resource": "*", "effect": "deny"},
			map[string]any{"action": "edit", "resource": "/plans/*", "effect": "allow"},
		}, "request": map[string]any{"headers": map[string]any{"Authorization": "secret"}}},
			map[string]any{"id": "build", "name": "Build", "mode": "primary", "permissions": []any{
				map[string]any{"action": "*", "resource": "*", "effect": "allow"},
			}}}}
	})
	f.on("POST /api/session/"+fakeSession+"/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionReadOnly})
	f.next("POST", "/api/session")
	send(t, sess, agent.Text("x"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "edit", "resources": []any{"a.go"}, "save": []any{"*"}})
	if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	// readOnly's deny decides edits, so the grant answers no ask and
	// writes nothing.
	if n := f.count("PATCH", "/api/session/"+fakeSession); n != 0 {
		t.Fatalf("readOnly rules changed %d times", n)
	}
	if err := sess.SetPermissionMode(context.Background(), agent.PermissionPlan); err != nil {
		t.Fatal(err)
	}
	rules := ruleList(t, f.next("PATCH", "/api/session/"+fakeSession).Body["permissions"])
	deny, path := slices.Index(rules, "edit:*=deny"), slices.Index(rules, "edit:/plans/*=allow")
	if deny < 0 || path < deny || slices.Contains(rules, "edit:*=allow") {
		t.Fatalf("plan rules = %v", rules)
	}
	agents, err := sess.Agents(context.Background())
	if err != nil || len(agents) != 2 || strings.Contains(string(agents[0].Raw), "secret") {
		t.Fatalf("agents = %+v, %v", agents, err)
	}
}

// AllowSession on a subagent's request grants it to that subagent session.
func TestChildGrant(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/ses_child", func(*fakeRequest) (int, any) {
		info := sessionInfo("ses_child")
		info["permissions"] = []any{map[string]any{"action": "shell", "resource": "*", "effect": "ask"}}
		return 200, data(info)
	})
	f.on("POST /api/session/ses_child/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	send(t, sess, agent.Text("go"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "agent": "general"})
	f.emit("permission.asked", map[string]any{"id": "per_1", "sessionID": "ses_child", "action": "shell", "resources": []any{"ls"}, "save": []any{"ls *"}})
	if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The child's rules are the parent's, built for the general agent,
	// with the grant right after the ask it answers.
	var rules []string
	for !slices.Contains(rules, "shell:ls *=allow") {
		rules = ruleList(t, f.next("PATCH", "/api/session/ses_child").Body["permissions"])
	}
	if i := slices.Index(rules, "shell:ls *=allow"); i < 1 || rules[i-1] != "shell:*=ask" || !slices.Contains(rules, "subagent:*=deny") {
		t.Fatalf("child rules = %v", rules)
	}
	if n := f.count("PATCH", "/api/session/"+fakeSession); n != 0 {
		t.Fatalf("parent rules changed %d times", n)
	}
}

// The session environment on an attached server is the whole environment.
func TestSessionEnvironment(t *testing.T) {
	t.Setenv("OPENCODE_REVIEW_KEEP", "1")
	t.Setenv("OPENCODE_REVIEW_DROP", "1")
	f := newFakeServer(t)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		h.Send(map[string]any{"url": f.srv.URL})
		h.WaitClosed()
	})
	p := opencode.New(opencode.Options{Spawner: spawner, SharedServer: true, HelperDir: t.TempDir()})
	sess, err := p.Start(context.Background(), agent.Config{Cwd: "/work", Env: map[string]string{"EXTRA": "x"}, UnsetEnv: []string{"OPENCODE_REVIEW_DROP"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	vars := f.next("PUT", "/api/session/"+fakeSession+"/environment").Body["variables"].(map[string]any)
	if vars["EXTRA"] != "x" || vars["OPENCODE_REVIEW_KEEP"] != "1" || vars["PATH"] == nil || vars["OPENCODE_REVIEW_DROP"] != nil {
		t.Fatalf("environment keys = %d, EXTRA %v, KEEP %v, DROP %v", len(vars), vars["EXTRA"], vars["OPENCODE_REVIEW_KEEP"], vars["OPENCODE_REVIEW_DROP"])
	}
}

// Sessions of one provider share an MCP server they both registered; the
// last to close removes it.
func TestSharedMCPRegistration(t *testing.T) {
	f := newFakeServer(t)
	f.on("PUT /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
	f.on("DELETE /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
	p := f.provider()
	cfg := agent.Config{Cwd: "/work", MCPServers: map[string]agent.MCPServer{"docs": agent.StdioServer("docs-mcp")}}
	a, err := p.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count("PUT", "/api/experimental/mcp/docs"); n != 1 {
		t.Fatalf("registered %d times", n)
	}
	_ = a.Close()
	if n := f.count("DELETE", "/api/experimental/mcp/docs"); n != 0 {
		t.Fatal("removed a server another session uses")
	}
	_ = b.Close()
	f.next("DELETE", "/api/experimental/mcp/docs")
}

func TestHelperDirectory(t *testing.T) {
	var seenDir string
	var seenEnv []string
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		seenDir, seenEnv = h.Cmd.Dir, h.Cmd.Env
		h.Stderr("no server")
	})
	helperEnvSet := func() bool {
		return slices.Contains(seenEnv, "OPENCODE_DISABLE_PROJECT_CONFIG=1") && slices.Contains(seenEnv, "OPENCODE_CONFIG_PROJECT_DISABLE=1")
	}
	// The default folder under the user cache directory is used, even in
	// a shared tree: helper servers load no project config from parents.
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	p := opencode.New(opencode.Options{Spawner: spawner})
	_, _ = p.ListModels(context.Background())
	if want := filepath.Join(cache, "agent-cli-connector", "opencode"); seenDir != want || !helperEnvSet() {
		t.Fatalf("helper ran in %q (want %q), project config off = %v", seenDir, want, helperEnvSet())
	}
	// An explicit folder is used and tightened.
	dir := filepath.Join(t.TempDir(), "helper")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	p = opencode.New(opencode.Options{Spawner: spawner, HelperDir: dir})
	for _, call := range []func() error{
		func() error { _, err := p.ListModels(context.Background()); return err },
		func() error { _, err := p.Check(context.Background()); return err },
		func() error { _, err := p.ListSessions(context.Background(), agent.ListSessionsOptions{}); return err },
		func() error {
			_, err := p.ReadHistory(context.Background(), "ses_x", agent.ReadHistoryOptions{})
			return err
		},
		func() error { return p.DeleteSession(context.Background(), "ses_x", "") },
		func() error { return p.Call(context.Background(), "GET", "/api/info", nil, nil) },
	} {
		seenDir, seenEnv = "", nil
		_ = call()
		if seenDir == "" && seenEnv == nil {
			continue // Check spawns "--version" first, which fails here
		}
		if seenDir != dir || !helperEnvSet() {
			t.Fatalf("helper ran in %q, project config off = %v", seenDir, helperEnvSet())
		}
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("helper directory mode = %v, %v", info.Mode(), err)
	}
	// A symlink is refused.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	p = opencode.New(opencode.Options{Spawner: spawner, HelperDir: link})
	if _, err := p.ListModels(context.Background()); err == nil || !strings.Contains(err.Error(), "not a plain directory") {
		t.Fatalf("symlinked helper directory: %v", err)
	}
}

func TestModelRawHasNoCredentials(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/model", func(*fakeRequest) (int, any) {
		return 200, map[string]any{"data": []any{map[string]any{"id": "m", "providerID": "p", "enabled": true,
			"settings": map[string]any{"apiKey": "sk-secret"}, "headers": map[string]any{"x": "sk-secret"}, "body": map[string]any{"k": "sk-secret"},
			"variants": []any{map[string]any{"id": "high", "headers": map[string]any{"x": "sk-secret"}}}}}}
	})
	models, err := f.provider().ListModels(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("models = %+v, %v", models, err)
	}
	if strings.Contains(string(models[0].Raw), "sk-secret") || !strings.Contains(string(models[0].Raw), `"high"`) {
		t.Fatalf("raw = %s", models[0].Raw)
	}
}

func TestCompactFailure(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	done := make(chan error, 1)
	go func() { done <- sess.Compact(context.Background(), "") }()
	f.next("POST", "/api/session/"+fakeSession+"/compact")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.compaction.started", map[string]any{"reason": "manual", "recent": ""})
	f.emit("session.compaction.failed", map[string]any{"reason": "manual", "error": map[string]any{"type": "x", "message": "too small"}})
	f.emit("session.execution.succeeded", map[string]any{})
	var te *agent.TurnError
	if err := <-done; !errors.As(err, &te) || te.Code != "compaction_failed" {
		t.Fatalf("compact = %v", err)
	}
}

func TestEffortOnDefaultModelAndSlashParts(t *testing.T) {
	f := newFakeServer(t)
	f.on("POST /api/session", func(r *fakeRequest) (int, any) {
		info := sessionInfo(fakeSession)
		delete(info, "model")
		return 200, data(info)
	})
	sess, _ := startFake(t, f, agent.Config{Effort: agent.EffortHigh})
	if r := f.next("POST", "/api/session/"+fakeSession+"/model"); fakeproc.Str(r.Body, "model", "id") != "test-model" || fakeproc.Str(r.Body, "model", "variant") != "high" {
		t.Fatalf("effort = %v", r.Body)
	}
	turn := send(t, sess, agent.Text("/docs explain"))
	if r := f.next("POST", "/api/session/"+fakeSession+"/prompt"); fakeproc.Str(r.Body, "skills", 0, "id") != "docs" || r.Body["text"] != "explain" {
		t.Fatalf("skill prompt = %v", r.Body)
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
	send(t, sess, agent.Text("/summarize"))
	f.next("POST", "/api/session/"+fakeSession+"/compact")
}

// Resuming a session adopts its running execution and shows the requests
// it waits on; a reconnect withdraws requests the server dropped.
func TestResumeReconciles(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/active", func(*fakeRequest) (int, any) {
		return 200, data(map[string]any{fakeSession: map[string]any{"type": "running"}})
	})
	perms := []any{map[string]any{"id": "per_old", "sessionID": fakeSession, "action": "shell", "resources": []any{"ls"}}}
	f.on("GET /api/session/"+fakeSession+"/permission", func(*fakeRequest) (int, any) { return 200, data(perms) })
	f.on("GET /api/session/"+fakeSession+"/form", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session", func(*fakeRequest) (int, any) { return 200, map[string]any{"data": []any{}, "cursor": map[string]any{}} })
	sess, ev := startFake(t, f, agent.Config{Resume: fakeSession})
	started := waitFor[*agent.TurnStarted](t, ev)
	req := waitFor[*agent.PermissionRequest](t, ev)
	if req.ID != "per_old" {
		t.Fatalf("request = %+v", req)
	}
	f.mu.Lock()
	perms = []any{}
	f.mu.Unlock()
	f.emit("session.execution.succeeded", map[string]any{})
	completed := waitFor[*agent.TurnCompleted](t, ev)
	if completed.Result.TurnID != started.TurnID {
		t.Fatalf("turns %s and %s", started.TurnID, completed.Result.TurnID)
	}
	_ = sess
}

func TestReconnectWithdrawsDroppedRequests(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession+"/permission", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session/"+fakeSession+"/form", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	sess, ev := startFake(t, f, agent.Config{})
	send(t, sess, agent.Text("x"))
	f.emit("session.execution.started", map[string]any{})
	f.on("GET /api/session/active", func(*fakeRequest) (int, any) {
		return 200, data(map[string]any{fakeSession: map[string]any{"type": "running"}})
	})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "shell", "resources": []any{"ls"}})
	req := waitFor[*agent.PermissionRequest](t, ev)
	f.dropStreams()
	select {
	case <-req.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("request the server dropped stays pending")
	}
}

// Interrupting a turn that waits only for steers also stops OpenCode.
func TestInterruptHeldTurn(t *testing.T) {
	f := newFakeServer(t)
	// Nothing runs: OpenCode had not started the steers' execution yet.
	f.on("POST /api/session/"+fakeSession+"/interrupt", func(*fakeRequest) (int, any) { return 200, map[string]any{"interrupted": false} })
	sess, _ := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("stop")}, Delivery: agent.DeliverSteer})
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.succeeded", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) { return 204, nil })
	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if res := waitTurn(t, turn); res.Status != agent.TurnInterrupted {
		t.Fatalf("result = %+v", res)
	}
	// The steers are taken back before the interrupt.
	f.next("DELETE", "/api/session/"+fakeSession+"/inbox/*")
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
}

func TestRevertRemovesItems(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("hi"))
	id := turn.ID()
	f.emit("session.inbox.enqueued", map[string]any{"inboxID": id, "item": map[string]any{"type": "user", "payload": map[string]any{"text": "hi"}}})
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.inbox.delivered", map[string]any{"inboxID": id})
	f.emit("session.text.started", map[string]any{"assistantMessageID": "msg_a", "ordinal": 0})
	f.emit("session.text.ended", map[string]any{"assistantMessageID": "msg_a", "ordinal": 0, "text": "yo"})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
	f.emit("session.revert.committed", map[string]any{"to": id})
	removed := waitFor[*agent.ItemsRemoved](t, ev)
	if !slices.Equal(removed.IDs, []string{id, "msg_a:text:0"}) {
		t.Fatalf("removed = %v", removed.IDs)
	}
}

func TestFormConditions(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	send(t, sess, agent.Text("ask"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("form.created", map[string]any{"sessionID": "", "form": map[string]any{
		"id": "frm_1", "sessionID": fakeSession, "title": "Q",
		"fields": []any{
			map[string]any{"key": "more", "type": "boolean", "default": true},
			map[string]any{"key": "why", "type": "string", "when": []any{map[string]any{"key": "more", "op": "eq", "value": true}}},
			map[string]any{"key": "n", "type": "number", "default": 3},
			map[string]any{"key": "secret", "type": "string", "hidden": true},
		},
	}})
	req := waitFor[*agent.UserInputRequest](t, ev)
	if len(req.Questions) != 3 || req.Questions[0].Default != "Yes" || req.Questions[2].Default != "3" || !strings.Contains(req.Questions[1].Text, "only when more") {
		t.Fatalf("questions = %+v", req.Questions)
	}
	if err := req.Answer(context.Background(), map[string][]string{"more": {"No"}, "why": {"because"}, "n": {"4"}}); err != nil {
		t.Fatal(err)
	}
	answer := f.next("POST", "/api/session/"+fakeSession+"/form/frm_1/reply").Body["answer"].(map[string]any)
	if answer["more"] != false || answer["why"] != nil || answer["n"] != float64(4) {
		t.Fatalf("answer = %v", answer)
	}
}

func TestServerURLPathPrefix(t *testing.T) {
	f := newFakeServer(t)
	proxy := http.NewServeMux()
	proxy.Handle("/oc/", http.StripPrefix("/oc", f.srv.Config.Handler))
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	p := opencode.New(opencode.Options{ServerURL: srv.URL + "/oc/", Password: "secret"})
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// When the private server dies, the session releases its host tool
// listener.
func TestServerExitReleasesHostTools(t *testing.T) {
	f := newFakeServer(t)
	var hostURL string
	f.on("PUT /api/experimental/mcp/*", func(r *fakeRequest) (int, any) {
		hostURL = fakeproc.Str(r.Body, "config", "url")
		return 204, nil
	})
	stop := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		h.Send(map[string]any{"url": f.srv.URL})
		<-stop
	})
	tool := agent.Tool{Name: "t", Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) {
		return agent.TextResult("x"), nil
	}}
	sess, err := opencode.New(opencode.Options{Spawner: spawner}).Start(context.Background(), agent.Config{Cwd: "/work", Tools: []agent.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-sess.Done()
	if resp, err := http.Post(hostURL, "application/json", strings.NewReader(`{}`)); err == nil {
		resp.Body.Close()
		t.Fatal("host tool listener still serves after the server exited")
	}
}
