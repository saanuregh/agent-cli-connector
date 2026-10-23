//go:build cgo && !agent_use_lib && !android && ((linux && (amd64 || arm64)) || (darwin && (amd64 || arm64)) || (windows && amd64))

package agent_test

import (
	"testing"

	agent "github.com/saanuregh/agent-cli-connector/bindings/go"
)

// TestStaticLink checks that the linked-in static library is this version.
func TestStaticLink(t *testing.T) {
	if got := agent.LoadedLibraryVersion(); got != agent.LibraryVersion {
		t.Fatalf("LoadedLibraryVersion() = %q, want %q", got, agent.LibraryVersion)
	}
}
