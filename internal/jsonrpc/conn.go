// Package jsonrpc is a JSON-RPC 2.0 peer over newline-delimited messages.
// It tolerates peers that omit the "jsonrpc" member, as the Codex app
// server does.
package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
)

// Error is a JSON-RPC error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// Message is any JSON-RPC message.
type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// IsRequest reports a message with a method and an ID.
func (m *Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification reports a message with a method and no ID.
func (m *Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// Writer sends one encoded message.
type Writer func(data []byte) error

// Handler receives messages from the peer. Notify runs on the read
// goroutine in arrival order and must not block. Request runs on its own
// goroutine; its return value is sent back.
type Handler struct {
	Notify  func(msg *Message, line []byte)
	Request func(ctx context.Context, msg *Message, line []byte) (any, error)
}

// Conn is one side of a JSON-RPC connection.
type Conn struct {
	write   Writer
	handler Handler
	nextID  atomic.Int64

	mu       sync.Mutex
	pending  map[string]pendingCall
	closed   bool
	closeErr error
}

// New returns a connection that writes with write and dispatches incoming
// messages to h. Feed incoming lines to Receive.
func New(write Writer, h Handler) *Conn {
	return &Conn{write: write, handler: h, pending: make(map[string]pendingCall)}
}

// Call sends a request and decodes the result into result, which may be
// nil.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	raw, err := c.CallRaw(ctx, method, params)
	if err != nil {
		return err
	}
	if result == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

// pendingCall is a call waiting for its reply.
type pendingCall struct {
	ch    chan *Message
	reply func(*Message)
}

// CallRaw sends a request and returns the raw result.
func (c *Conn) CallRaw(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.CallOrdered(ctx, method, params, nil)
}

// CallOrdered is CallRaw with reply, when not nil, run on the goroutine
// that reads the reply, before any message read after it is handled. Use
// it to update state that later messages depend on. reply must not block.
func (c *Conn) CallOrdered(ctx context.Context, method string, params any, reply func(*Message)) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	ch := make(chan *Message, 1)
	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		return nil, err
	}
	c.pending[key] = pendingCall{ch: ch, reply: reply}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	data, err := json.Marshal(outgoing{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", method, err)
	}
	if err := c.write(data); err != nil {
		return nil, err
	}
	select {
	case msg := <-ch:
		if msg == nil {
			c.mu.Lock()
			err := c.closeErr
			c.mu.Unlock()
			return nil, err
		}
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// outgoing is a request, notification, or response. Its fields are in
// sorted name order, the order encoding/json gave the map it replaced, and a
// nil interface field is omitted.
type outgoing struct {
	Error  *Error `json:"error,omitempty"`
	ID     any    `json:"id,omitempty"`
	Method string `json:"method,omitempty"`
	Params any    `json:"params,omitempty"`
	Result any    `json:"result,omitempty"`
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	data, err := json.Marshal(outgoing{Method: method, Params: params})
	if err != nil {
		return err
	}
	return c.write(data)
}

// Receive handles one incoming line.
func (c *Conn) Receive(line []byte) error {
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return err
	}
	switch {
	case msg.Method == "" && len(msg.ID) > 0:
		key := idKey(msg.ID)
		c.mu.Lock()
		call, ok := c.pending[key]
		c.mu.Unlock()
		if ok {
			if call.reply != nil {
				call.reply(&msg)
			}
			call.ch <- &msg
		}
	case msg.IsRequest():
		go c.serve(&msg, line)
	case msg.IsNotification():
		if c.handler.Notify != nil {
			c.handler.Notify(&msg, line)
		}
	}
	return nil
}

func (c *Conn) serve(msg *Message, line []byte) {
	var result any
	var err error
	if c.handler.Request != nil {
		result, err = c.handler.Request(context.Background(), msg, line)
	} else {
		err = &Error{Code: -32601, Message: "method not found: " + msg.Method}
	}
	_ = c.Reply(msg.ID, result, err)
}

// Reply answers a request by ID. Handlers that answer later return
// ErrDeferred and call Reply themselves.
func (c *Conn) Reply(id json.RawMessage, result any, err error) error {
	if err == ErrDeferred {
		return nil
	}
	resp := outgoing{ID: id}
	if err != nil {
		rpcErr, ok := err.(*Error)
		if !ok {
			rpcErr = &Error{Code: -32603, Message: err.Error()}
		}
		resp.Error = rpcErr
	} else {
		if result == nil {
			result = map[string]any{}
		}
		resp.Result = result
	}
	data, mErr := json.Marshal(resp)
	if mErr != nil {
		return mErr
	}
	return c.write(data)
}

// ErrDeferred tells the connection that a request handler will reply
// later with Reply.
var ErrDeferred = &Error{Code: 0, Message: "deferred"}

// Close fails pending calls with err.
func (c *Conn) Close(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.closeErr = err
	for key, call := range c.pending {
		select {
		case call.ch <- nil:
		default:
		}
		delete(c.pending, key)
	}
}

// idKey normalizes numeric and string IDs to one key form.
func idKey(id json.RawMessage) string {
	var n int64
	if json.Unmarshal(id, &n) == nil {
		return strconv.FormatInt(n, 10)
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s
	}
	return string(id)
}
