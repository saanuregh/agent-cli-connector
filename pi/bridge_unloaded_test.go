package pi_test

import (
	"context"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func TestSwitchWithoutBridgeRefusesGatedTurns(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		respond(h, h.RecvWhere(isCommand("new_session")), map[string]any{"cancelled": false})
		respond(h, h.RecvWhere(isCommand("get_state")), map[string]any{"sessionId": "pi-2"})
		respond(h, h.RecvWhere(isCommand("get_commands")), map[string]any{"commands": []any{}})
		prompt := h.RecvWhere(isCommand("prompt"))
		if fakeproc.Str(prompt, "message") != "after bypass" {
			t.Errorf("prompt = %v", prompt)
		}
		respond(h, prompt, map[string]any{"disposition": "handled"})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(ctx, agent.Config{PermissionMode: agent.PermissionDefault})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		sess.Close()
		for range sess.Events() {
		}
	}()
	if _, err := sess.NewSession(ctx, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for found := false; !found; {
		select {
		case ev := <-sess.Events():
			if n, ok := ev.(*agent.Notice); ok && n.Code == "bridge_unloaded" {
				if n.Level != agent.NoticeError {
					t.Errorf("notice = %+v", n)
				}
				found = true
			}
		case <-deadline:
			t.Fatal("no bridge_unloaded notice")
		}
	}
	if _, err := sess.Send(ctx, agent.Text("gated")); err == nil || !strings.Contains(err.Error(), "cannot be enforced") {
		t.Fatalf("Send without the bridge: err = %v", err)
	}
	if err := sess.SetPermissionMode(ctx, agent.PermissionDefault); err == nil {
		t.Error("SetPermissionMode(default) without the bridge succeeded")
	}
	if err := sess.SetPermissionMode(ctx, agent.PermissionBypass); err != nil {
		t.Fatal(err)
	}
	turn, err := sess.Send(ctx, agent.Text("after bypass"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
