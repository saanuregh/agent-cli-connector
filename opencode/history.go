package opencode

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

// historyMessage is the part of OpenCode's SessionMessageInfo history uses.
type historyMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
	Text     string                   `json:"text"`
	Metadata json.RawMessage          `json:"metadata"`
	Content  []historyPart            `json:"content"`
	Error    *structuredError         `json:"error"`
	Status   string                   `json:"status"`
	Summary  string                   `json:"summary"`
	Command  string                   `json:"command"`
	Exit     json.RawMessage          `json:"exit"`
	Output   *struct{ Output string } `json:"output"`
}

type historyPart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	State struct {
		Status   string           `json:"status"`
		Input    json.RawMessage  `json:"input"`
		Content  []toolContent    `json:"content"`
		Metadata json.RawMessage  `json:"metadata"`
		Error    *structuredError `json:"error"`
	} `json:"state"`
}

// listMessages reads one page of a session's messages. order is "asc" or
// "desc" for the first page; later pages pass only the cursor, because
// OpenCode refuses a cursor combined with an order.
func listMessages(ctx context.Context, c *Client, sid, order, cursor string, limit int) ([]json.RawMessage, string, error) {
	q := []string{param("limit", strconv.Itoa(limit))}
	if cursor != "" {
		q = append(q, param("cursor", cursor))
	} else {
		q = append(q, param("order", order))
	}
	var page struct {
		Data   []json.RawMessage `json:"data"`
		Cursor struct {
			Next *string `json:"next"`
		} `json:"cursor"`
	}
	if err := c.Call(ctx, "GET", withQuery(sessionPath(sid, "/message"), q...), nil, &page); err != nil {
		return nil, "", err
	}
	next := ""
	if page.Cursor.Next != nil && len(page.Data) > 0 {
		next = *page.Cursor.Next
	}
	return page.Data, next, nil
}

