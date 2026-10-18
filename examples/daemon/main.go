// Command daemon serves agent sessions over HTTP, so other programs and
// local tools can drive any harness without linking the library.
// Sessions outlive the connections that use them: a client can disconnect,
// come back, replay what it missed, and answer requests that waited.
//
//	go run ./examples/daemon -addr 127.0.0.1:8080
//
//	# start a session; the reply carries its id
//	curl -s localhost:8080/sessions -d '{"provider":"claude","cwd":"/tmp"}'
//	# follow its events (Server-Sent Events; reconnects resume with Last-Event-ID)
//	curl -N localhost:8080/sessions/$ID/events
//	# send a message; "wait":true returns the turn's result instead
//	curl -s localhost:8080/sessions/$ID/messages -d '{"text":"List the files here"}'
//	# answer a permission, question, or elicitation the agent is waiting on
//	curl -s localhost:8080/sessions/$ID/requests
//	curl -s localhost:8080/sessions/$ID/requests/$RID -d '{"decision":"allow"}'
//	# end it
//	curl -s -X DELETE localhost:8080/sessions/$ID
//
// The API has no authentication and runs agents with your credentials, so
// it listens on loopback by default. Do not expose it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/opencode"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	keep := flag.Int("keep", 1000, "events kept per session for clients that reconnect")
	flag.Parse()

	// Cancelling root ends every request, including open event streams,
	// so Shutdown does not wait for them.
	root, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	d := &daemon{
		registry: agent.NewRegistry(claude.New(claude.Options{}), codex.New(codex.Options{}), pi.New(pi.Options{}), opencode.New(opencode.Options{})),
		sessions: map[string]*hosted{},
		keep:     *keep,
	}
	srv := &http.Server{
		Addr:        *addr,
		Handler:     d.routes(),
		BaseContext: func(net.Listener) context.Context { return root },
	}
	go func() {
		<-root.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("listening on http://%s", *addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	d.closeAll()
}

type daemon struct {
	registry *agent.Registry
	keep     int

	mu       sync.Mutex
	sessions map[string]*hosted
}

func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /providers", d.providers)
	mux.HandleFunc("GET /sessions", d.list)
	mux.HandleFunc("POST /sessions", d.start)
	mux.HandleFunc("GET /sessions/{id}", d.withSession(d.info))
	mux.HandleFunc("DELETE /sessions/{id}", d.withSession(d.close))
	mux.HandleFunc("POST /sessions/{id}/messages", d.withSession(d.send))
	mux.HandleFunc("POST /sessions/{id}/interrupt", d.withSession(d.interrupt))
	mux.HandleFunc("GET /sessions/{id}/events", d.withSession(d.events))
	mux.HandleFunc("GET /sessions/{id}/requests", d.withSession(d.requests))
	mux.HandleFunc("POST /sessions/{id}/requests/{rid}", d.withSession(d.answer))
	return mux
}

// providers reports each harness's install and sign-in status.
func (d *daemon) providers(w http.ResponseWriter, r *http.Request) {
	type status struct {
		Name   string
		Status *agent.ProviderStatus `json:",omitempty"`
		Error  string                `json:",omitempty"`
	}
	names := d.registry.Names()
	out := make([]status, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		out[i].Name = name
		p, _ := d.registry.Get(name)
		checker, ok := p.(agent.Checker)
		if !ok {
			continue
		}
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			st, err := checker.Check(ctx)
			if err != nil {
				out[i].Error = err.Error()
			}
			out[i].Status = st
		})
	}
	wg.Wait()
	reply(w, http.StatusOK, out)
}

func (d *daemon) list(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	sessions := maps.Clone(d.sessions)
	d.mu.Unlock()
	out := []map[string]any{}
	for id, h := range sessions {
		out = append(out, map[string]any{"id": id, "info": brief(h.sess.Info())})
	}
	reply(w, http.StatusOK, out)
}

