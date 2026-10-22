//! One API over coding-agent CLIs (Claude Code, Codex, Pi).
//!
//! A [`Client`] is one connection to the agent-cli-connector library, linked
//! statically or run as an `agentd` subprocess ([`Options::agentd`]). It
//! starts [`Session`]s; each session streams [`Event`]s and runs turns.
//!
//! The API is async and runs on tokio: open the client inside a tokio
//! runtime, which also runs host tools and request handlers.
//!
//! ```no_run
//! use agent_cli_connector::{Client, Config, EventPayload, Options};
//! use serde_json::json;
//!
//! # async fn example() -> agent_cli_connector::Result<()> {
//! let client = Client::open(Options::default().provider("claude", json!({}))).await?;
//! let session = client
//!     .start("claude", Config { model: "haiku".into(), ..Default::default() })
//!     .await?;
//! let turn = session.send("Summarize this repository").await?;
//! let mut events = turn.events().await;
//! while let Some(ev) = events.next().await {
//!     if let EventPayload::TextDelta(d) = &ev.payload {
//!         print!("{}", d.delta);
//!     }
//! }
//! let result = turn.wait().await?;
//! println!("\n{} output tokens", result.usage.output_tokens);
//! # Ok(())
//! # }
//! ```
//!
//! Every type of the protocol is in [`protocol`] and re-exported here.

mod client;
mod error;
mod methods;
pub mod protocol;
mod session;
pub mod sys;
mod tool;
mod transcript;
mod transport;

pub use client::{Client, Options};
pub use error::{Error, Result};
pub use protocol::*;
pub use session::{
    Events, RequestResponse, Session, SessionEvent, SessionOptions, Subscription, Turn, TurnEvents,
};
pub use tool::{CancelToken, Tool};
pub use transcript::{Transcript, TranscriptChange};
pub use transport::library_version;