// allMessages reads every message of a session, oldest first.
func allMessages(ctx context.Context, c *Client, sid string) ([]json.RawMessage, error) {
	var out []json.RawMessage
	cursor := ""
	for {
		page, next, err := listMessages(ctx, c, sid, "asc", cursor, 100)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// readHistory reads a session's transcript: all of it when limit is zero,
// otherwise only as many of the newest messages as the page needs. Tool
// call IDs start with toolPrefix, as in a live session; see historyItems.
func readHistory(ctx context.Context, c *Client, sid, toolPrefix string, limit, offset int) ([]agent.Item, error) {
	if limit <= 0 {
		msgs, err := allMessages(ctx, c, sid)
		if err != nil {
			return nil, err
		}
		return providerutil.LastPage(historyItems(msgs, toolPrefix), 0, offset), nil
	}
	var newest []json.RawMessage
	count := 0
	cursor := ""
	for {
		page, next, err := listMessages(ctx, c, sid, "desc", cursor, 100)
		if err != nil {
			return nil, err
		}
		newest = append(newest, page...)
		for _, raw := range page {
			count += len(messageItems(raw, "", ""))
		}
		// The oldest message read must end an execution, so the next one
		// starts a turn, or the items before it would carry no turn ID.
		if next == "" || count >= limit+offset && len(newest) > 0 && messageType(newest[len(newest)-1]) == "idle" {
			slices.Reverse(newest)
			return providerutil.LastPage(historyItems(newest, toolPrefix), limit, offset), nil
		}
		cursor = next
	}
}

func messageType(raw json.RawMessage) string {
	var m struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Type
}

// messageTurns returns the turn of each message, oldest first. A turn is
// one execution: the messages after an idle record through the next one.
// Its ID is its first user message, else its first synthetic message, such
// as a subagent report that woke the session, else its compaction. Other
// messages of the turn, such as mode reminders OpenCode inserts before the
// prompt and steers delivered into the turn, carry that ID; a turn without
// such a message continues the previous one.
func messageTurns(msgs []json.RawMessage) []string {
	turns := make([]string, len(msgs))
	turn := ""
	for start := 0; start < len(msgs); {
		end := start
		for end < len(msgs) && messageType(msgs[end]) != "idle" {
			end++
		}
		if end < len(msgs) {
			end++ // the idle record closes the turn
		}
		best, rank := "", 0
		for _, raw := range msgs[start:end] {
			r := map[string]int{"user": 3, "synthetic": 2, "compaction": 1}[messageType(raw)]
			if r > rank {
				best, rank = requestID(raw), r
			}
		}
		if best != "" {
			turn = best
		}
		for i := start; i < end; i++ {
			turns[i] = turn
		}
		start = end
	}
	return turns
}

// historyItems converts messages, oldest first, to transcript items. Each
// item's TurnID is its turn; see messageTurns. Tool call IDs start with
// toolPrefix: a live session gives a subagent's tool calls the prefix
// "<subagent session ID>/".
func historyItems(msgs []json.RawMessage, toolPrefix string) []agent.Item {
	var items []agent.Item
	turnOf := messageTurns(msgs)
	for i, raw := range msgs {
		items = append(items, messageItems(raw, turnOf[i], toolPrefix)...)
	}
	return items
}

// messageItems converts one message of turn to transcript items. A failed
// step's error is not an item, as in a live session, which reports it as
// a notice and in the turn's result.
func messageItems(raw json.RawMessage, turn, toolPrefix string) []agent.Item {
	var items []agent.Item
	var m historyMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	at := providerutil.FromUnixMilli(m.Time.Created)
	add := func(it agent.Item) {
		it.TurnID, it.Time = turn, at
		items = append(items, it)
	}
	switch m.Type {
	case "user", "synthetic":
		var meta struct {
			ClientID string `json:"clientID"`
		}
		_ = json.Unmarshal(m.Metadata, &meta)
		add(agent.Item{ID: m.ID, Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: m.Text, ClientID: meta.ClientID, Synthetic: m.Type == "synthetic", Raw: raw})
	case "system", "skill":
		add(agent.Item{ID: m.ID, Kind: agent.ItemOther, Status: agent.ItemDone, Text: m.Text, Raw: raw})
	case "shell":
		call := &agent.ToolCall{Name: "shell", Kind: agent.ToolShell, Command: m.Command}
		if m.Output != nil {
			call.Output = m.Output.Output
		}
		var code *int
		if json.Unmarshal(m.Exit, &code) == nil {
			call.ExitCode = code
		}
		status := agent.ItemDone
		switch m.Status {
		case "running":
			status = agent.ItemRunning
		case "timeout", "killed":
			status = agent.ItemFailed
		}
		add(agent.Item{ID: m.ID, Kind: agent.ItemToolCall, Status: status, Tool: call, Raw: raw})
	case "compaction":
		it := agent.Item{ID: m.ID, Kind: agent.ItemCompaction, Status: agent.ItemDone, Text: m.Summary, Raw: raw}
		switch m.Status {
		case "running":
			it.Status = agent.ItemRunning
		case "failed":
			it.Status = agent.ItemFailed
			if m.Error != nil {
				it.Text = m.Error.Message
			}
		}
		add(it)
	case "assistant":
		ordinals := map[string]int{}
		for _, part := range m.Content {
			switch part.Type {
			case "text", "reasoning":
				kind := agent.ItemAssistantMessage
				if part.Type == "reasoning" {
					kind = agent.ItemReasoning
				}
				id := m.ID + ":" + part.Type + ":" + strconv.Itoa(ordinals[part.Type])
				ordinals[part.Type]++
				add(agent.Item{ID: id, Kind: kind, Status: agent.ItemDone, Text: part.Text})
			case "tool":
				it := historyTool(part)
				it.ID = toolPrefix + it.ID
				add(it)
			}
		}
	}
	return items
}

// historyTool converts a tool part. Host and MCP tools are not told apart
// in history; they are ToolOther unless the name is a built-in tool.
func historyTool(part historyPart) agent.Item {
	call := &agent.ToolCall{Name: part.Name, Kind: builtinToolKind(part.Name)}
	if len(part.State.Input) > 0 && part.State.Input[0] == '{' {
		call.Input = part.State.Input
		fillToolDetails(call, part.State.Input)
	}
	call.Output, call.Images = contentText(part.State.Content)
	if len(part.State.Metadata) > 0 && string(part.State.Metadata) != "null" {
		call.OutputData = part.State.Metadata
		var m struct {
			Exit *int `json:"exit"`
		}
		if json.Unmarshal(part.State.Metadata, &m) == nil {
			call.ExitCode = m.Exit
		}
	}
	it := agent.Item{ID: part.ID, Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: call}
	switch part.State.Status {
	case "streaming", "running":
		it.Status = agent.ItemRunning
	case "error":
		call.IsError = true
		it.Status = agent.ItemFailed
		if part.State.Error != nil {
			if call.Output == "" {
				call.Output = part.State.Error.Message
			}
			switch part.State.Error.Type {
			case "aborted":
				it.Status = agent.ItemCancelled
			case "permission.rejected":
				it.Status = agent.ItemDeclined
			}
		}
	}
	return it
}

// subagentToolItem returns the ID a live session gives the tool call that
// ran subagent session sub, as seen from session sessionID: the call's ID,
// prefixed with its session's ID when that is a subagent too. It returns
// "" when the call is not found.
func subagentToolItem(ctx context.Context, c *Client, sessionID, sub string) string {
	var native nativeSession
	if c.data(ctx, "GET", sessionPath(sub, ""), nil, &native) != nil || native.ParentID == "" {
		return ""
	}
	msgs, err := allMessages(ctx, c, native.ParentID)
	if err != nil {
		return ""
	}
	prefix := ""
	if native.ParentID != sessionID {
		prefix = native.ParentID + "/"
	}
	for _, raw := range msgs {
		var m historyMessage
		if json.Unmarshal(raw, &m) != nil || m.Type != "assistant" {
			continue
		}
		for _, part := range m.Content {
			var meta struct {
				SessionID string `json:"sessionID"`
			}
			if part.Type == "tool" && json.Unmarshal(part.State.Metadata, &meta) == nil && meta.SessionID == sub {
				return prefix + part.ID
			}
		}
	}
	return ""
}
