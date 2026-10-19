package opencode_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/opencode"
)

// fakeServer is a scripted OpenCode 2 server: it answers the HTTP routes
// the provider uses and streams the events a test emits.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	password string
	routes   map[string]func(r *fakeRequest) (int, any)
	subs     []chan []byte
	requests chan *fakeRequest
	log      []*fakeRequest
	streams  int
}

type fakeRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
	Raw    []byte
}

const fakeSession = "ses_fake"

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, routes: map[string]func(*fakeRequest) (int, any){}, requests: make(chan *fakeRequest, 256)}
	f.defaults()
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.close)
	return f
}

func (f *fakeServer) close() {
	f.mu.Lock()
	for _, ch := range f.subs {
		close(ch)
	}
	f.subs = nil
	f.mu.Unlock()
	f.srv.CloseClientConnections()
	f.srv.Close()
}

// providerWithURL returns a provider attached to serverURL.
func (f *fakeServer) providerWithURL(serverURL string) *opencode.Provider {
	return opencode.New(opencode.Options{ServerURL: serverURL, Password: "secret"})
}

// provider returns a provider attached to the fake. It allows host
// tools, which an attached server refuses by default.
func (f *fakeServer) provider() *opencode.Provider {
	return opencode.New(opencode.Options{ServerURL: f.srv.URL, Password: "secret", AllowHostToolsOnAttachedServer: true})
}

// workDir and otherDir are the fixtures' project folders as the library
// resolves them: absolute, which on Windows adds the current drive.
var (
	workDir  = absPath("/work")
	otherDir = absPath("/other")
)

func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return abs
}

func data(v any) map[string]any { return map[string]any{"data": v} }

