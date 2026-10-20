// Command schemarefresh regenerates codex/testdata/app-server.schema.json
// from the codex CLI on PATH. It runs from the codex package directory via
// go generate.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	generatedName = "codex_app_server_protocol.schemas.json"
	snapshotPath  = "testdata/app-server.schema.json"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "schemarefresh:", err)
		os.Exit(1)
	}
}

func run() error {
	tempDir, err := os.MkdirTemp("", "codex-schema-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	// An empty home keeps the user's Codex config and login out of the output.
	home := filepath.Join(tempDir, "home")
	outDir := filepath.Join(tempDir, "out")
	if err := os.Mkdir(home, 0o700); err != nil {
		return err
	}
	cmd := exec.Command("codex", "app-server", "generate-json-schema", "--experimental", "--out", outDir)
	cmd.Env = append(os.Environ(), "HOME="+home, "CODEX_HOME="+home, "USERPROFILE="+home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codex app-server generate-json-schema: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, generatedName))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(snapshotPath, data, 0o644)
}
