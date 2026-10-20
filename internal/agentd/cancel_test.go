package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/agenttest"
)

// slowLister is agenttest with a ListModels that waits until cancelled.
type slowLister struct {
	*agenttest.Provider
	started chan struct{}
}

func (p *slowLister) ListModels(ctx context.Context) ([]agent.Model, error) {
	close(p.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func cancelledReply(t *testing.T, res map[string]json.RawMessage) {
	t.Helper()
	var e struct {
		Code int
		Data ErrorInfo
	}
	if err := json.Unmarshal(res["error"], &e); err != nil || e.Code != codeCancelled || e.Data.Kind != "cancelled" {
		t.Errorf("reply %s, want a cancelled error", res["error"])
	}
}

func TestClientCancelsRequest(t *testing.T) {
	p := &slowLister{Provider: agenttest.New(echo), started: make(chan struct{})}
	c := newClient(t, fakeSpec(p))
	c.mustCall("initialize", InitializeParams{}, nil)
	c.writeLine(`{"jsonrpc":"2.0","id":"lm","method":"provider/listModels","params":{"Provider":"agenttest"}}`)
	<-p.started
	c.writeLine(`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":"lm"}}`)
	cancelledReply(t, c.replyTo(`"lm"`))
}

func TestCancelWhileQueued(t *testing.T) {
	release := make(chan struct{})
	slow := ProviderSpec{Name: agenttest.Name, New: func(json.RawMessage, ProviderBridge) (agent.Provider, error) {
		<-release
		return agenttest.New(echo), nil
	}}
	c := newClient(t, slow)
	c.writeLine(`{"jsonrpc":"2.0","id":"i","method":"initialize","params":{}}`)
	c.writeLine(`{"jsonrpc":"2.0","id":"st","method":"session/start","params":{"Provider":"agenttest"}}`)
	// session/start waits for initialize, which waits for release.
	c.writeLine(`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":"st"}}`)
	cancelledReply(t, c.replyTo(`"st"`))
	close(release)
	if res := c.replyTo(`"i"`); res["error"] != nil {
		t.Errorf("initialize: %s", res["error"])
	}
}

func TestSecondInitializeWaitsForFirst(t *testing.T) {
	release := make(chan struct{})
	slow := ProviderSpec{Name: agenttest.Name, New: func(json.RawMessage, ProviderBridge) (agent.Provider, error) {
		<-release
		return agenttest.New(echo), nil
	}}
	c := newClient(t, slow)
	c.writeLine(`{"jsonrpc":"2.0","id":"i1","method":"initialize","params":{}}`)
	c.writeLine(`{"jsonrpc":"2.0","id":"i2","method":"initialize","params":{}}`)
	c.writeLine(`{"jsonrpc":"2.0","id":"st","method":"session/start","params":{"Provider":"agenttest"}}`)
	// While the first initialize builds, neither the second nor
	// session/start may finish.
	time.Sleep(100 * time.Millisecond)
	for len(c.lines) > 0 {
		var msg struct{ ID json.RawMessage }
		_ = json.Unmarshal(<-c.lines, &msg)
		if id := string(msg.ID); id == `"i2"` || id == `"st"` {
			t.Errorf("%s answered before the first initialize finished", id)
		}
	}
	close(release)
	if res := c.replyTo(`"st"`); res["error"] != nil {
		t.Errorf("session/start after two initializes: %s", res["error"])
	}
}

// blockingSession is an agenttest session whose Compact waits for
// release and whose SetTitle records the title.
type blockingSession struct {
	agent.Session
	release chan struct{}
	mu      sync.Mutex
	titles  []string
}

func (s *blockingSession) Compact(context.Context, string) error {
	<-s.release
	return nil
}

func (s *blockingSession) SetTitle(_ context.Context, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.titles = append(s.titles, title)
	return nil
}

type blockingProvider struct {
	*agenttest.Provider
	sess *blockingSession
}

func (p *blockingProvider) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	inner, err := p.Provider.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	p.sess.Session = inner
	return p.sess, nil
}

func TestCancelledQueuedRequestKeepsOrder(t *testing.T) {
	sess := &blockingSession{release: make(chan struct{})}
	c := newClient(t, fakeSpec(&blockingProvider{Provider: agenttest.New(echo), sess: sess}))
	s := startFake(c)
	c.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":"a","method":"session/compact","params":{"Session":%q}}`, s.Session))
	c.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":"b","method":"session/setTitle","params":{"Session":%q,"Value":"b"}}`, s.Session))
	c.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":"c","method":"session/setTitle","params":{"Session":%q,"Value":"c"}}`, s.Session))
	c.writeLine(`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":"b"}}`)
	cancelledReply(t, c.replyTo(`"b"`))
	time.Sleep(100 * time.Millisecond) // a request that jumped the queue would run now
	sess.mu.Lock()
	early := slices.Clone(sess.titles)
	sess.mu.Unlock()
	if len(early) != 0 {
		t.Errorf("titles %v set while compact still ran", early)
	}
	close(sess.release)
	c.replyTo(`"c"`)
	if !slices.Equal(sess.titles, []string{"c"}) {
		t.Errorf("titles %v, want [c]", sess.titles)
	}
}