func sessionInfo(id string) map[string]any {
	return map[string]any{
		"id": id, "projectID": "p", "cost": 0, "title": "Fake",
		"model":    map[string]any{"providerID": "opencode", "id": "test-model", "variant": "default"},
		"tokens":   map[string]any{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
		"time":     map[string]any{"created": 1000, "updated": 2000},
		"location": map[string]any{"directory": workDir},
	}
}

func (f *fakeServer) defaults() {
	f.on("GET /api/info", func(*fakeRequest) (int, any) { return 200, map[string]any{"version": "2.0.32"} })
	f.on("POST /api/session", func(r *fakeRequest) (int, any) {
		info := sessionInfo(fakeSession)
		if perms, ok := r.Body["permissions"]; ok {
			info["permissions"] = perms
		}
		return 200, data(info)
	})
	f.on("GET /api/session/"+fakeSession, func(*fakeRequest) (int, any) { return 200, data(sessionInfo(fakeSession)) })
	f.on("GET /api/model", func(*fakeRequest) (int, any) {
		return 200, map[string]any{"data": []any{
			map[string]any{"id": "test-model", "providerID": "opencode", "name": "Test", "enabled": true,
				"variants":     []any{map[string]any{"id": "low"}, map[string]any{"id": "high"}},
				"capabilities": map[string]any{"input": []any{"text", "image"}}, "limit": map[string]any{"context": 1000, "output": 100}},
			map[string]any{"id": "text-only", "providerID": "opencode", "name": "Text", "enabled": true,
				"variants": []any{}, "capabilities": map[string]any{"input": []any{"text"}}, "limit": map[string]any{"context": 500, "output": 100}},
		}}
	})
	f.on("GET /api/model/default", func(*fakeRequest) (int, any) {
		return 200, map[string]any{"data": map[string]any{"id": "test-model", "providerID": "opencode"}}
	})
	f.on("GET /api/command", func(*fakeRequest) (int, any) {
		return 200, map[string]any{"data": []any{map[string]any{"name": "review", "description": "review changes"}}}
	})
	f.on("GET /api/skill", func(*fakeRequest) (int, any) {
		return 200, map[string]any{"data": []any{map[string]any{"id": "docs", "name": "docs", "path": "/builtin/docs.md"}}}
	})
	f.on("GET /api/mcp", func(*fakeRequest) (int, any) { return 200, map[string]any{"data": []any{}} })
	f.on("GET /api/agent", func(*fakeRequest) (int, any) { return 200, data(fakeAgents()) })
	ok := func(*fakeRequest) (int, any) { return 204, nil }
	inbox := func(r *fakeRequest) (int, any) {
		return 200, data(map[string]any{"id": r.Body["id"], "type": "user", "delivery": "steer"})
	}
	f.on("POST /api/session/"+fakeSession+"/prompt", inbox)
	f.on("POST /api/session/"+fakeSession+"/compact", inbox)
	f.on("POST /api/session/"+fakeSession+"/command", ok)
	f.on("POST /api/session/"+fakeSession+"/interrupt", func(*fakeRequest) (int, any) { return 200, map[string]any{"interrupted": true} })
	f.on("PATCH /api/session/"+fakeSession, ok)
	f.on("POST /api/session/"+fakeSession+"/model", ok)
	f.on("POST /api/session/"+fakeSession+"/agent", ok)
	f.on("POST /api/session/"+fakeSession+"/revert/stage", func(r *fakeRequest) (int, any) { return 200, data(map[string]any{"messageID": r.Body["messageID"]}) })
	f.on("POST /api/session/"+fakeSession+"/revert/commit", ok)
	f.on("POST /api/session/"+fakeSession+"/fork", func(*fakeRequest) (int, any) { return 200, data(sessionInfo("ses_fork")) })
	f.on("GET /api/session/ses_fork", func(*fakeRequest) (int, any) { return 200, data(sessionInfo("ses_fork")) })
	f.on("DELETE /api/session/"+fakeSession, ok)
	f.on("GET /api/session/active", func(*fakeRequest) (int, any) { return 200, data(map[string]any{}) })
	f.on("POST /api/session/"+fakeSession+"/move", ok)
	f.on("PUT /api/session/*", ok)
	f.on("PATCH /api/session/*", ok)
	f.on("POST /api/session/ses_fork/*", ok)
	f.on("PUT /api/experimental/session/*", ok)
}

// fakeAgents are OpenCode 2.0.32's build, plan, explore, and general
// agents for a project at /work, as GET /api/agent lists them.
func fakeAgents() []any {
	r := func(action, resource, effect string) any {
		return map[string]any{"action": action, "resource": resource, "effect": effect}
	}
	env := func(rules ...any) []any {
		return append([]any{r("*", "*", "allow"), r("read", "*.env", "ask"), r("read", "*.env.*", "ask"), r("read", "*.env.example", "allow")}, rules...)
	}
	a := func(id, mode string, rules []any) any {
		return map[string]any{"id": id, "name": id, "mode": mode, "permissions": rules}
	}
	return []any{
		a("build", "primary", env(r("question", "*", "allow"), r("browser", "*", "deny"))),
		a("plan", "primary", env(r("question", "*", "allow"), r("edit", "*", "deny"), r("edit", "/work/.opencode/plan/*", "allow"), r("browser", "*", "deny"))),
		a("general", "subagent", env(r("question", "*", "deny"), r("subagent", "*", "deny"), r("browser", "*", "deny"))),
		a("explore", "subagent", env(r("*", "*", "deny"), r("shell", "*", "allow"), r("grep", "*", "allow"), r("glob", "*", "allow"),
			r("webfetch", "*", "allow"), r("websearch", "*", "allow"), r("read", "*", "allow"), r("read", "*.env", "ask"),
			r("read", "*.env.*", "ask"), r("read", "*.env.example", "allow"), r("subagent", "*", "deny"),
			r("external_directory", "*", "allow"), r("browser", "*", "deny"))),
	}
}

// on sets the handler of "METHOD /path"; a path ending in "*" matches any
// path with that prefix.
func (f *fakeServer) on(route string, h func(*fakeRequest) (int, any)) {
	f.mu.Lock()
	f.routes[route] = h
	f.mu.Unlock()
}

func (f *fakeServer) handler(method, path string) func(*fakeRequest) (int, any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h := f.routes[method+" "+path]; h != nil {
		return h
	}
	best := ""
	var found func(*fakeRequest) (int, any)
	for route, h := range f.routes {
		prefix, ok := strings.CutSuffix(route, "*")
		if ok && strings.HasPrefix(method+" "+path, prefix) && len(prefix) > len(best) {
			best, found = prefix, h
		}
	}
	return found
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	password := f.password
	f.mu.Unlock()
	if user, pass, ok := r.BasicAuth(); !ok || user != "opencode" || password != "" && pass != password {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"_tag":"UnauthorizedError","message":"Authentication required"}`)
		return
	}
	if r.URL.Path == "/api/event" {
		f.stream(w, r)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	req := &fakeRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Raw: raw}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req.Body)
	}
	f.mu.Lock()
	f.log = append(f.log, req)
	f.mu.Unlock()
	select {
	case f.requests <- req:
	default:
		f.t.Errorf("fake: request buffer full")
	}
	h := f.handler(r.Method, r.URL.Path)
	if h == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = fmt.Fprintf(w, `{"_tag":"SessionNotFoundError","message":"Session not found: %s"}`, r.URL.Path)
		return
	}
	status, body := h(req)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func (f *fakeServer) stream(w http.ResponseWriter, r *http.Request) {
	ch := make(chan []byte, 256)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.streams++
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	_, _ = io.WriteString(w, "data: {\"type\":\"server.connected\",\"data\":{}}\n\n: heartbeat\n\n")
	fl.Flush()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", ev)
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// emit sends an event to every subscriber.
func (f *fakeServer) emit(typ string, d map[string]any) {
	if _, ok := d["sessionID"]; !ok {
		d["sessionID"] = fakeSession
	}
	raw, _ := json.Marshal(map[string]any{"id": "evt_" + typ, "type": typ, "data": d})
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		ch <- raw
	}
}

// dropStreams ends every open event stream.
func (f *fakeServer) dropStreams() {
	f.mu.Lock()
	for _, ch := range f.subs {
		close(ch)
	}
	f.subs = nil
	f.mu.Unlock()
}

func (f *fakeServer) streamCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams
}

// next returns the next request matching method and path, skipping others.
func (f *fakeServer) next(method, path string) *fakeRequest {
	f.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case r := <-f.requests:
			if r.Method == method && (r.Path == path || strings.HasSuffix(path, "*") && strings.HasPrefix(r.Path, strings.TrimSuffix(path, "*"))) {
				return r
			}
		case <-timeout:
			f.t.Fatalf("fake: no %s %s request", method, path)
			return nil
		}
	}
}

// count returns how many requests matched method and path so far.
func (f *fakeServer) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.log {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// startFake starts a session on the fake and drains its events.
func startFake(t *testing.T, f *fakeServer, cfg agent.Config) (*opencode.Session, *events) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if cfg.Cwd == "" {
		cfg.Cwd = workDir
	}
	sess, err := f.provider().StartSession(ctx, cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	ev := &events{}
	go ev.drain(sess)
	return sess, ev
}

type events struct {
	mu   sync.Mutex
	list []agent.Event
}

func (e *events) drain(sess agent.Session) {
	for ev := range sess.Events() {
		e.mu.Lock()
		e.list = append(e.list, ev)
		e.mu.Unlock()
	}
}

func (e *events) all() []agent.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]agent.Event(nil), e.list...)
}

// wait returns the first event for which match is true, waiting for it.
func (e *events) wait(t *testing.T, match func(agent.Event) bool) agent.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range e.all() {
			if match(ev) {
				return ev
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("event not seen")
	return nil
}

func waitFor[T agent.Event](t *testing.T, e *events) T {
	t.Helper()
	ev := e.wait(t, func(ev agent.Event) bool { _, ok := ev.(T); return ok })
	return ev.(T)
}

func countOf[T agent.Event](e *events) int {
	n := 0
	for _, ev := range e.all() {
		if _, ok := ev.(T); ok {
			n++
		}
	}
	return n
}

func waitTurn(t *testing.T, turn *agent.Turn) *agent.TurnResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := turn.Wait(ctx)
	if res == nil {
		t.Fatalf("turn: %v", err)
	}
	return res
}

func send(t *testing.T, sess agent.Session, in agent.Input) *agent.Turn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := sess.Send(ctx, in)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return turn
}
