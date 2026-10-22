use std::collections::{HashMap, HashSet};

use crate::protocol::{Event, EventPayload, Item, ItemKind, ItemStatus, ToolCall};

/// Folds events into the current list of transcript items, assembling
/// streamed text, reasoning, plan text, and tool output as it arrives. A chat
/// UI can render [`items`](Self::items) after each [`apply`](Self::apply)
/// instead of tracking deltas itself. Items nest through their `parent_id`
/// and `subagent_id` fields; the list itself is flat.
#[derive(Debug, Clone, Default)]
pub struct Transcript {
    items: Vec<Item>,
    index: HashMap<String, usize>,
}

/// What [`Transcript::apply`] changed.
#[derive(Debug, Clone, PartialEq)]
pub enum TranscriptChange<'a> {
    /// The item an event added or updated.
    Item(&'a Item),
    /// The IDs of the items an event removed, in transcript order.
    Removed(Vec<String>),
}

impl Transcript {
    pub fn new() -> Self {
        Self::default()
    }

    /// Updates the transcript with `event`; `None` when nothing changed.
    pub fn apply(&mut self, event: &Event) -> Option<TranscriptChange<'_>> {
        let i = match &event.payload {
            EventPayload::ItemStarted(e) => self.put(e.item.clone(), false),
            EventPayload::ItemUpdated(e) => self.put(e.item.clone(), true),
            EventPayload::ItemCompleted(e) => self.put(e.item.clone(), false),
            EventPayload::TextDelta(d) => self.append_text(
                event,
                ItemKind::AssistantMessage,
                &d.item_id,
                &d.parent_id,
                &d.subagent_id,
                &d.delta,
            ),
            EventPayload::ReasoningDelta(d) => self.append_text(
                event,
                ItemKind::Reasoning,
                &d.item_id,
                &d.parent_id,
                &d.subagent_id,
                &d.delta,
            ),
            EventPayload::PlanDelta(d) => self.append_text(
                event,
                ItemKind::Plan,
                &d.item_id,
                &d.parent_id,
                &d.subagent_id,
                &d.delta,
            ),
            EventPayload::ToolOutputDelta(d) => {
                let i = self.ensure(
                    &event.turn_id,
                    &d.item_id,
                    ItemKind::ToolCall,
                    &d.parent_id,
                    &d.subagent_id,
                );
                let tool = self.items[i].tool.get_or_insert_with(ToolCall::default);
                tool.output.push_str(&d.delta);
                i
            }
            EventPayload::ItemsRemoved(e) => return self.remove(&e.ids),
            EventPayload::ConversationReset(_) => {
                let ids: Vec<String> = self.items.drain(..).map(|it| it.id).collect();
                self.index.clear();
                return (!ids.is_empty()).then_some(TranscriptChange::Removed(ids));
            }
            _ => return None,
        };
        Some(TranscriptChange::Item(&self.items[i]))
    }

    /// Stores `item`. With `merge`, or while the item runs, text and tool
    /// output assembled from deltas are kept when the item carries none.
    fn put(&mut self, mut item: Item, merge: bool) -> usize {
        let Some(&i) = self.index.get(&item.id) else {
            self.index.insert(item.id.clone(), self.items.len());
            self.items.push(item);
            return self.items.len() - 1;
        };
        let old = &mut self.items[i];
        if merge || item.status == ItemStatus::InProgress {
            if item.text.is_empty() {
                item.text = std::mem::take(&mut old.text);
            }
            if let (Some(tool), Some(old_tool)) = (&mut item.tool, &mut old.tool)
                && tool.output.is_empty()
            {
                tool.output = std::mem::take(&mut old_tool.output);
            }
        }
        *old = item;
        i
    }

    fn append_text(
        &mut self,
        event: &Event,
        kind: ItemKind,
        id: &str,
        parent_id: &str,
        subagent_id: &str,
        delta: &str,
    ) -> usize {
        let i = self.ensure(&event.turn_id, id, kind, parent_id, subagent_id);
        self.items[i].text.push_str(delta);
        i
    }

    fn ensure(
        &mut self,
        turn_id: &str,
        id: &str,
        kind: ItemKind,
        parent_id: &str,
        subagent_id: &str,
    ) -> usize {
        if let Some(&i) = self.index.get(id) {
            return i;
        }
        self.index.insert(id.into(), self.items.len());
        self.items.push(Item {
            id: id.into(),
            kind,
            status: ItemStatus::InProgress,
            parent_id: parent_id.into(),
            subagent_id: subagent_id.into(),
            turn_id: turn_id.into(),
            ..Default::default()
        });
        self.items.len() - 1
    }

    fn remove(&mut self, ids: &[String]) -> Option<TranscriptChange<'_>> {
        let first = ids
            .iter()
            .filter_map(|id| self.index.get(id))
            .min()
            .copied()?;
        let doomed: HashSet<&str> = ids.iter().map(String::as_str).collect();
        let mut removed = Vec::new();
        let mut position = 0;
        self.items.retain_mut(|it| {
            let keep = position < first || !doomed.contains(it.id.as_str());
            position += 1;
            if !keep {
                self.index.remove(&it.id);
                removed.push(std::mem::take(&mut it.id));
            }
            keep
        });
        for (i, it) in self.items.iter().enumerate().skip(first) {
            if let Some(slot) = self.index.get_mut(&it.id) {
                *slot = i;
            }
        }
        Some(TranscriptChange::Removed(removed))
    }

    /// The items in order of first appearance.
    pub fn items(&self) -> &[Item] {
        &self.items
    }

    /// The item with ID `id`.
    pub fn item(&self, id: &str) -> Option<&Item> {
        self.index.get(id).map(|&i| &self.items[i])
    }

    /// The items grouped by `turn_id`, groups and items in order of first
    /// appearance.
    pub fn by_turn(&self) -> Vec<(&str, Vec<&Item>)> {
        let mut groups: Vec<(&str, Vec<&Item>)> = Vec::new();
        let mut group_of: HashMap<&str, usize> = HashMap::new();
        for item in &self.items {
            let g = *group_of.entry(&item.turn_id).or_insert_with(|| {
                groups.push((&item.turn_id, Vec::new()));
                groups.len() - 1
            });
            groups[g].1.push(item);
        }
        groups
    }

    /// Replaces the content with `items`, such as those
    /// [`Client::read_history`](crate::Client::read_history) returns. A later
    /// item replaces an earlier one with the same ID in place.
    pub fn load(&mut self, items: impl IntoIterator<Item = Item>) {
        self.reset();
        for item in items {
            self.put(item, false);
        }
    }

    /// Removes every item.
    pub fn reset(&mut self) {
        self.items.clear();
        self.index.clear();
    }
}
