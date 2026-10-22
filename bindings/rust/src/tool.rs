use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use serde::de::DeserializeOwned;
use serde_json::Value;
use tokio::sync::Notify;

use crate::protocol::{Input, ToolContent, ToolDef, ToolResult};

pub(crate) type BoxFuture<T> = Pin<Box<dyn Future<Output = T> + Send>>;

/// A host tool: its definition, sent in `config.tools`, and the async handler
/// that runs each call as its own tokio task.
#[derive(Clone)]
pub struct Tool {
    /// Change it to set flags such as `needs_approval` or `read_only`.
    pub def: ToolDef,
    handler: Arc<Handler>,
}

type Handler = dyn Fn(Value, CancelToken) -> BoxFuture<ToolResult> + Send + Sync;

/// Tells a running tool call that its turn was interrupted or its session
/// closed. The library no longer waits for the result, so a long handler
/// should check [`is_cancelled`](Self::is_cancelled) or await
/// [`cancelled`](Self::cancelled) and return early.
#[derive(Clone, Default)]
pub struct CancelToken(Arc<CancelState>);

#[derive(Default)]
struct CancelState {
    cancelled: AtomicBool,
    notify: Notify,
}

impl CancelToken {
    pub fn is_cancelled(&self) -> bool {
        self.0.cancelled.load(Ordering::Acquire)
    }

    /// Completes once the call is cancelled.
    pub async fn cancelled(&self) {
        let notified = self.0.notify.notified();
        let mut notified = std::pin::pin!(notified);
        // Registered before the check, so a cancel in between still wakes it.
        notified.as_mut().enable();
        if self.is_cancelled() {
            return;
        }
        notified.await;
    }

    pub(crate) fn cancel(&self) {
        self.0.cancelled.store(true, Ordering::Release);
        self.0.notify.notify_waiters();
    }
}

impl Tool {
    /// A tool whose arguments decode into `A`. Missing arguments decode from
    /// `{}`. A decoding error, an `Err`, or a panic becomes an error result.
    /// The handler gets a [`CancelToken`] that fires when the turn is
    /// interrupted.
    pub fn new<A, F, Fut>(
        name: impl Into<String>,
        description: impl Into<String>,
        input_schema: Value,
        handler: F,
    ) -> Self
    where
        A: DeserializeOwned,
        F: Fn(A, CancelToken) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = Result<ToolResult, String>> + Send + 'static,
    {
        let def = ToolDef {
            name: name.into(),
            description: description.into(),
            input_schema: (!input_schema.is_null()).then_some(input_schema),
            ..Default::default()
        };
        let handler = move |args: Value, cancel: CancelToken| -> BoxFuture<ToolResult> {
            let args = if args.is_null() {
                Value::Object(Default::default())
            } else {
                args
            };
            match serde_json::from_value::<A>(args) {
                Ok(args) => {
                    let call = handler(args, cancel);
                    Box::pin(async move { call.await.unwrap_or_else(ToolResult::error) })
                }
                Err(e) => {
                    let result = ToolResult::error(format!("invalid arguments: {e}"));
                    Box::pin(async move { result })
                }
            }
        };
        Self {
            def,
            handler: Arc::new(handler),
        }
    }

    /// Runs one call as its own task, so a panic becomes an error result.
    pub(crate) async fn call(&self, args: Value, cancel: CancelToken) -> ToolResult {
        let handler = self.handler.clone();
        let task = tokio::spawn(async move { handler(args, cancel).await });
        match task.await {
            Ok(result) => result,
            Err(e) if e.is_panic() => ToolResult::error(format!("tool {} panicked", self.def.name)),
            Err(_) => ToolResult::error(format!("tool {} was cancelled", self.def.name)),
        }
    }
}

impl ToolResult {
    /// A result with one text block.
    pub fn text(text: impl Into<String>) -> Self {
        Self {
            content: vec![ToolContent {
                text: text.into(),
                ..Default::default()
            }],
            ..Default::default()
        }
    }

    /// An error result with one text block.
    pub fn error(message: impl Into<String>) -> Self {
        Self {
            is_error: true,
            ..Self::text(message)
        }
    }
}

impl From<&str> for Input {
    fn from(text: &str) -> Self {
        Self {
            text: text.into(),
            ..Default::default()
        }
    }
}

impl From<String> for Input {
    fn from(text: String) -> Self {
        Self {
            text,
            ..Default::default()
        }
    }
}
