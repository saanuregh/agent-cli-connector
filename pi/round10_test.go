package pi

import (
	"encoding/json"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

func partial(t *testing.T, text string, total int64) partialOutput {
	t.Helper()
	r := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	if total > 0 {
		r["details"] = map[string]any{"truncation": map[string]any{"truncated": true, "totalBytes": total}}
	}
	raw, _ := json.Marshal(r)
	return newPartialOutput(raw)
}

// piSnapshot builds the partial result Pi's bash tool sends for output
// full: all of it while short, else its last maxLines lines joined without
// the final newline, with the total size of full.
func piSnapshot(t *testing.T, full string, maxLines int) partialOutput {
	lines := strings.Split(full, "\n")
	if strings.HasSuffix(full, "\n") {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= maxLines {
		return partial(t, full, 0)
	}
	return partial(t, strings.Join(lines[len(lines)-maxLines:], "\n"), int64(len(full)))
}

func joined(updates []partialOutput) string {
	var b strings.Builder
	var prev partialOutput
	for _, u := range updates {
		b.WriteString(outputDelta(prev, &u))
		prev = u
	}
	return b.String()
}

func TestOutputDeltaFollowsTruncatedSnapshots(t *testing.T) {
	// Output arrives in pieces that end mid-line, on newlines, and inside
	// repeated lines; some lines hold multi-byte characters.
	pieces := []string{"ok\nok\n", "ok\nhalf ", "line ü\n", "ok\nok\nok\n", "é", "è\n", "ok\n", "end\nok", "\n", "tail"}
	var full string
	var updates []partialOutput
	for _, p := range pieces {
		full += p
		updates = append(updates, piSnapshot(t, full, 4))
	}
	if got := joined(updates); got != full {
		t.Errorf("joined deltas = %q, want %q", got, full)
	}
	// More new output than one snapshot holds: the whole snapshot is new.
	got := joined([]partialOutput{partial(t, "a\n", 0), partial(t, "x\ny", 100)})
	if got != "a\nx\ny\n" && got != "a\nx\ny" {
		t.Errorf("joined = %q", got)
	}
}

func TestOutputDeltaReplacedProgress(t *testing.T) {
	got := joined([]partialOutput{partial(t, "Parallel: 0/3 done", 0), partial(t, "Parallel: 1/3 done", 0), partial(t, "Parallel: 1/3 done, writing", 0)})
	if want := "Parallel: 0/3 done\nParallel: 1/3 done, writing"; got != want {
		t.Errorf("joined = %q, want %q", got, want)
	}
}

func TestClampEffortPrefersHigherLevels(t *testing.T) {
	if got := clampEffort("high", []agent.Effort{"low", "medium", "max"}); got != "max" {
		t.Errorf("clamp high = %q, want max", got)
	}
	if got := clampEffort("max", []agent.Effort{"low", "medium"}); got != "medium" {
		t.Errorf("clamp max = %q, want medium", got)
	}
}
