package opencode_test

import (
	"context"
	"net"
	"net/url"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/opencode"
)

// A server running elsewhere prints a URL with its own host, and reaches
// the host tools at a host only it knows; URLHost and HostToolsHost
// rewrite both.
func TestURLHostAndHostToolsHost(t *testing.T) {
	f := newFakeServer(t)
	var hostToolsURL string
	f.on("PUT /api/experimental/mcp/*", func(r *fakeRequest) (int, any) {
		hostToolsURL = fakeproc.Str(r.Body, "config", "url")
		return 204, nil
	})
	served, _ := url.Parse(f.srv.URL)
	_, port, _ := net.SplitHostPort(served.Host)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		// The address as the far side sees it: unreachable from here.
		h.Send(map[string]any{"url": "http://server.invalid:" + port})
		h.WaitClosed()
	})
	tool := agent.Tool{Name: "t", Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) {
		return agent.TextResult("x"), nil
	}}
	p := opencode.New(opencode.Options{Spawner: spawner, URLHost: "127.0.0.1", HostToolsHost: "host.docker.internal"})
	sess, err := p.Start(context.Background(), agent.Config{Cwd: workDir, Tools: []agent.Tool{tool}})
	if err != nil {
		t.Fatalf("start through URLHost: %v", err)
	}
	t.Cleanup(func() {
		_ = sess.Close()
		for range sess.Events() {
		}
	})
	u, err := url.Parse(hostToolsURL)
	if err != nil || u.Hostname() != "host.docker.internal" || u.Port() == "" || u.Path != "/mcp" {
		t.Errorf("host tools registered at %q", hostToolsURL)
	}
}
