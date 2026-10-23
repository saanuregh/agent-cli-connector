//go:build cgo && !agent_use_lib && ((linux && (amd64 || arm64)) || (darwin && (amd64 || arm64)) || (windows && amd64))

package agent_test

import (
	"testing"

	agent "github.com/saanuregh/agent-cli-connector/bindings/go"
)

// TestStaticLink checks that the linked-in static library is used, with no
// shared library path, an empty cache, and downloads off.
func TestStaticLink(t *testing.T) {
	t.Setenv("AGENT_CLI_CONNECTOR_LIB", "")
	t.Setenv("AGENT_CLI_CONNECTOR_NO_DOWNLOAD", "1")
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	t.Setenv("LocalAppData", cache)
	if got := agent.LoadedLibraryVersion(); got != agent.LibraryVersion {
		t.Fatalf("LoadedLibraryVersion() = %q, want %q", got, agent.LibraryVersion)
	}
}
