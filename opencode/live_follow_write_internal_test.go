//go:build live

package opencode

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Live regression test for the eleventh review, against OpenCode's
// evaluator; it ports the reviewer's reproduction.

// patchHook is an HTTP transport that runs before once, before forwarding
// the first rule write that match accepts; it widens the window of a write
// in flight without a hook in the library.
type patchHook struct {
	match  func(path string, body []byte) bool
	before func()
	once   sync.Once
}

func (h *patchHook) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPatch && r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if h.match(r.URL.Path, body) {
			h.once.Do(h.before)
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// An agent selection that lands while a following session's write for a
// new default agent is in flight, naming the old default, gets rules for
// the selected agent: build's config deny of secrets/ holds under bypass.
func TestLiveSelectionDuringFollowWrite(t *testing.T) {
	ctx := context.Background()
	p := rulesProvider(t, `{}`)
	var s *Session
	hook := &patchHook{
		match: func(path string, body []byte) bool {
			return s != nil && strings.HasSuffix(path, "/api/session/"+s.ID()) && strings.Contains(string(body), `{"action":"shell","resource":"*","effect":"deny"}`)
		},
		before: func() {
			// Another client selects build while this write is in flight.
			if err := s.c.Call(ctx, "POST", "/api/session/"+s.ID()+"/agent", map[string]any{"agent": "build"}, nil); err != nil {
				t.Errorf("select: %v", err)
			}
			time.Sleep(500 * time.Millisecond)
		},
	}
	p.opts.HTTPClient = &http.Client{Transport: hook}
	sess, err := p.StartSession(ctx, agent.Config{Cwd: t.TempDir(), PermissionMode: agent.PermissionBypass})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	go func() {
		for range sess.Events() {
		}
	}()
	s = sess
	e := newLiveEvaluator(t, s)
	waitAgent(t, s, "build")
	writeConfig(t, p, `{"default_agent":"locked","agents":{"build":{"permissions":[{"action":"edit","resource":"secrets/*","effect":"deny"}]},"locked":{"mode":"primary","permissions":[{"action":"shell","resource":"*","effect":"deny"}]}}}`)
	waitDefault(t, s, "locked")
	if err := s.followDefaultAgent(ctx); err != nil {
		t.Fatal(err)
	}
	if _, native := nativeRules(t, s, s.ID()); native != "build" {
		t.Fatalf("session agent on the server = %q", native)
	}
	eventually(t, s, e, s.ID(), "edit", "secrets/k", "deny")
	eventually(t, s, e, s.ID(), "shell", "ls", "allow")
}
