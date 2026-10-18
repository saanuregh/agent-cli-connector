package claude

import (
	"testing"

	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

func TestUUID(t *testing.T) {
	id := providerutil.NewUUID()
	if !isUUID(id) || id[14] != '4' {
		t.Errorf("uuid %q", id)
	}
	if isUUID("not-a-uuid") {
		t.Error("accepted invalid uuid")
	}
}

func TestHistoryItemID(t *testing.T) {
	ids := map[string]bool{}
	for block := range 12 {
		id := historyItemID("u", block)
		if ids[id] {
			t.Fatalf("block %d reuses id %q", block, id)
		}
		ids[id] = true
	}
	if got := historyItemID("u", 0); got != "u" {
		t.Errorf("block 0 = %q", got)
	}
	if got := historyItemID("u", 11); got != "u/11" {
		t.Errorf("block 11 = %q", got)
	}
}
