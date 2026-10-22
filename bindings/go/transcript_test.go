package agent_test

import (
	"slices"
	"strings"
	"sync"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector/bindings/go"
)

func event(turnID string, p agent.EventPayload) agent.Event {
	return agent.Event{EventMeta: agent.EventMeta{TurnID: turnID}, Type: p.EventType(), Payload: p}
}

func TestTranscriptDeltas(t *testing.T) {
	var tr agent.Transcript
	tr.Apply(event("t1", agent.TextDelta{ItemID: "a", Delta: "hel"}))
	ch, ok := tr.Apply(event("t1", agent.TextDelta{ItemID: "a", Delta: "lo"}))
	if !ok || ch.Item.Text != "hello" || ch.Item.Kind != agent.ItemKindAssistantMessage || ch.Item.TurnID != "t1" || ch.Item.Status != agent.ItemStatusInProgress {
		t.Fatalf("text change %+v", ch)
	}
	tr.Apply(event("t1", agent.ReasoningDelta{ItemID: "r", ParentID: "p", SubagentID: "s", Delta: "think"}))
	tr.Apply(event("t1", agent.PlanDelta{ItemID: "plan", Delta: "step"}))
	if it, _ := tr.Item("r"); it.Kind != agent.ItemKindReasoning || it.ParentID != "p" || it.SubagentID != "s" || it.Text != "think" {
		t.Fatalf("reasoning %+v", it)
	}
	if it, _ := tr.Item("plan"); it.Kind != agent.ItemKindPlan || it.Text != "step" {
		t.Fatalf("plan %+v", it)
	}

	tool := &agent.ToolCall{Name: "sh"}
	tr.Apply(event("t1", agent.ItemStarted{Item: agent.Item{ID: "c", Kind: agent.ItemKindToolCall, Status: agent.ItemStatusInProgress, Tool: tool}}))
	tr.Apply(event("t1", agent.ToolOutputDelta{ItemID: "c", Delta: "out"}))
	ch, _ = tr.Apply(event("t1", agent.ToolOutputDelta{ItemID: "c", Delta: "put"}))
	if ch.Item.Tool.Output != "output" || tool.Output != "" {
		t.Fatalf("tool output %q, shared tool %q", ch.Item.Tool.Output, tool.Output)
	}
	if ids := itemIDs(tr.Items()); !slices.Equal(ids, []string{"a", "r", "plan", "c"}) {
		t.Fatalf("order %v", ids)
	}
	if _, ok := tr.Apply(event("t1", agent.StatusChanged{})); ok {
		t.Fatal("status change reported a change")
	}
}

func TestTranscriptMerge(t *testing.T) {
	var tr agent.Transcript
	tr.Apply(event("t1", agent.TextDelta{ItemID: "a", Delta: "streamed"}))
	ch, _ := tr.Apply(event("t1", agent.ItemUpdated{Item: agent.Item{ID: "a", Kind: agent.ItemKindAssistantMessage, Status: agent.ItemStatusCompleted}}))
	if ch.Item.Text != "streamed" {
		t.Fatalf("update dropped text: %+v", ch.Item)
	}
	tr.Apply(event("t1", agent.ToolOutputDelta{ItemID: "c", Delta: "out"}))
	ch, _ = tr.Apply(event("t1", agent.ItemStarted{Item: agent.Item{ID: "c", Status: agent.ItemStatusInProgress, Tool: &agent.ToolCall{Name: "sh"}}}))
	if ch.Item.Tool.Output != "out" || ch.Item.Tool.Name != "sh" {
		t.Fatalf("running item dropped output: %+v", ch.Item.Tool)
	}
	ch, _ = tr.Apply(event("t1", agent.ItemCompleted{Item: agent.Item{ID: "a", Text: "final", Status: agent.ItemStatusCompleted}}))
	if ch.Item.Text != "final" {
		t.Fatalf("completed %+v", ch.Item)
	}
	tr.Apply(event("t1", agent.TextDelta{ItemID: "a", Delta: "!"}))
	if it, _ := tr.Item("a"); it.Text != "final!" {
		t.Fatalf("delta after put %q", it.Text)
	}
}

func TestTranscriptRemovalAndReset(t *testing.T) {
	var tr agent.Transcript
	for _, id := range []string{"a", "b", "c"} {
		tr.Apply(event("t1", agent.ItemCompleted{Item: agent.Item{ID: id}}))
	}
	ch, ok := tr.Apply(event("t1", agent.ItemsRemoved{Ids: []string{"b", "missing"}}))
	if !ok || !slices.Equal(ch.Removed, []string{"b"}) || !slices.Equal(itemIDs(tr.Items()), []string{"a", "c"}) {
		t.Fatalf("removed %v, items %v", ch.Removed, itemIDs(tr.Items()))
	}
	if _, ok := tr.Apply(event("t1", agent.ItemsRemoved{Ids: []string{"missing"}})); ok {
		t.Fatal("removing nothing reported a change")
	}
	if it, _ := tr.Item("c"); it.ID != "c" {
		t.Fatal("index not rebuilt")
	}
	ch, ok = tr.Apply(event("", agent.ConversationReset{}))
	if !ok || !slices.Equal(ch.Removed, []string{"a", "c"}) || len(tr.Items()) != 0 {
		t.Fatalf("reset %v, items %v", ch.Removed, tr.Items())
	}
	tr.Apply(event("t1", agent.ItemCompleted{Item: agent.Item{ID: "x"}}))
	tr.Reset()
	if len(tr.Items()) != 0 {
		t.Fatal("Reset kept items")
	}
}

func TestTranscriptLoadAndByTurn(t *testing.T) {
	var tr agent.Transcript
	tr.Apply(event("old", agent.ItemCompleted{Item: agent.Item{ID: "gone"}}))
	tr.Load([]agent.Item{{ID: "u1", TurnID: "t1"}, {ID: "a1", TurnID: "t1", Text: "hi"}, {ID: "u2", TurnID: "t2"}})
	if _, ok := tr.Item("gone"); ok {
		t.Fatal("Load kept old items")
	}
	tr.Apply(event("t2", agent.TextDelta{ItemID: "a2", Delta: "yo"}))
	tr.Apply(event("t1", agent.TextDelta{ItemID: "a1", Delta: "!"}))
	groups := tr.ByTurn()
	if len(groups) != 2 || groups[0].TurnID != "t1" || !slices.Equal(itemIDs(groups[0].Items), []string{"u1", "a1"}) ||
		groups[1].TurnID != "t2" || !slices.Equal(itemIDs(groups[1].Items), []string{"u2", "a2"}) {
		t.Fatalf("by turn %+v", groups)
	}
	if groups[0].Items[1].Text != "hi!" {
		t.Fatalf("loaded text %q", groups[0].Items[1].Text)
	}
}

func TestTranscriptConcurrent(t *testing.T) {
	var tr agent.Transcript
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				tr.Apply(event("t", agent.TextDelta{ItemID: "a", Delta: "x"}))
				_ = tr.Items()
				_ = tr.ByTurn()
			}
		}()
	}
	wg.Wait()
	if it, _ := tr.Item("a"); it.Text != strings.Repeat("x", 800) {
		t.Fatalf("text length %d", len(it.Text))
	}
}

func itemIDs(items []agent.Item) []string {
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}
