//go:build live

package opencode

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Live regression test for the twelfth review, against OpenCode's
// evaluator; it ports the reviewer's reproduction.

// getHook is an HTTP transport that runs after once, after the first read
// of session sid while armed, before that read's answer is used.
type getHook struct {
	armed atomic.Bool
	sid   atomic.Value
	once  sync.Once
	after func()
}

func (h *getHook) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	sid, _ := h.sid.Load().(string)
	if err == nil && h.armed.Load() && r.Method == http.MethodGet && sid != "" && r.URL.Path == "/api/session/"+sid {
		h.once.Do(func() {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(body))
			h.after()
		})
	}
	return resp, err
}

// Another client selects the locked agent while the reconnection's read of
// the session is in flight: the selection stands, and its shell deny holds
// under bypass.
func TestLiveResyncReadOvertakenBySelection(t *testing.T) {
	ctx := context.Background()
	p := rulesProvider(t, lockedConfig)
	var s *Session
	hook := &getHook{}
	hook.after = func() {
		if err := s.c.Call(context.Background(), "POST", "/api/session/"+s.ID()+"/agent", map[string]any{"agent": "locked"}, nil); err != nil {
			t.Errorf("select: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
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
	waitAgent(t, s, "locked")
	eventually(t, s, e, s.ID(), "shell", "ls", "allow")
	hook.sid.Store(s.ID())
	hook.armed.Store(true)
	s.streamMu.Lock()
	stream := s.stream
	s.streamMu.Unlock()
	stream.close()
	time.Sleep(4 * time.Second)
	hook.armed.Store(false)
	eventually(t, s, e, s.ID(), "shell", "ls", "deny")
	if got := s.currentAgent(); got != "locked" {
		t.Fatalf("session agent = %q", got)
	}
}
