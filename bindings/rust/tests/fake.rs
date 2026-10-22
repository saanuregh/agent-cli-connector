//! Behavior tests against the library's scripted `fake` provider.
use std::path::PathBuf;

use agent_cli_connector::{
    CancelToken, Client, Config, ErrorKind, EventPayload, Options, PermissionDecision,
    PermissionResponse, SessionOptions, Tool, ToolResult, TurnStatus,
};
use serde::Deserialize;
use serde_json::{Value, json};

fn fake() -> Options {
    Options::default().provider("fake", json!({}))
}

async fn text_of(mut events: agent_cli_connector::TurnEvents) -> String {
    let mut text = String::new();
    while let Some(ev) = events.next().await {
        if let EventPayload::TextDelta(d) = ev.payload {
            text.push_str(&d.delta);
        }
    }
    text
}

#[tokio::test]
async fn run_streams_text_deltas() {
    let client = Client::open(fake()).await.unwrap();
    let session = client.start("fake", Config::default()).await.unwrap();
    let turn = session.send("hello streaming world").await.unwrap();
    assert_eq!(text_of(turn.events().await).await, "hello streaming world");
    assert_eq!(turn.wait().await.unwrap().text, "hello streaming world");
    assert_eq!(session.run("again").await.unwrap().text, "again");

    // A finished turn's stream starts again at its first event.
    assert_eq!(text_of(turn.events().await).await, "hello streaming world");

    // Reading the turn took nothing from the session stream.
    session.close().await.unwrap();
    let mut events = session.events().await;
    let (mut text, mut completed) = (String::new(), 0);
    while let Some(ev) = events.next().await {
        match ev.payload {
            EventPayload::TextDelta(d) => text.push_str(&d.delta),
            EventPayload::TurnCompleted(_) => completed += 1,
            _ => {}
        }
    }
    assert_eq!(text, "hello streaming worldagain");
    assert_eq!(completed, 2);
}

#[tokio::test]
async fn permission_requests_are_answered() {
    let client = Client::open(fake()).await.unwrap();

    // By hand, from the event stream.
    let session = client.start("fake", Config::default()).await.unwrap();
    let turn = session.send("needs permission").await.unwrap();
    let request = first_permission_request(&session).await;
    session.deny(&request.id, "not now").await.unwrap();
    assert_eq!(turn.wait().await.unwrap().text, "permission denied");
    assert_eq!(
        session.allow(&request.id).await.unwrap_err().kind(),
        ErrorKind::RequestDone
    );

    // By the helper on the request itself.
    let turn = session.send("needs permission").await.unwrap();
    let request = first_permission_request(&session).await;
    request.allow(&session).await.unwrap();
    assert_eq!(turn.wait().await.unwrap().text, "needs permission");
    assert_eq!(
        request.deny(&session, "too late").await.unwrap_err().kind(),
        ErrorKind::RequestDone
    );

    // By a handler set at start.
    let options = SessionOptions::new(Config::default()).on_permission(|_| async {
        PermissionResponse {
            decision: PermissionDecision::Allow,
            ..Default::default()
        }
    });
    let session = client.start("fake", options).await.unwrap();
    assert_eq!(
        session.run("needs permission").await.unwrap().text,
        "needs permission"
    );
}

async fn first_permission_request(
    session: &agent_cli_connector::Session,
) -> agent_cli_connector::PermissionRequest {
    let mut events = session.events().await;
    while let Some(ev) = events.next().await {
        if let EventPayload::PermissionRequest(r) = ev.payload {
            return r;
        }
    }
    panic!("the session closed before a permission request");
}

#[derive(Deserialize)]
struct LookupArgs {
    #[serde(default)]
    ticket: Option<u32>,
}

#[tokio::test]
async fn host_tool_answers_tool_calls() {
    let client = Client::open(fake()).await.unwrap();
    let lookup = Tool::new(
        "lookup",
        "Looks up a ticket",
        json!({"type": "object"}),
        |args: LookupArgs, _| async move {
            Ok(ToolResult::text(format!(
                "ticket {}",
                args.ticket.unwrap_or(42)
            )))
        },
    );
    let session = client
        .start("fake", SessionOptions::new(Config::default()).tool(lookup))
        .await
        .unwrap();
    assert_eq!(session.run("tool:lookup").await.unwrap().text, "ticket 42");

    let failing = Tool::new("broken", "", json!(null), |_: Value, _| async {
        Err("no database".to_string())
    });
    let session = client
        .start("fake", SessionOptions::new(Config::default()).tool(failing))
        .await
        .unwrap();
    assert_eq!(
        session.run("tool:broken").await.unwrap().text,
        "no database"
    );
}