// start starts a session. Requests nobody answers wait until a client
// answers them through /requests.
func (d *daemon) start(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider  string               `json:"provider"`
		Model     string               `json:"model"`
		Effort    agent.Effort         `json:"effort"`
		Mode      agent.PermissionMode `json:"mode"`
		Cwd       string               `json:"cwd"`
		Resume    string               `json:"resume"`
		Ephemeral bool                 `json:"ephemeral"`
		Title     string               `json:"title"`
	}
	if !decode(w, r, &body) {
		return
	}
	// The session outlives this request, so it must not use its context.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	sess, err := d.registry.Start(ctx, body.Provider, agent.Config{
		Model:          body.Model,
		Effort:         body.Effort,
		PermissionMode: body.Mode,
		Cwd:            body.Cwd,
		Resume:         body.Resume,
		Ephemeral:      body.Ephemeral,
		Title:          body.Title,
	})
	if err != nil {
		reply(w, http.StatusBadRequest, errorBody(err))
		return
	}
	// The ID can change later, such as after a /clear, so the key is fixed
	// now.
	id := sess.ID()
	h := &hosted{sess: sess, keep: d.keep, changed: make(chan struct{})}
	d.mu.Lock()
	d.sessions[id] = h
	d.mu.Unlock()
	go func() {
		h.record()
		d.mu.Lock()
		delete(d.sessions, id)
		d.mu.Unlock()
	}()
	reply(w, http.StatusCreated, map[string]any{"id": id, "info": brief(sess.Info()), "capabilities": sess.Capabilities()})
}

func (d *daemon) withSession(handle func(http.ResponseWriter, *http.Request, *hosted)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		h := d.sessions[r.PathValue("id")]
		d.mu.Unlock()
		if h == nil {
			reply(w, http.StatusNotFound, map[string]string{"error": "no such session"})
			return
		}
		handle(w, r, h)
	}
}

func (d *daemon) info(w http.ResponseWriter, _ *http.Request, h *hosted) {
	reply(w, http.StatusOK, map[string]any{"info": brief(h.sess.Info()), "capabilities": h.sess.Capabilities()})
}

