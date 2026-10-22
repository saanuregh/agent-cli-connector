package agent

import (
	"slices"
	"strings"
	"sync"
)

// Transcript folds events into the current list of transcript items,
// assembling streamed text, reasoning, plans, and tool output as it
// arrives. A chat UI can render Items after each Apply instead of tracking
// deltas itself. Subagent items stay in the flat list; ParentID and
// SubagentID say where they nest. The zero value is ready to use and safe
// for concurrent use.
type Transcript struct {
	mu    sync.Mutex
	items []Item
	index map[string]int
	// text and output build streamed text and tool output in linear time;
	// an item's string shares its builder's buffer.
	text   map[string]*strings.Builder
	output map[string]*strings.Builder
}

// TranscriptChange is what one Apply changed: the updated item, or the IDs
// of the items an ItemsRemoved or ConversationReset removed.
type TranscriptChange struct {
	Item    Item
	Removed []string
}

// TurnItems is the items of one turn, in order of first appearance.
type TurnItems struct {
	TurnID string
	Items  []Item
}

// Apply updates the transcript with ev. It reports false when ev changed
// nothing.
func (t *Transcript) Apply(ev Event) (TranscriptChange, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch p := ev.Payload.(type) {
	case ItemStarted:
		return TranscriptChange{Item: t.put(p.Item, false)}, true
	case ItemUpdated:
		return TranscriptChange{Item: t.put(p.Item, true)}, true
	case ItemCompleted:
		return TranscriptChange{Item: t.put(p.Item, false)}, true
	case TextDelta:
		return t.appendText(ev.TurnID, p.ItemID, ItemKindAssistantMessage, p.ParentID, p.SubagentID, p.Delta), true
	case ReasoningDelta:
		return t.appendText(ev.TurnID, p.ItemID, ItemKindReasoning, p.ParentID, p.SubagentID, p.Delta), true
	case PlanDelta:
		return t.appendText(ev.TurnID, p.ItemID, ItemKindPlan, p.ParentID, p.SubagentID, p.Delta), true
	case ToolOutputDelta:
		it := t.ensure(ev.TurnID, p.ItemID, ItemKindToolCall, p.ParentID, p.SubagentID)
		// Replace, never modify, the ToolCall: event items and returned
		// items share it.
		call := ToolCall{}
		if it.Tool != nil {
			call = *it.Tool
		}
		call.Output = appendTo(&t.output, it.ID, call.Output, p.Delta)
		it.Tool = &call
		return TranscriptChange{Item: *it}, true
	case ItemsRemoved:
		removed := t.remove(p.Ids)
		return TranscriptChange{Removed: removed}, len(removed) > 0
	case ConversationReset:
		removed := make([]string, len(t.items))
		for i, it := range t.items {
			removed[i] = it.ID
		}
		t.resetLocked()
		return TranscriptChange{Removed: removed}, len(removed) > 0
	}
	return TranscriptChange{}, false
}

func (t *Transcript) appendText(turnID, id string, kind ItemKind, parent, subagent, delta string) TranscriptChange {
	it := t.ensure(turnID, id, kind, parent, subagent)
	it.Text = appendTo(&t.text, it.ID, it.Text, delta)
	return TranscriptChange{Item: *it}
}

// put stores item. With merge, or while the item runs, text and tool
// output assembled from deltas are kept when the item carries none.
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
	if merge || item.Status == ItemStatusInProgress {
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

// remove drops the items with the given IDs and returns the IDs it found.
func (t *Transcript) remove(ids []string) []string {
	var removed []string
	drop := map[string]bool{}
	for _, id := range ids {
		if _, ok := t.index[id]; ok {
			removed = append(removed, id)
			drop[id] = true
			delete(t.index, id)
			delete(t.text, id)
			delete(t.output, id)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	t.items = slices.DeleteFunc(t.items, func(it Item) bool { return drop[it.ID] })
	for i, it := range t.items {
		t.index[it.ID] = i
	}
	return removed
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

// ensure returns the item with id, creating a running item of kind when
// it is unknown.
func (t *Transcript) ensure(turnID, id string, kind ItemKind, parent, subagent string) *Item {
	if t.index == nil {
		t.index = make(map[string]int)
	}
	if i, ok := t.index[id]; ok {
		return &t.items[i]
	}
	t.index[id] = len(t.items)
	t.items = append(t.items, Item{ID: id, Kind: kind, Status: ItemStatusInProgress, ParentID: parent, SubagentID: subagent, TurnID: turnID})
	return &t.items[len(t.items)-1]
}

// Items returns a copy of the items in order of first appearance.
func (t *Transcript) Items() []Item {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.items)
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

// ByTurn returns the items grouped by TurnID, the groups in order of
// their first item. Items without a turn form a group with an empty TurnID.
func (t *Transcript) ByTurn() []TurnItems {
	t.mu.Lock()
	defer t.mu.Unlock()
	var groups []TurnItems
	group := map[string]int{}
	for _, it := range t.items {
		i, ok := group[it.TurnID]
		if !ok {
			i = len(groups)
			group[it.TurnID] = i
			groups = append(groups, TurnItems{TurnID: it.TurnID})
		}
		groups[i].Items = append(groups[i].Items, it)
	}
	return groups
}

// Load replaces the transcript with items, such as the result of
// Client.ReadHistory.
func (t *Transcript) Load(items []Item) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resetLocked()
	for _, it := range items {
		t.put(it, false)
	}
}

// Reset removes every item.
func (t *Transcript) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resetLocked()
}
