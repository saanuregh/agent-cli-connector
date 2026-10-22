//! Sends one prompt to a harness, streams the reply, and prints what the turn
//! used. Tool calls that need approval are refused, and the session is
//! ephemeral.
//!
//!     cargo run --example basic -- claude "Summarize this repository in three bullets"
//!     cargo run --example basic -- fake "hello there"
//!     cargo run --example basic -- --agentd ../../zig-out/bin/agentd pi "What does build.zig build?"
use std::io::Write;
use std::process::ExitCode;

use agent_cli_connector::{
    Client, Config, EventPayload, Options, PermissionDecision, PermissionResponse, SessionOptions,
};
use serde_json::json;

#[tokio::main]
async fn main() -> ExitCode {
    let mut args: Vec<String> = std::env::args().skip(1).collect();
    let mut options = Options::default();
    if args.first().map(String::as_str) == Some("--agentd") && args.len() > 1 {
        options = options.agentd(args.remove(1));
        args.remove(0);
    }
    if args.len() < 2 {
        eprintln!("usage: basic [--agentd PATH] PROVIDER PROMPT...");
        return ExitCode::from(2);
    }
    let provider = args.remove(0);
    let prompt = args.join(" ");
    match run(options.provider(&provider, json!({})), &provider, &prompt).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("error: {e}");
            ExitCode::FAILURE
        }
    }
}

async fn run(options: Options, provider: &str, prompt: &str) -> agent_cli_connector::Result<()> {
    let client = Client::open(options).await?;
    let session = client
        .start(
            provider,
            SessionOptions::new(Config {
                ephemeral: true,
                ..Default::default()
            })
            .on_permission(|_| async {
                PermissionResponse {
                    decision: PermissionDecision::Deny,
                    message: "This run is read-only.".into(),
                    ..Default::default()
                }
            }),
        )
        .await?;
    let turn = session.send(prompt).await?;
    let mut out = std::io::stdout();
    let mut events = turn.events().await;
    while let Some(ev) = events.next().await {
        match ev.payload {
            // An empty parent_id: not a subagent's text.
            EventPayload::TextDelta(d) if d.parent_id.is_empty() => {
                let _ = out.write_all(d.delta.as_bytes());
                let _ = out.flush();
            }
            EventPayload::ItemStarted(s) => {
                if let Some(tool) = s.item.tool {
                    eprintln!("[{:?} {}]", tool.kind, tool.name);
                }
            }
            _ => {}
        }
    }
    let result = turn.wait().await?;
    let info = session.info().await?;
    eprintln!(
        "\n[{} {}: {} in, {} out tokens, {} ms]",
        info.provider,
        info.model,
        result.usage.input_tokens,
        result.usage.output_tokens,
        result.duration_ms
    );
    client.close().await;
    Ok(())
}
