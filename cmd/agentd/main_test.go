package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestBinaryEndToEnd builds agentd and speaks the protocol over its
// standard input and output. No harness CLI is needed: Check reports a
// missing binary without starting anything.
func TestBinaryEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := filepath.Join(t.TempDir(), "agentd")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	missing := filepath.Join(t.TempDir(), "no-such-claude")
	send := func(line string) {
		t.Helper()
		if _, err := io.WriteString(stdin, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	replies := bufio.NewScanner(stdout)
	replies.Buffer(nil, 1<<24)
	read := func() map[string]json.RawMessage {
		t.Helper()
		if !replies.Scan() {
			t.Fatalf("agentd closed its output: %v\nstderr: %s", replies.Err(), stderr.String())
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(replies.Bytes(), &msg); err != nil {
			t.Fatalf("reply %s: %v", replies.Bytes(), err)
		}
		if string(msg["jsonrpc"]) != `"2.0"` {
			t.Errorf("reply without jsonrpc 2.0: %s", replies.Bytes())
		}
		return msg
	}

	init, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"ProtocolVersion": 1, "Providers": map[string]any{"claude": map[string]any{"Options": map[string]any{"Path": missing}}}}})
	send(string(init))
	var initRes struct {
		Providers []struct{ Name string }
	}
	if err := json.Unmarshal(read()["result"], &initRes); err != nil || len(initRes.Providers) != 1 || initRes.Providers[0].Name != "claude" {
		t.Fatalf("initialize result %+v, %v", initRes, err)
	}

	send(`{"jsonrpc":"2.0","id":2,"method":"provider/check","params":{"Provider":"claude"}}`)
	reply := read()
	var status struct {
		Installed bool
		Err       *struct{ Message string }
	}
	if err := json.Unmarshal(reply["result"], &status); err != nil || status.Installed {
		t.Fatalf("provider/check reply %s %s, %v", reply["result"], reply["error"], err)
	}

	send(`{"jsonrpc":"2.0","id":3,"method":"session/info","params":{"Session":"s1"}}`)
	var rpcErr struct {
		Code int
		Data struct{ Kind string }
	}
	if err := json.Unmarshal(read()["error"], &rpcErr); err != nil || rpcErr.Data.Kind != "not_found" {
		t.Errorf("unknown session error %+v, %v", rpcErr, err)
	}

	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("agentd exited with %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("agentd did not exit after its input closed")
	}
}
