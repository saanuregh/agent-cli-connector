package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Client calls the HTTP API of one OpenCode 2 server. It is safe for
// concurrent use.
type Client struct {
	base     *url.URL
	username string
	password string
	http     *http.Client
	tap      func(agent.WireDirection, []byte)
}

func newClient(rawURL, username, password string, hc *http.Client, tap func(agent.WireDirection, []byte)) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		// The URL is not echoed: it can carry a password, even in a form
		// url.Parse cannot read.
		return nil, errors.New("opencode: invalid server URL; it must look like http://host:port")
	}
	if hc == nil {
		hc = &http.Client{}
	}
	if username == "" {
		username = "opencode"
	}
	return &Client{base: u, username: username, password: password, http: hc, tap: tap}, nil
}

// URL returns the server's base URL.
func (c *Client) URL() string { return c.base.String() }

// redactedURL returns the base URL for errors, with any password masked
// and without a query, which can carry tokens.
func (c *Client) redactedURL() string {
	u := *c.base
	u.RawQuery, u.Fragment = "", ""
	return u.Redacted()
}

// withTap returns a copy of c that taps traffic to tap.
func (c *Client) withTap(tap func(agent.WireDirection, []byte)) *Client {
	cp := *c
	cp.tap = tap
	return &cp
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("opencode: path %q must start with /", path)
	}
	// Appended rather than resolved, so a server URL with a path prefix,
	// such as one behind a reverse proxy, keeps it.
	base := *c.base
	base.RawQuery, base.Fragment = "", ""
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base.String(), "/")+path, body)
	if err != nil {
		return nil, err
	}
	if c.password != "" {
		// OpenCode decodes Basic credentials as UTF-8.
		req.SetBasicAuth(c.username, c.password)
	}
	return req, nil
}

// Call sends an API request and decodes the JSON answer into out, which
// may be nil to discard it. body is encoded as JSON unless it is nil, a
// json.RawMessage, or a []byte, which are sent as they are. path is
// relative to the server, such as "/api/session?limit=10", and may carry a
// query string. Most routes wrap their answer in {"data": ...}; Call does
// not unwrap it. A failed request returns an *agent.ProtocolError whose
// Code is the HTTP status and whose Data is the answer body; a missing
// session also matches agent.ErrSessionNotFound.
func (c *Client) Call(ctx context.Context, method, path string, body, out any) error {
	data, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("opencode: %s %s: decode answer: %w", method, routeOf(path), err)
	}
	return nil
}

