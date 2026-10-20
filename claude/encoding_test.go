package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

func TestProjectDirNameMatchesSDK(t *testing.T) {
	long := "/home/u/" + strings.Repeat("a", 250)
	got := projectDirName(long)
	if len(got) != 207 || !strings.HasSuffix(got, "a-bkw3mj") {
		t.Errorf("long path = %q (len %d)", got[len(got)-20:], len(got))
	}
	// Non-ASCII characters count per UTF-16 unit, like the SDK's regexp.
	if got := projectDirName("/tmp/é😀x"); got != "-tmp----x" {
		t.Errorf("non-ASCII = %q", got)
	}
	if got := projectDirName("/a/b.c"); got != "-a-b-c" {
		t.Errorf("short = %q", got)
	}
}

func TestProjectDirsResolveSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	dirs := projectDirs("/cfg", link)
	resolved, _ := filepath.EvalSymlinks(real)
	if len(dirs) != 2 || filepath.Base(dirs[0]) != projectDirName(resolved) || filepath.Base(dirs[1]) != projectDirName(link) {
		t.Errorf("dirs = %v", dirs)
	}
}

func TestTranscriptTextHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>main.go</command-args>": "/review main.go",
		"<command-name>clear</command-name><command-args></command-args>":                                                       "/clear",
		"plain": "plain",
	} {
		if got := normalizeCommand(in); got != want {
			t.Errorf("normalizeCommand(%q) = %q, want %q", in, got, want)
		}
	}
	for _, noise := range []string{"[Request interrupted by user]", "[Request interrupted by user for tool use]", " No response requested. ", "<local-command-stdout>ok</local-command-stdout>"} {
		if !isTranscriptNoise(noise) {
			t.Errorf("%q not noise", noise)
		}
	}
	if isTranscriptNoise("hello [Request interrupted by user]") {
		t.Error("prompt treated as noise")
	}
}

func TestFormatWorkflowResult(t *testing.T) {
	for in, want := range map[string]string{
		`{"report":{"text":" done "}}`: "done",
		`{"a":1,"b":2}`:                "```json\n{\n  \"a\": 1,\n  \"b\": 2\n}\n```",
		`42`:                           "42",
		`true`:                         "true",
		`null`:                         "",
	} {
		if got := formatWorkflowResult(json.RawMessage(in)); got != want {
			t.Errorf("formatWorkflowResult(%s) = %q, want %q", in, got, want)
		}
	}
	dir := t.TempDir()
	small := filepath.Join(dir, "out.json")
	_ = os.WriteFile(small, []byte(`{"result":{"summary":"all good"}}`), 0o644)
	if got := readWorkflowOutputFrom(agent.LocalFileSystem, small); got != "all good" {
		t.Errorf("readWorkflowOutputFrom = %q", got)
	}
	big := filepath.Join(dir, "big.json")
	_ = os.WriteFile(big, []byte(`{"result":"`+strings.Repeat("x", maxWorkflowOutput)+`"}`), 0o644)
	if got := readWorkflowOutputFrom(agent.LocalFileSystem, big); got != "" {
		t.Errorf("oversized output read: %d bytes", len(got))
	}
	if got := readWorkflowOutputFrom(agent.LocalFileSystem, filepath.Join(dir, "missing")); got != "" {
		t.Errorf("missing output = %q", got)
	}
}
