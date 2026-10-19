package claude

import "testing"

func TestManagedMCPConfigPath(t *testing.T) {
	for goos, want := range map[string]string{
		"linux":   "/etc/claude-code/managed-mcp.json",
		"darwin":  "/Library/Application Support/ClaudeCode/managed-mcp.json",
		"windows": `C:\Program Files\ClaudeCode\managed-mcp.json`,
	} {
		if got := managedMCPConfigPath(goos); got != want {
			t.Errorf("%s: %q; want %q", goos, got, want)
		}
	}
}
