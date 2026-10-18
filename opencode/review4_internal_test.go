package opencode

import (
	"encoding/json"
	"sync"
	"testing"
)

// formAnswer matches OpenCode's server check: external fields are always
// acknowledged, list answers compare strictly, and required fields take
// their defaults when unanswered.
func TestFormAnswerMatchesServerCheck(t *testing.T) {
	var fields []formField
	if err := json.Unmarshal([]byte(`[
	 {"key":"mode","type":"string","options":[{"value":"oauth","label":"OAuth"},{"value":"key","label":"Key"}]},
	 {"key":"open","type":"external","url":"https://x","when":[{"key":"mode","op":"eq","value":"oauth"}]},
	 {"key":"pick","type":"multiselect","options":[{"value":"1"},{"value":"2"}]},
	 {"key":"extra","type":"string","when":[{"key":"pick","op":"eq","value":1}]},
	 {"key":"same","type":"string","when":[{"key":"pick","op":"eq","value":"1"}]},
	 {"key":"region","type":"string","required":true,"default":"eu"}
	]`), &fields); err != nil {
		t.Fatal(err)
	}
	out := formAnswer(fields, map[string][]string{"mode": {"Key"}, "pick": {"1"}, "extra": {"x"}, "same": {"y"}})
	if out["open"] != true {
		t.Errorf("external field not acknowledged: %v", out)
	}
	if _, ok := out["extra"]; ok {
		t.Errorf("a number matched a list item: %v", out)
	}
	if out["same"] != "y" {
		t.Errorf("an equal string did not match: %v", out)
	}
	if out["region"] != "eu" {
		t.Errorf("required field without its default: %v", out)
	}
}

// MCP directory locks are freed once nobody holds or waits for them.
func TestMCPDirLocksFreed(t *testing.T) {
	p := New(Options{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unlock := p.lockMCPDir([]string{"a", "b"}[i%2])
			unlock()
		}(i)
	}
	wg.Wait()
	p.mcpMu.Lock()
	defer p.mcpMu.Unlock()
	if len(p.mcpDirLocks) != 0 {
		t.Fatalf("locks left: %d", len(p.mcpDirLocks))
	}
}
