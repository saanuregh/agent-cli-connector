package leakcheck_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/saanuregh/agent-cli-connector/internal/leakcheck"
)

const childEnv = "LEAKCHECK_CHILD"

func TestMain(m *testing.M) { leakcheck.Main(m) }

func runChild(t *testing.T, test string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"=1", "LEAKCHECK_TIMEOUT=200ms")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("run child: %v", err)
	}
	return cmd.ProcessState.ExitCode(), stderr.String()
}

func TestReportsLeak(t *testing.T) {
	code, stderr := runChild(t, "TestLeakChild")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "leakcheck_test.blockForever") {
		t.Fatalf("stderr lacks the leaked goroutine's stack:\n%s", stderr)
	}
}

func TestCleanRunPasses(t *testing.T) {
	code, stderr := runChild(t, "TestCleanChild")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
}

func blockForever(ch chan struct{}) { <-ch }

func TestLeakChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("runs only as a child of TestReportsLeak")
	}
	go blockForever(make(chan struct{}))
}

func TestCleanChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("runs only as a child of TestCleanRunPasses")
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { blockForever(stop); close(done) }()
	close(stop)
	<-done
}
