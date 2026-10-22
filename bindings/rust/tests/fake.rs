//! Behavior tests against the library's scripted `fake` provider.
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use agent_cli_connector::{
    CancelToken, Client, Config, DialogResponse, DialogResponseBehavior, ErrorKind, EventPayload,
    NativeResponse, Options, PermissionDecision, PermissionResponse, SessionEvent, SessionOptions,
    Tool, ToolResult, TurnStatus,
};
use serde::Deserialize;
use serde_json::{Value, json};

fn fake() -> Options {
    Options::default().provider("fake", json!({}))
}

async fn text_of(mut events: agent_cli_connector::TurnEvents) -> String {
    let mut text = String::new();
    while let Some(ev) = events.next().await {
        if let EventPayload::TextDelta(d) = &ev.payload {
            text.push_str(&d.delta);
        }
    }
    text
}

#[tokio::test]
async fn run_streams_text_deltas() {
    let client = Client::open(fake()).await.unwrap();
    let session = client.start("fake", Config::default()).await.unwrap();
    let mut events = session.events().await;
    let turn = session.send("hello streaming world").await.unwrap();
    assert_eq!(text_of(turn.events().await).await, "hello streaming world");
    assert_eq!(turn.wait().await.unwrap().text, "hello streaming world");
    assert_eq!(session.run("again").await.unwrap().text, "again");

    // A finished turn's stream starts again at its first event.
    assert_eq!(text_of(turn.events().await).await, "hello streaming world");

    // Reading the turn took nothing from the session stream.
    session.close().await.unwrap();
    let (mut text, mut completed) = (String::new(), 0);
    while let Some(ev) = events.next().await {
        match &ev.payload {
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
    let mut events = session.events().await;
    let turn = session.send("needs permission").await.unwrap();
    let request = first_permission_request(&mut events).await;
    session.deny(&request.id, "not now").await.unwrap();
    assert_eq!(turn.wait().await.unwrap().text, "permission denied");
    assert_eq!(
        session.allow(&request.id).await.unwrap_err().kind(),
        ErrorKind::RequestDone
    );

    // By the helper on the request itself.
    let turn = session.send("needs permission").await.unwrap();
    let request = first_request(&mut events).await;
    request.allow().await.unwrap();
    assert_eq!(turn.wait().await.unwrap().text, "needs permission");
    assert_eq!(
        request.deny("too late").await.unwrap_err().kind(),
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
    events: &mut agent_cli_connector::Events,
) -> agent_cli_connector::PermissionRequest {
    while let Some(ev) = events.next().await {
        if let EventPayload::PermissionRequest(r) = ev.into_event().payload {
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
    // agentd comes from AGENTD_PATH or a repository build; the library itself
    // may have been downloaded, which brings no agentd.
    let agentd = std::env::var_os("AGENTD_PATH")
        .map(PathBuf::from)
        .unwrap_or_else(|| {
            PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../zig-out/bin/agentd")
        });
    if !agentd.exists() {
        eprintln!("skipped: no agentd at {}", agentd.display());
        return;
    }
    let client = Client::open(fake().agentd(agentd)).await.unwrap();
    assert_eq!(client.providers()[0].name, "fake");
    let session = client.start("fake", Config::default()).await.unwrap();
    let mut events = session.events().await;
    assert_eq!(
        session.run("over a pipe").await.unwrap().text,
        "over a pipe"
    );
    session.close().await.unwrap();
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

#[test]
fn crate_version_names_the_release_it_downloads() {
    // build.rs downloads the release for CARGO_PKG_VERSION.
    assert_eq!(
        env!("CARGO_PKG_VERSION"),
        agent_cli_connector::LIBRARY_VERSION
    );
}

#[tokio::test]
async fn dialog_and_native_helpers_answer_from_the_event() {
    let client = Client::open(fake()).await.unwrap();
    let session = client.start("fake", Config::default()).await.unwrap();
    let mut events = session.events().await;

    let turn = session.send("dialog").await.unwrap();
    let ev = first_request(&mut events).await;
    assert!(!ev.handled());
    ev.complete(Some(json!({"ok": 1}))).await.unwrap();
    assert_eq!(
        turn.wait().await.unwrap().text,
        r#"dialog completed {"ok":1}"#
    );

    let turn = session.send("dialog").await.unwrap();
    first_request(&mut events).await.cancel().await.unwrap();
    assert_eq!(turn.wait().await.unwrap().text, "dialog cancelled");

    let turn = session.send("native").await.unwrap();
    let ev = first_request(&mut events).await;
    assert!(matches!(&ev.payload, EventPayload::NativeRequest(r) if r.method == "fake/ping"));
    ev.reply(Some(json!("pong"))).await.unwrap();
    assert_eq!(turn.wait().await.unwrap().text, r#"native result "pong""#);

    let turn = session.send("native").await.unwrap();
    first_request(&mut events)
        .await
        .fail(7, "nope")
        .await
        .unwrap();
    assert_eq!(turn.wait().await.unwrap().text, "native error 7 nope");

    // A helper for another kind of request fails without sending.
    let turn = session.send("native").await.unwrap();
    let ev = first_request(&mut events).await;
    assert_eq!(
        ev.allow().await.unwrap_err().kind(),
        ErrorKind::InvalidParams
    );
    ev.reply(None).await.unwrap();
    assert_eq!(turn.wait().await.unwrap().text, "native result null");
}

#[tokio::test]
async fn handled_requests_are_delivered_and_marked() {
    let client = Client::open(fake()).await.unwrap();
    let options = SessionOptions::new(Config::default())
        .on_dialog(|_| async {
            DialogResponse {
                behavior: DialogResponseBehavior::Completed,
                result: Some(json!(2)),
            }
        })
        .on_native_request(|r| async move {
            NativeResponse {
                result: Some(json!(r.method)),
                ..Default::default()
            }
        });
    let session = client.start("fake", options).await.unwrap();
    let mut events = session.events().await;
    let turn = session.send("dialog").await.unwrap();
    assert_eq!(turn.wait().await.unwrap().text, "dialog completed 2");
    let mut turn_events = turn.events().await;
    let mut in_turn = false;
    while let Some(ev) = turn_events.next().await {
        in_turn |= ev.handled() && matches!(ev.payload, EventPayload::DialogRequest(_));
    }
    assert!(in_turn);
    assert_eq!(
        session.run("native").await.unwrap().text,
        r#"native result "fake/ping""#
    );
    session.close().await.unwrap();
    let mut handled = Vec::new();
    while let Some(ev) = events.next().await {
        if matches!(
            ev.payload,
            EventPayload::DialogRequest(_) | EventPayload::NativeRequest(_)
        ) {
            handled.push(ev.handled());
        }
    }
    assert_eq!(handled, [true, true]);
}

#[tokio::test]
async fn failing_callbacks_answer_fail_closed() {
    let client = Client::open(fake()).await.unwrap();
    let options = SessionOptions::new(Config::default())
        .on_permission(|_| async { panic!("permission broke") })
        .on_dialog(|_| async { panic!("dialog broke") })
        .on_native_request(|_| async { panic!("native broke") });
    let session = client.start("fake", options).await.unwrap();
    assert_eq!(
        session.run("dialog").await.unwrap().text,
        "dialog cancelled"
    );
    assert_eq!(
        session.run("native").await.unwrap().text,
        "native error -32603 native broke"
    );
    assert_eq!(
        session.run("needs permission").await.unwrap().text,
        "permission denied"
    );
}

#[tokio::test]
async fn subscribers_see_every_event_and_events_buffers_lazily() {
    let client = Client::open(fake()).await.unwrap();
    let session = client.start("fake", Config::default()).await.unwrap();
    let seen = Arc::new(Mutex::new(String::new()));
    let sink = seen.clone();
    let subscription = session.subscribe(move |ev| {
        if let EventPayload::TextDelta(d) = &ev.payload {
            sink.lock().unwrap().push_str(&d.delta);
        }
    });
    let _panicking = session.subscribe(|_| panic!("listener broke"));
    session.run("before").await.unwrap();
    let mut events = session.events().await;
    session.run("after").await.unwrap();
    subscription.unsubscribe();
    session.run("ignored").await.unwrap();
    session.close().await.unwrap();

    assert_eq!(*seen.lock().unwrap(), "beforeafter");
    let mut text = String::new();
    while let Some(ev) = events.next().await {
        if let EventPayload::TextDelta(d) = &ev.payload {
            text.push_str(&d.delta);
        }
    }
    assert_eq!(text, "afterignored");
}

async fn first_request(events: &mut agent_cli_connector::Events) -> SessionEvent {
    while let Some(ev) = events.next().await {
        if matches!(
            ev.payload,
            EventPayload::PermissionRequest(_)
                | EventPayload::DialogRequest(_)
                | EventPayload::NativeRequest(_)
        ) {
            return ev;
        }
    }
    panic!("the session closed before a request");
}

#[tokio::test]
async fn the_session_stream_starts_with_session_started() {
    let client = Client::open(fake()).await.unwrap();
    let session = client.start("fake", Config::default()).await.unwrap();
    session.run("before the stream").await.unwrap();
    let mut events = session.events().await;
    let first = events.next().await.unwrap();
    assert!(matches!(first.payload, EventPayload::SessionStarted(_)));
    session.close().await.unwrap();
    while let Some(ev) = events.next().await {
        assert!(!matches!(ev.payload, EventPayload::TextDelta(_)));
    }
}
