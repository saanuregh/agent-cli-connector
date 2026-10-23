package agent_test

import (
	"os"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector/bindings/go"
)

// TestPlatformModuleVersions checks that go.mod requires the platform modules
// at this library version. Inside this repository the replace directives
// hide a stale requirement; users outside it would link that older archive.
func TestPlatformModuleVersions(t *testing.T) {
	goMod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "github.com/saanuregh/agent-cli-connector/bindings/go/lib/"
	want := "v" + agent.LibraryVersion
	found := 0
	for line := range strings.Lines(string(goMod)) {
		line, _, _ = strings.Cut(line, "//")
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "require" {
			fields = fields[1:]
		}
		// Replace directives ("<module> => <path>") are not requirements.
		if len(fields) != 2 || !strings.HasPrefix(fields[0], prefix) {
			continue
		}
		found++
		if fields[1] != want {
			t.Errorf("go.mod requires %s %s, want %s", fields[0], fields[1], want)
		}
	}
	entries, err := os.ReadDir("lib")
	if err != nil {
		t.Fatal(err)
	}
	platforms := 0
	for _, entry := range entries {
		if entry.IsDir() {
			platforms++
		}
	}
	if found != platforms {
		t.Errorf("go.mod requires %d platform modules, lib has %d", found, platforms)
	}
}