func (d *daemon) close(w http.ResponseWriter, _ *http.Request, h *hosted) {
	if err := h.sess.Close(); err != nil {
		reply(w, http.StatusInternalServerError, errorBody(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// send submits a message. Without "wait" it returns as soon as the harness
// accepted it; the reply streams on /events.
func (d *daemon) send(w http.ResponseWriter, r *http.Request, h *hosted) {
	var body struct {
		Text     string          `json:"text"`
		Delivery agent.Delivery  `json:"delivery"` // queue, steer, or interrupt
		Schema   json.RawMessage `json:"schema"`   // ask for JSON matching this schema
		Wait     bool            `json:"wait"`
	}
	if !decode(w, r, &body) {
		return
	}
	in := agent.Text(body.Text)
	in.Delivery, in.OutputSchema = body.Delivery, body.Schema
	turn, err := h.sess.Send(r.Context(), in)
	if err != nil {
		reply(w, http.StatusBadRequest, errorBody(err))
		return
	}
	if !body.Wait {
		reply(w, http.StatusAccepted, map[string]string{"turn": turn.ID()})
		return
	}
	// A client that hangs up stops waiting; the turn keeps running.
	res, err := turn.Wait(r.Context())
	if res == nil {
		reply(w, http.StatusGatewayTimeout, errorBody(err))
		return
	}
	res.Raw, res.Items = nil, nil
	reply(w, http.StatusOK, res)
}

func (d *daemon) interrupt(w http.ResponseWriter, r *http.Request, h *hosted) {
	if err := h.sess.Interrupt(r.Context()); err != nil {
		reply(w, http.StatusBadRequest, errorBody(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requests lists the requests waiting for an answer, so a client that
// connects late sees what the agent is blocked on.
func (d *daemon) requests(w http.ResponseWriter, _ *http.Request, h *hosted) {
	out := []json.RawMessage{}
	for _, req := range h.sess.PendingRequests() {
		out = append(out, encodeEvent(req))
	}
	reply(w, http.StatusOK, out)
}

// answer answers one request. The body fits the request's type:
//
//	permission:  {"decision":"allow|allow_session|allow_always|deny|cancel","message":"why"}
//	question:    {"answers":{"<question id>":["<option label or text>"]}}, or {} to cancel
//	elicitation: {"action":"accept|decline|cancel","content":{...}}
func (d *daemon) answer(w http.ResponseWriter, r *http.Request, h *hosted) {
	var body struct {
		Decision agent.PermissionDecision `json:"decision"`
		Message  string                   `json:"message"`
		Answers  map[string][]string      `json:"answers"`
		Action   agent.ElicitationAction  `json:"action"`
		Content  json.RawMessage          `json:"content"`
	}
	if !decode(w, r, &body) {
		return
	}
	var req agent.Request
	for _, p := range h.sess.PendingRequests() {
		if p.RequestID() == r.PathValue("rid") {
			req = p
		}
	}
	if req == nil {
		reply(w, http.StatusNotFound, map[string]string{"error": "no such pending request"})
		return
	}
	var err error
	switch req := req.(type) {
	case *agent.PermissionRequest:
		err = req.Respond(r.Context(), agent.PermissionResponse{Decision: body.Decision, Message: body.Message})
	case *agent.UserInputRequest:
		err = req.Respond(r.Context(), agent.UserInputResponse{Answers: body.Answers, Cancelled: body.Answers == nil})
	case *agent.ElicitationRequest:
		err = req.Respond(r.Context(), agent.ElicitationResponse{Action: body.Action, Content: body.Content})
	}
	if err != nil {
		reply(w, http.StatusBadRequest, errorBody(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// events streams the session's events as Server-Sent Events. Each event's
// id is its sequence number; a client reconnecting with Last-Event-ID gets
// the events it missed, as far as they are still kept.
func (d *daemon) events(w http.ResponseWriter, r *http.Request, h *hosted) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		reply(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	after, _ := strconv.Atoi(r.Header.Get("Last-Event-ID"))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		batch, changed, ended := h.since(after)
		for _, e := range batch {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.seq, e.kind, e.data)
			after = e.seq
		}
		flusher.Flush()
		if ended && len(batch) == 0 {
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}

func (d *daemon) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, h := range d.sessions {
		_ = h.sess.Close()
	}
}

// hosted is a running session and the recent events kept for clients.
type hosted struct {
	sess agent.Session
	keep int

	mu      sync.Mutex
	log     []recorded
	seq     int
	changed chan struct{} // closed and replaced when an event arrives
	ended   bool
}

type recorded struct {
	seq  int
	kind string
	data []byte
}

// record reads the session's events until it ends. It is the only reader,
// so the session never buffers events without limit.
func (h *hosted) record() {
	for ev := range h.sess.Events() {
		kind, data := agent.EventType(ev), encodeEvent(ev)
		h.mu.Lock()
		h.seq++
		h.log = append(h.log, recorded{seq: h.seq, kind: kind, data: data})
		if len(h.log) > h.keep {
			h.log = h.log[len(h.log)-h.keep:]
		}
		close(h.changed)
		h.changed = make(chan struct{})
		h.mu.Unlock()
	}
	h.mu.Lock()
	h.ended = true
	close(h.changed)
	h.mu.Unlock()
}

// since returns the kept events after seq, a channel closed when more
// arrive, and whether the session ended. The log holds consecutive
// sequence numbers, so the start is found by arithmetic. The returned
// slice is shared: entries are only appended or trimmed, never changed.
func (h *hosted) since(seq int) ([]recorded, <-chan struct{}, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	start := 0
	if len(h.log) > 0 {
		start = min(max(seq-h.log[0].seq+1, 0), len(h.log))
	}
	return h.log[start:], h.changed, h.ended
}

// encodeEvent marshals an event without its native payload, which can be
// large and differs per harness.
func encodeEvent(ev agent.Event) json.RawMessage {
	data, err := json.Marshal(ev)
	if err != nil {
		return errorJSON(err)
	}
	// Only the top level is decoded; field values stay as encoded.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return errorJSON(err)
	}
	if _, native := ev.(*agent.RawEvent); !native {
		delete(fields, "Raw")
	}
	fields["Type"], _ = json.Marshal(agent.EventType(ev))
	data, _ = json.Marshal(fields)
	return data
}

// brief drops the native startup payload from session info.
func brief(i agent.SessionInfo) agent.SessionInfo {
	i.Raw = nil
	return i
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		reply(w, http.StatusBadRequest, errorBody(err))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errorBody(err error) map[string]string { return map[string]string{"error": fmt.Sprint(err)} }

func errorJSON(err error) json.RawMessage {
	data, _ := json.Marshal(errorBody(err))
	return data
}
