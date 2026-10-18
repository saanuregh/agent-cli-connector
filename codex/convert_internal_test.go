package codex

import (
	"errors"
	"reflect"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

func TestUserInputSendsFilesAsPathText(t *testing.T) {
	got, err := userInput([]agent.Part{agent.FileRef("add.go"), agent.MentionPart("app", "app://x")})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"type": "text", "text": providerutil.AbsDir("add.go"), "text_elements": []any{}},
		{"type": "mention", "name": "app", "path": "app://x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("userInput = %v, want %v", got, want)
	}
	_, err = userInput([]agent.Part{{Type: agent.PartFile, Data: []byte("x")}})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("file part without a path = %v", err)
	}
}

func TestHookOutcome(t *testing.T) {
	for status, want := range map[string]agent.HookOutcome{
		"completed": agent.HookSucceeded,
		"failed":    agent.HookFailed,
		"blocked":   agent.HookBlocked,
		"stopped":   agent.HookBlocked,
		"running":   "",
		"other":     "",
	} {
		if got := hookOutcome(status); got != want {
			t.Errorf("hookOutcome(%q) = %q, want %q", status, got, want)
		}
	}
}
