// Package leakcheck fails a test binary that leaves goroutines running after its tests pass.
package leakcheck

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

const pollInterval = 50 * time.Millisecond

// ignoredFrames mark goroutines that a passing test binary may leave behind.
var ignoredFrames = []string{
	// signal.Notify starts a receiver goroutine that lives until the process exits, even after
	// signal.Stop.
	"os/signal.signal_recv",
	"os/signal.loop",
	"runtime.ensureSigM",
}

// Main runs the tests and, if they pass, exits 1 when goroutines started during the run are
// still alive after a grace period.
func Main(m *testing.M) {
	baseline := map[string]bool{}
	for _, g := range goroutines() {
		baseline[g.id] = true
	}
	code := m.Run()
	if code != 0 {
		os.Exit(code)
	}
	// Idle keep-alive connections of the default transport outlive their tests; their read and
	// write loops end shortly after this, within the polling window.
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	timeout := 5 * time.Second
	if value := os.Getenv("LEAKCHECK_TIMEOUT"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leakcheck: invalid LEAKCHECK_TIMEOUT %q: %v\n", value, err)
			os.Exit(1)
		}
		timeout = parsed
	}
	deadline := time.Now().Add(timeout)
	for {
		leaked := leakedGoroutines(baseline)
		if len(leaked) == 0 {
			os.Exit(0)
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "leakcheck: %d goroutine(s) still running %v after tests passed:\n\n", len(leaked), timeout)
			for _, g := range leaked {
				fmt.Fprintf(os.Stderr, "%s\n\n", g.stack)
			}
			os.Exit(1)
		}
		time.Sleep(pollInterval)
	}
}

type goroutine struct {
	id    string
	stack string
}

func leakedGoroutines(baseline map[string]bool) []goroutine {
	var leaked []goroutine
	// The first goroutine in runtime.Stack output is the caller, which is running Main.
	for _, g := range goroutines()[1:] {
		if !baseline[g.id] && !ignored(g.stack) {
			leaked = append(leaked, g)
		}
	}
	return leaked
}

func ignored(stack string) bool {
	for _, frame := range ignoredFrames {
		if strings.Contains(stack, frame) {
			return true
		}
	}
	return false
}

func goroutines() []goroutine {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var result []goroutine
	for _, stack := range strings.Split(string(buf), "\n\n") {
		header, _, _ := strings.Cut(stack, "\n")
		fields := strings.Fields(header)
		if len(fields) < 2 || fields[0] != "goroutine" {
			continue
		}
		result = append(result, goroutine{id: fields[1], stack: stack})
	}
	return result
}
