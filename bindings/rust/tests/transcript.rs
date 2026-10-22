//! Folding events into transcript items.
use agent_cli_connector::{
    ConversationReset, Delta, Event, EventPayload, Item, ItemCompleted, ItemKind, ItemStarted,
    ItemStatus, ItemUpdated, ItemsRemoved, ReasoningDelta, ToolCall, Transcript, TranscriptChange,
};

fn ev(payload: EventPayload) -> Event {
    Event {
        payload,
        provider: String::new(),
        session_id: String::new(),
        turn_id: String::new(),
        time_ms: 0,
        raw: None,
    }
}

fn delta(item_id: &str, text: &str) -> Delta {
    Delta {
        item_id: item_id.into(),
        delta: text.into(),
        ..Default::default()
    }
}

fn item(id: &str, turn_id: &str) -> Item {
    Item {
        id: id.into(),
        turn_id: turn_id.into(),
        ..Default::default()
    }
}

#[test]
fn deltas_create_and_extend_items() {
    let mut t = Transcript::default();
    let sub = Delta {
        parent_id: "p".into(),
        subagent_id: "s".into(),
        ..delta("a", "Hel")
    };
    t.apply(&ev(EventPayload::TextDelta(sub)));
    let change = t.apply(&ev(EventPayload::TextDelta(delta("a", "lo"))));
    let Some(TranscriptChange::Item(a)) = change else {
        panic!("no item change")
    };
    assert_eq!(a.text, "Hello");
    assert_eq!(a.kind, ItemKind::AssistantMessage);
    assert_eq!(a.status, ItemStatus::InProgress);
    assert_eq!((a.parent_id.as_str(), a.subagent_id.as_str()), ("p", "s"));

    t.apply(&ev(EventPayload::ReasoningDelta(ReasoningDelta {
        item_id: "r".into(),
        delta: "think".into(),
        ..Default::default()
    })));
    t.apply(&ev(EventPayload::PlanDelta(delta("p1", "step"))));
    t.apply(&ev(EventPayload::ToolOutputDelta(delta("t", "out"))));
    t.apply(&ev(EventPayload::ToolOutputDelta(delta("t", "put"))));
    let kinds: Vec<_> = t.items().iter().map(|i| i.kind).collect();
    assert_eq!(
        kinds,
        [
            ItemKind::AssistantMessage,
            ItemKind::Reasoning,
            ItemKind::Plan,
            ItemKind::ToolCall
        ]
    );
    assert_eq!(t.item("t").unwrap().tool.as_ref().unwrap().output, "output");
    assert!(
        t.apply(&ev(EventPayload::EventsDropped(Default::default())))
            .is_none()
    );
}

#[test]
fn updates_keep_streamed_text_and_output() {
    let mut t = Transcript::default();
    t.apply(&ev(EventPayload::TextDelta(delta("a", "streamed"))));
    t.apply(&ev(EventPayload::ItemUpdated(ItemUpdated {
        item: Item {
            status: ItemStatus::Completed,
            ..item("a", "t1")
        },
    })));
    assert_eq!(t.item("a").unwrap().text, "streamed");
    assert_eq!(t.item("a").unwrap().turn_id, "t1");

    t.apply(&ev(EventPayload::ToolOutputDelta(delta("t", "out"))));
    t.apply(&ev(EventPayload::ItemStarted(ItemStarted {
        item: Item {
            status: ItemStatus::InProgress,
            tool: Some(ToolCall {
                name: "shell".into(),
                ..Default::default()
            }),
            ..item("t", "t1")
        },
    })));
    let tool = t.item("t").unwrap().tool.clone().unwrap();
    assert_eq!((tool.name.as_str(), tool.output.as_str()), ("shell", "out"));

    // A completed item replaces the streamed text.
    t.apply(&ev(EventPayload::ItemCompleted(ItemCompleted {
        item: Item {
            text: "final".into(),
            ..item("a", "t1")
        },
    })));
    assert_eq!(t.item("a").unwrap().text, "final");
    t.apply(&ev(EventPayload::TextDelta(delta("a", "!"))));
    assert_eq!(t.item("a").unwrap().text, "final!");
}

#[test]
fn removal_reset_load_and_by_turn() {
    let mut t = Transcript::default();
    for (id, turn) in [("a", "t1"), ("b", "t2"), ("c", "t1")] {
        t.apply(&ev(EventPayload::ItemStarted(ItemStarted {
            item: item(id, turn),
        })));
    }
    let groups: Vec<(&str, Vec<&str>)> = t
        .by_turn()
        .into_iter()
        .map(|(turn, items)| (turn, items.iter().map(|i| i.id.as_str()).collect()))
        .collect();
    assert_eq!(groups, [("t1", vec!["a", "c"]), ("t2", vec!["b"])]);

    let removed = t.apply(&ev(EventPayload::ItemsRemoved(ItemsRemoved {
        ids: vec!["a".into(), "zz".into()],
    })));
    assert_eq!(removed, Some(TranscriptChange::Removed(vec!["a".into()])));
    assert!(
        t.apply(&ev(EventPayload::ItemsRemoved(ItemsRemoved {
            ids: vec!["zz".into()],
        })))
        .is_none()
    );
    assert_eq!(t.item("c").unwrap().id, "c");

    let reset = t.apply(&ev(EventPayload::ConversationReset(
        ConversationReset::default(),
    )));
    assert_eq!(
        reset,
        Some(TranscriptChange::Removed(vec!["b".into(), "c".into()]))
    );
    assert!(t.items().is_empty());

    t.load(vec![item("x", "t9"), item("y", "t9")]);
    assert_eq!(t.items().len(), 2);
    t.apply(&ev(EventPayload::TextDelta(delta("x", "more"))));
    assert_eq!(t.item("x").unwrap().text, "more");
    t.reset();
    assert!(t.items().is_empty() && t.item("x").is_none());
}

#[test]
fn delta_items_keep_nesting_and_turn() {
    let mut t = Transcript::default();
    let nested = |id: &str| Delta {
        parent_id: "p".into(),
        subagent_id: "s".into(),
        ..delta(id, "x")
    };
    t.apply(&ev(EventPayload::PlanDelta(nested("plan"))));
    t.apply(&ev(EventPayload::ToolOutputDelta(nested("tool"))));
    for id in ["plan", "tool"] {
        let it = t.item(id).unwrap();
        assert_eq!((it.parent_id.as_str(), it.subagent_id.as_str()), ("p", "s"));
    }

    let mut t = Transcript::default();
    let mut streamed = ev(EventPayload::TextDelta(delta("a", "hi")));
    streamed.turn_id = "t1".into();
    t.apply(&streamed);
    let groups: Vec<(&str, usize)> = t
        .by_turn()
        .iter()
        .map(|(turn, items)| (*turn, items.len()))
        .collect();
    assert_eq!(groups, [("t1", 1)]);
}

#[test]
fn load_keeps_one_item_per_id() {
    let mut t = Transcript::default();
    let replaced = Item {
        text: "new".into(),
        ..item("a", "t1")
    };
    t.load(vec![item("a", "t1"), item("b", "t1"), replaced]);
    let ids: Vec<&str> = t.items().iter().map(|i| i.id.as_str()).collect();
    assert_eq!(ids, ["a", "b"]);
    assert_eq!(t.item("a").unwrap().text, "new");
    assert_eq!(t.item("b").unwrap().id, "b");
}
