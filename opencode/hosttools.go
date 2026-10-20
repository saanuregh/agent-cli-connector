package opencode

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/mcpserver"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

// hostToolPrefix starts the name of every host tool MCP server, so the
// permission rules of one session can deny the host tools of others that
// share its directory.
const hostToolPrefix = "agenthost-"

// hostTools serves a session's agent.Tool values as a streamable HTTP MCP
// server on a local listener. OpenCode reaches it as a remote MCP server.
type hostTools struct {
	name   string
	token  string
	url    string
	mcp    *mcpserver.Server
	srv    *http.Server
	ctx    context.Context
	cancel context.CancelFunc
	// sessionID names the session in tool calls.
	sessionID func() string
}

// hostToolsURL returns the HostToolsURL to use: the option, or one that
// applies HostToolsHost, or nil for the listening address itself.
func (p *Provider) hostToolsURL() func(addr string) string {
	if p.opts.HostToolsURL != nil || p.opts.HostToolsHost == "" {
		return p.opts.HostToolsURL
	}
	host := p.opts.HostToolsHost
	return func(addr string) string {
		u, _ := replaceHost("http://"+addr+"/mcp", host)
		return u
	}
}

// replaceHost replaces the host of rawURL, keeping its port.
func replaceHost(rawURL, host string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else {
		u.Host = host
	}
	return u.String(), nil
}

func startHostTools(addr string, publicURL func(addr string) string, tools []agent.Tool, sessionID func() string) (*hostTools, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	id := strings.ReplaceAll(providerutil.NewUUID(), "-", "")[:12]
	ctx, cancel := context.WithCancel(context.Background())
	h := &hostTools{
		name:      hostToolPrefix + id,
		token:     newPassword(),
		mcp:       mcpserver.New("agent-cli-connector", "1", tools),
		ctx:       ctx,
		cancel:    cancel,
		sessionID: sessionID,
	}
	h.url = "http://" + ln.Addr().String() + "/mcp"
	if publicURL != nil {
		h.url = publicURL(ln.Addr().String())
	}
	h.srv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = h.srv.Serve(ln) }()
	return h, nil
}

func (h *hostTools) close() {
	h.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
}

// config returns the OpenCode MCP configuration of the server. Code mode is
// off so the model calls host tools directly.
func (h *hostTools) config() map[string]any {
	return map[string]any{
		"type":     "remote",
		"url":      h.url,
		"headers":  map[string]string{"Authorization": "Bearer " + h.token},
		"oauth":    false,
		"codemode": false,
	}
}

// ServeHTTP answers MCP messages with JSON. Streamable HTTP lets a server
// answer a POST with a plain JSON body, and these tools never push.
func (h *hostTools) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	want := "Bearer " + h.token
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	body = bytes.TrimSpace(body)
	var out []byte
	if len(body) > 0 && body[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		var answers []json.RawMessage
		for _, msg := range batch {
			if a := h.mcp.Handle(ctx, msg, h.sessionID()); a != nil {
				answers = append(answers, a)
			}
		}
		if len(answers) > 0 {
			out, _ = json.Marshal(answers)
		}
	} else {
		out = h.mcp.Handle(ctx, body, h.sessionID())
	}
	if out == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}