#[tokio::test]
async fn agentd_subprocess_runs_a_turn() {
    let agentd = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../zig-out/bin/agentd");
    let client = Client::open(fake().agentd(agentd)).await.unwrap();
    assert_eq!(client.providers()[0].name, "fake");
    let session = client.start("fake", Config::default()).await.unwrap();
    assert_eq!(
        session.run("over a pipe").await.unwrap().text,
        "over a pipe"
    );
    session.close().await.unwrap();
    let mut events = session.events().await;
    let mut completed = false;
    while let Some(ev) = events.next().await {
        completed |= matches!(ev.payload, EventPayload::TurnCompleted(_));
    }
    assert!(completed);
    client.close().await;
    assert_eq!(session.info().await.unwrap_err().kind(), ErrorKind::Closed);
}

#[test]
fn unknown_event_types_keep_their_json() {
    let ev: agent_cli_connector::Event =
        serde_json::from_value(json!({"type": "from_the_future", "x": 1, "turn_id": "t1"}))
            .unwrap();
    assert_eq!(ev.turn_id, "t1");
    assert_eq!(
        ev.payload,
        EventPayload::Unknown(json!({"type": "from_the_future", "x": 1}))
    );
}

#[tokio::test]
async fn interrupting_cancels_a_running_tool_call() {
    let (started_tx, mut started_rx) = tokio::sync::mpsc::unbounded_channel();
    let slow = Tool::new(
        "slow",
        "Waits until cancelled.",
        json!({"type": "object"}),
        move |_: Value, cancel: CancelToken| {
            let started_tx = started_tx.clone();
            async move {
                started_tx.send(()).unwrap();
                cancel.cancelled().await;
                assert!(cancel.is_cancelled());
                Ok(ToolResult::text("stopped"))
            }
        },
    );
    let client = Client::open(fake()).await.unwrap();
    let session = client
        .start("fake", SessionOptions::new(Config::default()).tool(slow))
        .await
        .unwrap();
    let turn = session.send("tool:slow").await.unwrap();
    started_rx.recv().await.unwrap();
    session.interrupt().await.unwrap();
    let result = turn.wait().await.unwrap();
    assert_eq!(result.status, TurnStatus::Interrupted);
}

/// The runtime is blocked while the reader handles `tool/call` and then
/// `tool/cancel`, so the cancel arrives before the tool's task runs.
#[tokio::test]
async fn a_cancel_before_the_tool_task_runs_is_kept() {
    let (returned_tx, mut returned_rx) = tokio::sync::mpsc::unbounded_channel();
    let slow = Tool::new(
        "slow",
        "Waits until cancelled.",
        json!({"type": "object"}),
        move |_: Value, cancel: CancelToken| {
            let returned_tx = returned_tx.clone();
            async move {
                cancel.cancelled().await;
                returned_tx.send(()).unwrap();
                Ok(ToolResult::text("stopped"))
            }
        },
    );
    let client = Client::open(fake()).await.unwrap();
    let session = client
        .start("fake", SessionOptions::new(Config::default()).tool(slow))
        .await
        .unwrap();
    let turn = session.send("tool:slow").await.unwrap();
    let interrupter = session.clone();
    std::thread::spawn(move || {
        let pause = std::time::Duration::from_millis(200);
        std::thread::sleep(pause);
        tokio::runtime::Builder::new_current_thread()
            .build()
            .unwrap()
            .block_on(interrupter.interrupt())
            .unwrap();
        std::thread::sleep(pause);
    })
    .join()
    .unwrap();
    tokio::time::timeout(std::time::Duration::from_secs(5), returned_rx.recv())
        .await
        .expect("the tool call never saw its cancel")
        .unwrap();
    assert_eq!(turn.wait().await.unwrap().status, TurnStatus::Interrupted);
}

#[tokio::test(flavor = "multi_thread")]
async fn futures_are_send_and_spawnable() {
    let client = Client::open(fake()).await.unwrap();
    let task = tokio::spawn(async move {
        let session = client.start("fake", Config::default()).await?;
        let turn = session.send("from a task").await?;
        let mut events = turn.events().await;
        while events.next().await.is_some() {}
        let text = turn.wait().await?.text;
        let _: Value = client
            .request(
                agent_cli_connector::method::SESSION_INFO,
                json!({"session": session.handle()}),
            )
            .await?;
        Ok::<_, agent_cli_connector::Error>(text)
    });
    assert_eq!(task.await.unwrap().unwrap(), "from a task");
}
