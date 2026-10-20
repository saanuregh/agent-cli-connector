package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// pair connects two peers in memory.
func pair(a, b Handler) (*Conn, *Conn) {
	var left, right *Conn
	var mu sync.Mutex
	left = New(func(data []byte) error {
		go func() { mu.Lock(); defer mu.Unlock(); _ = right.Receive(data) }()
		return nil
	}, a)
	right = New(func(data []byte) error {
		go func() { _ = left.Receive(data) }()
		return nil
	}, b)
	return left, right
}

func TestCallRequestAndNotify(t *testing.T) {
	notified := make(chan string, 1)
	client, _ := pair(Handler{}, Handler{
		Request: func(ctx context.Context, msg *Message, line []byte) (any, error) {
			if msg.Method == "fail" {
				return nil, &Error{Code: 42, Message: "nope"}
			}
			var p map[string]int
			_ = json.Unmarshal(msg.Params, &p)
			return map[string]int{"sum": p["a"] + p["b"]}, nil
		},
		Notify: func(msg *Message, line []byte) { notified <- msg.Method },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out struct{ Sum int }
	if err := client.Call(ctx, "add", map[string]int{"a": 2, "b": 3}, &out); err != nil || out.Sum != 5 {
		t.Fatalf("call = %+v, %v", out, err)
	}
	var rpcErr *Error
	if err := client.Call(ctx, "fail", nil, nil); !errors.As(err, &rpcErr) || rpcErr.Code != 42 {
		t.Errorf("fail = %v", err)
	}
	if err := client.Notify("hello", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-notified:
		if m != "hello" {
			t.Errorf("notified %q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
}

func TestCloseFailsPendingCalls(t *testing.T) { synctest.Test(t, testCloseFailsPendingCalls) }

func testCloseFailsPendingCalls(t *testing.T) {
	c := New(func([]byte) error { return nil }, Handler{})
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "never", nil, nil) }()
	synctest.Wait() // the call is pending
	boom := errors.New("gone")
	c.Close(boom)
	select {
	case err := <-errc:
		if !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call not released")
	}
	if err := c.Call(context.Background(), "after", nil, nil); !errors.Is(err, boom) {
		t.Errorf("after close = %v", err)
	}
}

func TestStringAndNumberIDsMatch(t *testing.T) {
	if idKey(json.RawMessage(`7`)) != "7" || idKey(json.RawMessage(`"7"`)) != "7" {
		t.Error("id normalization")
	}
}

func TestCallOrderedRunsReplyBeforeLaterMessages(t *testing.T) {
	sent := make(chan []byte, 1)
	var replied bool
	seenReplied := make(chan bool, 1)
	c := New(func(data []byte) error { sent <- data; return nil }, Handler{
		Notify: func(*Message, []byte) { seenReplied <- replied },
	})
	done := make(chan error, 1)
	go func() {
		_, err := c.CallOrdered(context.Background(), "stop", nil, func(m *Message) { replied = m.Error != nil })
		done <- err
	}()
	var req struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(<-sent, &req)
	reply, _ := json.Marshal(map[string]any{"id": req.ID, "error": map[string]any{"code": 1, "message": "no"}})
	_ = c.Receive(reply)
	_ = c.Receive([]byte(`{"method":"after"}`))
	if !<-seenReplied {
		t.Error("the notification after the reply ran before the reply hook")
	}
	if err := <-done; err == nil {
		t.Error("CallOrdered returned no error for an error reply")
	}
}

func TestCallCancelableNotifiesPeer(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	c := New(func(data []byte) error {
		mu.Lock()
		sent = append(sent, string(data))
		mu.Unlock()
		return nil
	}, Handler{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CallCancelable(ctx, "slow", nil, "$/cancelRequest"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[1] != `{"method":"$/cancelRequest","params":{"id":1}}` {
		t.Errorf("sent %q", sent)
	}
}

func TestNewStrictSendsVersion(t *testing.T) {
	var sent []string
	c := NewStrict(func(data []byte) error { sent = append(sent, string(data)); return nil }, Handler{})
	_ = c.Notify("n", nil)
	_ = c.Reply(json.RawMessage(`1`), nil, nil)
	if len(sent) != 2 || sent[0] != `{"jsonrpc":"2.0","method":"n"}` || sent[1] != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Errorf("sent %q", sent)
	}
}

func TestScheduleDecidesWhenRequestsRun(t *testing.T) {
	replies := make(chan string, 2)
	var queued []func()
	c := New(func(data []byte) error { replies <- string(data); return nil }, Handler{
		Request: func(_ context.Context, msg *Message, _ []byte) (any, error) { return msg.Method, nil },
		Schedule: func(ctx context.Context, _ *Message, serve func(context.Context)) {
			queued = append(queued, func() { serve(ctx) })
		},
	})
	_ = c.Receive([]byte(`{"id":1,"method":"first"}`))
	_ = c.Receive([]byte(`{"id":2,"method":"second"}`))
	if len(queued) != 2 || len(replies) != 0 {
		t.Fatalf("queued %d, replied %d before serving", len(queued), len(replies))
	}
	queued[1]()
	queued[0]()
	if a, b := <-replies, <-replies; a != `{"id":2,"result":"second"}` || b != `{"id":1,"result":"first"}` {
		t.Errorf("replies %s, %s", a, b)
	}
}

func TestCancelMethodCancelsServedRequest(t *testing.T) {
	started := make(chan struct{})
	replies := make(chan string, 2)
	c := New(func(data []byte) error { replies <- string(data); return nil }, Handler{
		CancelMethod: "$/cancelRequest",
		Request: func(ctx context.Context, msg *Message, _ []byte) (any, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		Notify: func(msg *Message, _ []byte) { t.Errorf("cancel reached Notify: %s", msg.Method) },
	})
	_ = c.Receive([]byte(`{"id":"r1","method":"slow"}`))
	<-started
	// Spacing around the ID does not matter.
	_ = c.Receive([]byte(`{"method":"$/cancelRequest","params":{"id": "r1"}}`))
	if got := <-replies; got != `{"error":{"code":-32603,"message":"context canceled"},"id":"r1"}` {
		t.Errorf("reply %s", got)
	}
}
