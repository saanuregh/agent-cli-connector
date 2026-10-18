//go:build unix

package providerkit

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// A harness that stops reading stdin must not make Stop hang while a write
// is blocked.
func TestStopWithBlockedWrite(t *testing.T) {
	p, err := StartProcess(context.Background(), ProcessOptions{Provider: "test", Command: agent.Command{Path: "sleep", Args: []string{"30"}}})
	if err != nil {
		t.Skip("sleep not available:", err)
	}
	go func() { _ = p.ReadLines(func([]byte) {}) }()
	writeDone := make(chan error, 1)
	go func() { writeDone <- p.WriteLine(bytes.Repeat([]byte("x"), 1<<20)) }()
	time.Sleep(100 * time.Millisecond)
	stopped := make(chan struct{})
	go func() {
		p.Stop(200 * time.Millisecond)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung on a blocked write")
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Error("blocked write reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked write never returned")
	}
}

func TestExitDetectedWhileChildHoldsPipes(t *testing.T) {
	p, err := StartProcess(context.Background(), ProcessOptions{Provider: "test", Command: agent.Command{Path: "sh", Args: []string{"-c", "sleep 30 & exit 3"}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.ReadLines(func([]byte) {}) }()
	select {
	case <-p.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("exit not detected while a child held the pipes")
	}
	<-done
}

func TestExitDetectedWhenEscapedChildHoldsPipes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs setsid")
	}
	p, err := StartProcess(context.Background(), ProcessOptions{Provider: "test", Command: agent.Command{Path: "sh", Args: []string{"-c", "setsid sleep 30 & exit 3"}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.ReadLines(func([]byte) {}) }()
	select {
	case <-p.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("exit not detected while an escaped child held the pipes")
	}
	if err := <-done; err != nil {
		t.Errorf("ReadLines = %v", err)
	}
}
