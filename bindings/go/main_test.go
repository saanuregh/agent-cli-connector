package agent_test

import (
	"os"
	"runtime"
	"testing"
)

// TestMain points the shared-library backend (cgo off, or -tags
// agent_use_lib) at the library built in this repository, unless
// AGENT_CLI_CONNECTOR_LIB is already set. With cgo the static backend ignores
// it and uses the committed bindings/go/lib archive.
func TestMain(m *testing.M) {
	if os.Getenv("AGENT_CLI_CONNECTOR_LIB") == "" {
		name := "libagent-cli-connector.so"
		if runtime.GOOS == "darwin" {
			name = "libagent-cli-connector.dylib"
		}
		path := "../../zig-out/lib/" + name
		if _, err := os.Stat(path); err == nil {
			os.Setenv("AGENT_CLI_CONNECTOR_LIB", path)
		}
	}
	os.Exit(m.Run())
}