// data sends a request and decodes the "data" field of the answer.
func (c *Client) data(ctx context.Context, method, path string, body, out any) error {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := c.Call(ctx, method, path, body, &env); err != nil {
		return err
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("opencode: %s %s: decode answer: %w", method, routeOf(path), err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var payload []byte
	switch b := body.(type) {
	case nil:
	case json.RawMessage:
		payload = b
	case []byte:
		payload = b
	default:
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("opencode: encode %s %s: %w", method, routeOf(path), err)
		}
		payload = data
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := c.request(ctx, method, path, reader)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.tap != nil {
		c.tap(agent.WireSend, wireLine(method, path, payload))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode: %s %s: %w", method, routeOf(path), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("opencode: %s %s: read answer: %w", method, routeOf(path), err)
	}
	if c.tap != nil && len(data) > 0 {
		c.tap(agent.WireReceive, data)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiError(method, path, resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		// OpenCode 1.x and the web UI answer unknown routes with HTML.
		return nil, &agent.ProtocolError{Provider: Name, Method: method + " " + routeOf(path), Code: resp.StatusCode, Message: "the server answered with HTML; it is not an OpenCode 2 server or the route does not exist"}
	}
	return data, nil
}

func wireLine(method, path string, body []byte) []byte {
	line, _ := json.Marshal(struct {
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body,omitempty"`
	}{method, path, body})
	return line
}

// routeOf returns path without its query, for error messages that must not
// echo query values.
func routeOf(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

// apiError converts a failed answer. OpenCode errors look like
// {"_tag":"SessionNotFoundError","message":"..."}.
func apiError(method, path string, status int, body []byte) error {
	var e struct {
		Tag     string `json:"_tag"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Message
	switch {
	case e.Tag != "" && msg != "":
		msg = e.Tag + ": " + msg
	case e.Tag != "":
		msg = e.Tag
	case msg == "":
		msg = http.StatusText(status)
	}
	pe := &agent.ProtocolError{Provider: Name, Method: method + " " + routeOf(path), Code: status, Message: msg, ErrorCode: e.Tag}
	if json.Valid(body) {
		pe.Data = body
	}
	if e.Tag == "SessionNotFoundError" {
		return fmt.Errorf("%w: %w", agent.ErrSessionNotFound, pe)
	}
	return pe
}

// errorTag returns the OpenCode error class of a failed call, such as
// "SessionNotFoundError" or "ConflictError", or "" when err is not an
// OpenCode error answer.
func errorTag(err error) string {
	var pe *agent.ProtocolError
	if !errors.As(err, &pe) || pe.Provider != Name {
		return ""
	}
	return pe.ErrorCode
}

// httpStatus returns the HTTP status of a failed call, or 0.
func httpStatus(err error) int {
	var pe *agent.ProtocolError
	if errors.As(err, &pe) && pe.Provider == Name {
		return pe.Code
	}
	return 0
}

// locationQuery returns the query that scopes a route to a directory.
func locationQuery(dir string) string {
	if dir == "" {
		return ""
	}
	return "location%5Bdirectory%5D=" + url.QueryEscape(dir)
}

func withQuery(path string, params ...string) string {
	var parts []string
	for _, p := range params {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return path
	}
	return path + "?" + strings.Join(parts, "&")
}

func param(key, value string) string {
	if value == "" {
		return ""
	}
	return url.QueryEscape(key) + "=" + url.QueryEscape(value)
}

// silentStreamTimeout is how long the event stream may stay silent. The
// server writes a heartbeat comment every 10 to 15 seconds.
const silentStreamTimeout = 45 * time.Second

// eventStream is an open subscription to /api/event.
type eventStream struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

// Err returns why the stream ended, after done is closed.
func (s *eventStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *eventStream) close() {
	s.cancel()
	<-s.done
}

// subscribe opens the global event stream and calls handle with the JSON
// data of each event on a goroutine. It returns once the server accepted
// the subscription.
func (c *Client) subscribe(ctx context.Context, handle func([]byte)) (*eventStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := c.request(ctx, http.MethodGet, "/api/event", nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("opencode: subscribe to events: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		return nil, apiError(http.MethodGet, "/api/event", resp.StatusCode, data)
	}
	s := &eventStream{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer resp.Body.Close()
		err := c.readEvents(ctx, resp.Body, handle)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
	}()
	return s, nil
}

var errSilentStream = errors.New("opencode: the server stopped sending events")

func (c *Client) readEvents(ctx context.Context, body io.ReadCloser, handle func([]byte)) error {
	silent := make(chan struct{})
	var silentOnce sync.Once
	watchdog := time.AfterFunc(silentStreamTimeout, func() {
		silentOnce.Do(func() { close(silent) })
		_ = body.Close()
	})
	defer watchdog.Stop()
	r := bufio.NewReaderSize(body, 256<<10)
	var data bytes.Buffer
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			watchdog.Reset(silentStreamTimeout)
		}
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0 && err == nil:
			if data.Len() > 0 {
				ev := bytes.Clone(data.Bytes())
				data.Reset()
				if c.tap != nil {
					c.tap(agent.WireReceive, ev)
				}
				handle(ev)
			}
		case bytes.HasPrefix(line, []byte("data:")):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
		}
		if err != nil {
			select {
			case <-silent:
				return errSilentStream
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
}
