package agent

import (
	"slices"
	"strings"
	"sync"
)

// Transcript folds events into the current list of transcript items,
// assembling streamed text, reasoning, and tool output as it arrives. A
// chat UI can render Items after each Apply instead of tracking deltas
// itself. The zero value is ready to use and safe for concurrent use.
type Transcript struct {
	mu    sync.Mutex
	items []Item
	index map[string]int
	// text and output build streamed text and tool output in linear time;
	// an item's string shares its builder's buffer.
	text   map[string]*strings.Builder
	output map[string]*strings.Builder
}

// Apply updates the transcript with ev. It returns the changed item and
// true when ev affected an item. For ItemsRemoved and ConversationReset it
// returns a zero Item and true when an item was removed.
func (t *Transcript) Apply(ev Event) (Item, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch ev := ev.(type) {
	case *ItemStarted:
		return t.put(ev.Item, false), true
	case *ItemUpdated:
		return t.put(ev.Item, true), true
	case *ItemCompleted:
		return t.put(ev.Item, false), true
	case *TextDelta:
		it := t.ensure(ev.ItemID, ItemAssistantMessage, ev.ParentID, ev.SubagentID)
		it.Text = appendTo(&t.text, it.ID, it.Text, ev.Delta)
		return *it, true
	case *ReasoningDelta:
		it := t.ensure(ev.ItemID, ItemReasoning, ev.ParentID, ev.SubagentID)
		it.Text = appendTo(&t.text, it.ID, it.Text, ev.Delta)
		return *it, true
	case *PlanDelta:
		it := t.ensure(ev.ItemID, ItemPlan, "", "")
		it.Text = appendTo(&t.text, it.ID, it.Text, ev.Delta)
		return *it, true
	case *ItemsRemoved:
		return Item{}, t.remove(ev.IDs)
	case *ConversationReset:
		removed := len(t.items) > 0
		t.resetLocked()
		return Item{}, removed
	case *ToolOutputDelta:
		it := t.ensure(ev.ItemID, ItemToolCall, "", "")
		// Replace, never modify, the ToolCall: event items and returned
		// items share it.
		call := ToolCall{}
		if it.Tool != nil {
			call = *it.Tool
		}
		call.Output = appendTo(&t.output, it.ID, call.Output, ev.Delta)
		it.Tool = &call
		return *it, true
	}
	return Item{}, false
}

// put stores item. With merge, text and tool output assembled from deltas
// are kept when the update carries none.
func (t *Transcript) put(item Item, merge bool) Item {
	if t.index == nil {
		t.index = make(map[string]int)
	}
	// The stored text may change; later deltas start a new builder from it.
	delete(t.text, item.ID)
	delete(t.output, item.ID)
	i, ok := t.index[item.ID]
	if !ok {
		t.index[item.ID] = len(t.items)
		t.items = append(t.items, item)
		return item
	}
	old := t.items[i]
	if merge || item.Status == ItemRunning {
		if item.Text == "" {
			item.Text = old.Text
		}
		if item.Tool != nil && old.Tool != nil && item.Tool.Output == "" {
			call := *item.Tool
			call.Output = old.Tool.Output
			item.Tool = &call
		}
	}
	t.items[i] = item
	return item
}

func (t *Transcript) remove(ids []string) bool {
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		if _, ok := t.index[id]; ok {
			drop[id] = true
			delete(t.index, id)
			delete(t.text, id)
			delete(t.output, id)
		}
	}
	if len(drop) == 0 {
		return false
	}
	t.items = slices.DeleteFunc(t.items, func(it Item) bool { return drop[it.ID] })
	for i, it := range t.items {
		t.index[it.ID] = i
	}
	return true
}

// appendTo appends delta to the text built for id, seeding the builder
// with current, and returns the new text.
func appendTo(builders *map[string]*strings.Builder, id, current, delta string) string {
	if *builders == nil {
		*builders = make(map[string]*strings.Builder)
	}
	b := (*builders)[id]
	if b == nil {
		b = &strings.Builder{}
		b.WriteString(current)
		(*builders)[id] = b
	}
	b.WriteString(delta)
	return b.String()
}

func (t *Transcript) resetLocked() {
	t.items, t.index, t.text, t.output = nil, nil, nil, nil
}

func (t *Transcript) ensure(id string, kind ItemKind, parent, subagent string) *Item {
	if t.index == nil {
		t.index = make(map[string]int)
	}
	if i, ok := t.index[id]; ok {
		return &t.items[i]
	}
	t.index[id] = len(t.items)
	t.items = append(t.items, Item{ID: id, Kind: kind, Status: ItemRunning, ParentID: parent, SubagentID: subagent})
	return &t.items[len(t.items)-1]
}

// Items returns a copy of the items in order of first appearance.
func (t *Transcript) Items() []Item {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Item, len(t.items))
	copy(out, t.items)
	return out
}

// Item returns the item with the given ID.
func (t *Transcript) Item(id string) (Item, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	i, ok := t.index[id]
	if !ok {
		return Item{}, false
	}
	return t.items[i], true
}

// Reset removes every item.
func (t *Transcript) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resetLocked()
}
