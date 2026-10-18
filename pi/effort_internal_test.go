package pi

import (
	"encoding/json"
	"slices"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

func TestModelEfforts(t *testing.T) {
	var m modelInfo
	if err := json.Unmarshal([]byte(`{"reasoning":true,"thinkingLevelMap":{"minimal":null,"xhigh":"xhigh"}}`), &m); err != nil {
		t.Fatal(err)
	}
	got := m.efforts()
	want := []agent.Effort{agent.EffortNone, agent.EffortLow, agent.EffortMedium, agent.EffortHigh, agent.EffortXHigh}
	if !slices.Equal(got, want) {
		t.Errorf("efforts = %v, want %v", got, want)
	}
	if e := clampEffort(agent.EffortMinimal, got); e != agent.EffortLow {
		t.Errorf("clamp minimal = %q", e)
	}
	if e := clampEffort(agent.EffortMax, got); e != agent.EffortXHigh {
		t.Errorf("clamp max = %q", e)
	}
	if (&modelInfo{}).efforts() != nil {
		t.Error("non-reasoning model has efforts")
	}
}
