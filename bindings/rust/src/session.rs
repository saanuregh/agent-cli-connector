use std::collections::HashMap;
use std::future::Future;
use std::panic::AssertUnwindSafe;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, OnceLock, Weak};
use std::task::{Context, Poll, Waker};
use std::time::Duration;

use futures_core::Stream;
use serde::Serialize;
use serde::de::DeserializeOwned;
use serde_json::Value;
use tokio::sync::mpsc::{self, UnboundedReceiver, UnboundedSender};
use tokio::sync::{Mutex as AsyncMutex, OwnedMutexGuard};

use crate::client::{ClientInner, Shared};
use crate::error::{Error, Result};
use crate::protocol::*;
use crate::tool::{BoxFuture, CancelToken, Tool};

type Handler<Req, Resp> = Arc<dyn Fn(Req) -> BoxFuture<Resp> + Send + Sync>;

fn boxed_handler<Req, Resp, F, Fut>(handler: F) -> Handler<Req, Resp>
where
    F: Fn(Req) -> Fut + Send + Sync + 'static,
    Fut: Future<Output = Resp> + Send + 'static,
{
    Arc::new(move |request| Box::pin(handler(request)))
}

/// A session's configuration, host tools, and request handlers. A plain
/// [`Config`] converts into it.
#[derive(Clone, Default)]
pub struct SessionOptions {
    pub config: Config,
    hooks: Hooks,
}

impl SessionOptions {
    pub fn new(config: Config) -> Self {
        Self {
            config,
            ..Default::default()
        }
    }

    /// Adds a host tool the model can call; its definition goes in
    /// `config.tools`.
    pub fn tool(mut self, tool: Tool) -> Self {
        self.hooks.tools.push(tool);
        self
    }

    /// Answers every permission request with `handler`, as its own tokio
    /// task. The request still appears in the event streams, marked
    /// [`handled`](SessionEvent::handled). If `handler` panics, the request
    /// is denied with the panic message.
    pub fn on_permission<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(PermissionRequest) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = PermissionResponse> + Send + 'static,
    {
        self.hooks.on_permission = Some(boxed_handler(handler));
        self
    }

    /// Answers every user input request with `handler`, as its own tokio
    /// task; a panic cancels the request.
    pub fn on_user_input<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(UserInputRequest) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = UserInputResponse> + Send + 'static,
    {
        self.hooks.on_user_input = Some(boxed_handler(handler));
        self
    }

    /// Answers every MCP elicitation with `handler`, as its own tokio task;
    /// a panic cancels the elicitation.
    pub fn on_elicitation<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(ElicitationRequest) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = ElicitationResponse> + Send + 'static,
    {
        self.hooks.on_elicitation = Some(boxed_handler(handler));
        self
    }

    /// Answers every dialog request with `handler`, as its own tokio task;
    /// a panic cancels the dialog.
    pub fn on_dialog<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(DialogRequest) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = DialogResponse> + Send + 'static,
    {
        self.hooks.on_dialog = Some(boxed_handler(handler));
        self
    }

    /// Answers every native request with `handler`, as its own tokio task;
    /// a panic answers with error code -32603 and the panic message.
    pub fn on_native_request<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(NativeRequest) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = NativeResponse> + Send + 'static,
    {
        self.hooks.on_native_request = Some(boxed_handler(handler));
        self
    }

    /// The config to send, with the tool definitions, and the client-side
    /// hooks.
    pub(crate) fn split(self) -> (Config, Hooks) {
        let mut config = self.config;
        config
            .tools
            .extend(self.hooks.tools.iter().map(|t| t.def.clone()));
        (config, self.hooks)
    }
}

impl From<Config> for SessionOptions {
    fn from(config: Config) -> Self {
        Self::new(config)
    }
}

#[derive(Clone, Default)]
pub(crate) struct Hooks {
    tools: Vec<Tool>,
    on_permission: Option<Handler<PermissionRequest, PermissionResponse>>,
    on_user_input: Option<Handler<UserInputRequest, UserInputResponse>>,
    on_elicitation: Option<Handler<ElicitationRequest, ElicitationResponse>>,
    on_dialog: Option<Handler<DialogRequest, DialogResponse>>,
    on_native_request: Option<Handler<NativeRequest, NativeResponse>>,
}

/// An event as kept for the streams: shared, with whether a handler answers it.
#[derive(Clone)]
struct Delivered {
    event: Arc<Event>,
    handled: bool,
}

/// The session stream's queue, made by the first [`Session::events`] call.
#[derive(Default)]
struct EventQueue {
    tx: Option<UnboundedSender<Delivered>>,
    /// Held by the one [`Events`] reading the session.
    rx: Option<Arc<AsyncMutex<UnboundedReceiver<Delivered>>>>,
    /// The `session_started` event, kept until the stream is made so that
    /// the stream starts with it.
    started: Option<Delivered>,
}

type Listener = Arc<dyn Fn(&SessionEvent) + Send + Sync>;

#[derive(Clone)]
struct Subscriber {
    id: u64,
    listener: Listener,
}

/// The reader's view of a session: its event queue, turn logs, and hooks.
pub(crate) struct SessionState {
    events: Mutex<EventQueue>,
    /// Replaced whole on subscribe and unsubscribe, so delivery clones one
    /// `Arc` and runs the listeners without holding the lock.
    subscribers: Mutex<Arc<[Subscriber]>>,
    /// The session the events belong to, set by the first subscribe.
    session: OnceLock<Weak<SessionInner>>,
    next_subscriber: AtomicU64,
    closed: AtomicBool,
    close_error: Mutex<Option<ErrorInfo>>,
    hooks: Hooks,
    /// The logs of the turns `send` started, by ID, from before the request
    /// until the turn ends.
    turns: Mutex<HashMap<String, Arc<TurnLog>>>,
    /// Running tool calls by ID, cancelled by `tool/cancel`.
    tool_calls: Mutex<HashMap<String, CancelToken>>,
}

impl SessionState {
    pub(crate) fn new(hooks: Hooks) -> Self {
        Self {
            events: Mutex::default(),
            subscribers: Mutex::new(Arc::from([])),
            session: OnceLock::new(),
            next_subscriber: AtomicU64::new(0),
            closed: AtomicBool::new(false),
            close_error: Mutex::new(None),
            hooks,
            turns: Mutex::new(HashMap::new()),
            tool_calls: Mutex::new(HashMap::new()),
        }
    }

    /// Starts the matching request handler, if any, passes an event to the
    /// subscribers, and queues it for its turn's log and the session stream.
    /// Runs on the reader, so it never blocks, unless a subscriber does.
    pub(crate) fn deliver(&self, shared: &Arc<Shared>, handle: &str, event: Event) {
        let event = Arc::new(event);
        let handled = start_handler(&self.hooks, shared, handle, &event);
        let delivered = Delivered { event, handled };
        self.notify_subscribers(&delivered);
        if !delivered.event.turn_id.is_empty() {
            self.log_turn_event(&delivered);
        }
        let mut queue = self.events.lock().unwrap();
        if let Some(tx) = &queue.tx {
            let _ = tx.send(delivered);
        } else if queue.rx.is_none()
            && matches!(delivered.event.payload, EventPayload::SessionStarted(_))
        {
            queue.started = Some(delivered);
        }
    }

    fn notify_subscribers(&self, delivered: &Delivered) {
        let subscribers = self.subscribers.lock().unwrap().clone();
        let Some(session) = self.session.get() else {
            return;
        };
        for subscriber in subscribers.iter() {
            let event = SessionEvent::new(delivered.clone(), session);
            // A panicking listener does not stop delivery to the others.
            let _ = std::panic::catch_unwind(AssertUnwindSafe(|| (subscriber.listener)(&event)));
        }
    }

    fn subscribe(&self, listener: Listener, session: Weak<SessionInner>) -> u64 {
        let id = self.next_subscriber.fetch_add(1, Ordering::Relaxed);
        self.session.get_or_init(|| session);
        let mut subscribers = self.subscribers.lock().unwrap();
        *subscribers = subscribers
            .iter()
            .cloned()
            .chain([Subscriber { id, listener }])
            .collect();
        id
    }

    /// The session stream's receiver, made on the first call. It starts with
    /// the `session_started` event; other events delivered before it are not
    /// queued.
    fn event_reader(&self) -> Arc<AsyncMutex<UnboundedReceiver<Delivered>>> {
        let mut queue = self.events.lock().unwrap();
        if let Some(rx) = &queue.rx {
            return rx.clone();
        }
        let (tx, rx) = mpsc::unbounded_channel();
        if let Some(started) = queue.started.take() {
            let _ = tx.send(started);
        }
        // `mark_closed` sets `closed` before it drops the sender under this lock.
        if !self.closed.load(Ordering::Acquire) {
            queue.tx = Some(tx);
        }
        let rx = Arc::new(AsyncMutex::new(rx));
        queue.rx = Some(rx.clone());
        rx
    }

    fn log_turn_event(&self, delivered: &Delivered) {
        let event = &delivered.event;
        let mut turns = self.turns.lock().unwrap();
        let Some(log) = turns.get(&event.turn_id) else {
            return;
        };
        log.add(delivered.clone());
        let ends_turn = matches!(&event.payload, EventPayload::TurnCompleted(c)
            if c.result.turn_id.is_empty() || c.result.turn_id == event.turn_id);
        if ends_turn {
            log.end();
            turns.remove(&event.turn_id);
        }
    }

    /// Adds the log of a turn about to be sent.
    fn add_turn(&self, turn_id: &str) -> Arc<TurnLog> {
        let log = Arc::new(TurnLog::default());
        let mut turns = self.turns.lock().unwrap();
        // `mark_closed` sets `closed` before it ends the logs under this lock.
        if self.closed.load(Ordering::Acquire) {
            log.end();
        } else {
            turns.insert(turn_id.into(), log.clone());
        }
        log
    }

    fn remove_turn(&self, turn_id: &str) {
        self.turns.lock().unwrap().remove(turn_id);
    }

    pub(crate) fn mark_closed(&self, error: Option<ErrorInfo>) {
        *self.close_error.lock().unwrap() = error;
        self.closed.store(true, Ordering::Release);
        self.events.lock().unwrap().tx.take();
        for log in self.turns.lock().unwrap().values() {
            log.end();
        }
    }

    /// Registers a tool call before its task starts, so a `tool/cancel`
    /// that the reader handles first still reaches it.
    pub(crate) fn register_tool_call(&self, call_id: &str) -> CancelToken {
        let cancel = CancelToken::default();
        self.tool_calls
            .lock()
            .unwrap()
            .insert(call_id.into(), cancel.clone());
        cancel
    }

    pub(crate) async fn run_tool(&self, call: ToolCallRequest, cancel: CancelToken) -> ToolResult {
        // The last tool of a name wins, as when the tools were a map.
        let tool = self
            .hooks
            .tools
            .iter()
            .rev()
            .find(|t| t.def.name == call.name)
            .cloned();
        let result = match tool {
            Some(tool) => {
                tool.call(call.arguments.unwrap_or(Value::Null), cancel)
                    .await
            }
            None => ToolResult::error(format!("unknown tool {}", call.name)),
        };
        self.tool_calls.lock().unwrap().remove(&call.id);
        result
    }

    pub(crate) fn cancel_tool(&self, call_id: &str) {
        if let Some(cancel) = self.tool_calls.lock().unwrap().get(call_id) {
            cancel.cancel();
        }
    }
}

/// Runs its function when dropped, unless disarmed: it undoes a
/// registration when the request that needed it fails or is dropped.
struct Undo<F: FnOnce()>(Option<F>);

impl<F: FnOnce()> Undo<F> {
    fn disarm(mut self) {
        self.0 = None;
    }
}

impl<F: FnOnce()> Drop for Undo<F> {
    fn drop(&mut self) {
        if let Some(undo) = self.0.take() {
            undo();
        }
    }
}

/// Starts the handler for a request event as a task, if the hooks have one,
/// and reports whether it did.
fn start_handler(hooks: &Hooks, shared: &Arc<Shared>, handle: &str, event: &Event) -> bool {
    let answer = match &event.payload {
        EventPayload::PermissionRequest(r) => {
            auto_answer(&hooks.on_permission, r, &r.id, handle, |message| {
                PermissionResponse {
                    decision: PermissionDecision::Deny,
                    message,
                    ..Default::default()
                }
            })
        }
        EventPayload::UserInputRequest(r) => {
            auto_answer(&hooks.on_user_input, r, &r.id, handle, |_| {
                UserInputResponse {
                    cancelled: true,
                    ..Default::default()
                }
            })
        }
        EventPayload::ElicitationRequest(r) => {
            auto_answer(&hooks.on_elicitation, r, &r.id, handle, |_| {
                ElicitationResponse {
                    action: ElicitationResponseAction::Cancel,
                    ..Default::default()
                }
            })
        }
        EventPayload::DialogRequest(r) => {
            auto_answer(&hooks.on_dialog, r, &r.id, handle, |_| DialogResponse {
                behavior: DialogResponseBehavior::Cancelled,
                result: None,
            })
        }
        EventPayload::NativeRequest(r) => {
            auto_answer(&hooks.on_native_request, r, &r.id, handle, |message| {
                NativeResponse {
                    result: None,
                    error: Some(NativeResponseError {
                        code: -32603,
                        message,
                    }),
                }
            })
        }
        _ => None,
    };
    let Some(answer) = answer else {
        return false;
    };
    let task_shared = shared.clone();
    shared.runtime.spawn(async move {
        let params = answer.await;
        let _: Result<()> = task_shared.request(method::REQUEST_RESPOND, params).await;
    });
    true
}

/// The events of one turn, kept for every [`TurnEvents`] that reads it.
#[derive(Default)]
pub(crate) struct TurnLog {
    state: Mutex<TurnLogState>,
}

#[derive(Default)]
struct TurnLogState {
    events: Vec<Delivered>,
    ended: bool,
    /// The readers waiting for the next event.
    wakers: Vec<Waker>,
}

impl TurnLog {
    fn add(&self, event: Delivered) {
        let mut state = self.state.lock().unwrap();
        if state.ended {
            return;
        }
        state.events.push(event);
        wake_readers(state);
    }

    /// Marks the log complete; later events are not added.
    fn end(&self) {
        let mut state = self.state.lock().unwrap();
        if state.ended {
            return;
        }
        state.ended = true;
        wake_readers(state);
    }

    fn is_ended(&self) -> bool {
        self.state.lock().unwrap().ended
    }

    fn poll_event(&self, index: usize, cx: &mut Context<'_>) -> Poll<Option<Delivered>> {
        let mut state = self.state.lock().unwrap();
        if let Some(event) = state.events.get(index) {
            return Poll::Ready(Some(event.clone()));
        }
        if state.ended {
            return Poll::Ready(None);
        }
        if !state.wakers.iter().any(|w| w.will_wake(cx.waker())) {
            state.wakers.push(cx.waker().clone());
        }
        Poll::Pending
    }
}

/// Wakes the readers waiting for the log, after releasing its lock.
fn wake_readers(mut state: MutexGuard<'_, TurnLogState>) {
    let wakers = std::mem::take(&mut state.wakers);
    drop(state);
    wakers.into_iter().for_each(Waker::wake);
}

/// The answer `handler` gives `request`, computed later as a task. If the
/// handler panics, the answer is `on_panic` of the panic message.
fn auto_answer<Req, Resp>(
    handler: &Option<Handler<Req, Resp>>,
    request: &Req,
    request_id: &str,
    handle: &str,
    on_panic: fn(String) -> Resp,
) -> Option<BoxFuture<RespondParams>>
where
    Req: Clone + Send + 'static,
    Resp: RequestResponse + Send + 'static,
{
    let handler = handler.clone()?;
    let (request, request_id, handle) = (request.clone(), request_id.to_owned(), handle.to_owned());
    // The handler is called inside its own task, so its synchronous part
    // never runs on the reader and a panic is caught by the join.
    Some(Box::pin(async move {
        let response = match tokio::spawn(async move { handler(request).await }).await {
            Ok(response) => response,
            Err(e) if e.is_panic() => on_panic(panic_message(e.into_panic())),
            Err(e) => on_panic(e.to_string()),
        };
        respond_params(handle, request_id, response)
    }))
}

fn panic_message(payload: Box<dyn std::any::Any + Send>) -> String {
    match payload.downcast::<String>() {
        Ok(message) => *message,
        Err(payload) => payload
            .downcast_ref::<&str>()
            .map_or_else(|| "the handler panicked".into(), |m| (*m).into()),
    }
}

fn respond_params(
    session: String,
    request_id: String,
    response: impl RequestResponse,
) -> RespondParams {
    let mut params = RespondParams {
        session,
        request_id,
        ..Default::default()
    };
    response.set(&mut params);
    params
}

/// An answer to a permission, user input, elicitation, dialog, or native
/// request.
pub trait RequestResponse {
    fn set(self, params: &mut RespondParams);
}

impl RequestResponse for PermissionResponse {
    fn set(self, params: &mut RespondParams) {
        params.permission = Some(self);
    }
}

impl RequestResponse for UserInputResponse {
    fn set(self, params: &mut RespondParams) {
        params.user_input = Some(self);
    }
}

impl RequestResponse for ElicitationResponse {
    fn set(self, params: &mut RespondParams) {
        params.elicitation = Some(self);
    }
}

impl RequestResponse for DialogResponse {
    fn set(self, params: &mut RespondParams) {
        params.dialog = Some(self);
    }
}

impl RequestResponse for NativeResponse {
    fn set(self, params: &mut RespondParams) {
        params.native = Some(self);
    }
}

// ---------------------------------------------------------------- session

/// A running session. Cloning is cheap and shares it; it closes when the
/// last clone is dropped, or on [`Session::close`].
#[derive(Clone)]
pub struct Session {
    inner: Arc<SessionInner>,
}

struct SessionInner {
    client: Arc<ClientInner>,
    state: Arc<SessionState>,
    handle: String,
    capabilities: Capabilities,
}

impl Drop for SessionInner {
    fn drop(&mut self) {
        // Sent without waiting for the response, so dropping never blocks.
        if !self.state.closed.load(Ordering::Acquire) {
            self.client
                .shared
                .send_without_reply(method::SESSION_CLOSE, self.params());
        }
    }
}

impl SessionInner {
    fn params(&self) -> SessionParams {
        SessionParams {
            session: self.handle.clone(),
        }
    }
}

impl Session {
    /// Starts a session with `session/start` or `session/fork`, whose
    /// `params` name `handle`. The session's state exists before the request,
    /// so every event finds it.
    pub(crate) async fn start(
        client: &Arc<ClientInner>,
        method: &str,
        handle: String,
        params: impl Serialize,
        hooks: Hooks,
    ) -> Result<Session> {
        let shared = &client.shared;
        let state = shared.add_session(&handle, hooks);
        // If the caller gives up after the request was sent, the library may
        // have started the session; nobody owns it now. Closing an unknown
        // handle fails harmlessly.
        let undo = Undo(Some(|| {
            shared.remove_session(&handle);
            shared.send_without_reply(
                method::SESSION_CLOSE,
                SessionParams {
                    session: handle.clone(),
                },
            );
        }));
        let started: StartResult = shared.request(method, params).await?;
        if started.session != handle {
            return Err(Error::protocol(format!("invalid {method} result")));
        }
        undo.disarm();
        Ok(Session {
            inner: Arc::new(SessionInner {
                client: client.clone(),
                state,
                handle,
                capabilities: started.capabilities,
            }),
        })
    }

    /// The library's handle for the session, stable for its lifetime.
    pub fn handle(&self) -> &str {
        &self.inner.handle
    }

    pub fn capabilities(&self) -> &Capabilities {
        &self.inner.capabilities
    }

    pub(crate) async fn request<R: DeserializeOwned>(
        &self,
        method: &str,
        params: impl Serialize,
    ) -> Result<R> {
        self.inner.client.shared.request(method, params).await
    }

    pub(crate) fn session_handle(&self) -> String {
        self.inner.handle.clone()
    }

    fn session_params(&self) -> SessionParams {
        self.inner.params()
    }

    /// Sends input (a `&str`, `String`, or [`Input`]) and returns the turn
    /// without waiting for it.
    pub async fn send(&self, input: impl Into<Input>) -> Result<Turn> {
        let turn_id = crate::client::new_uuid();
        let state = &self.inner.state;
        let log = state.add_turn(&turn_id);
        let undo = Undo(Some(|| state.remove_turn(&turn_id)));
        let params = SendParams {
            session: self.session_handle(),
            input: input.into(),
            turn_id: turn_id.clone(),
        };
        let _: SendResult = self.request(method::SESSION_SEND, params).await?;
        undo.disarm();
        if !log.is_ended() {
            self.end_log_if_never_run(&turn_id, &log);
        }
        Ok(Turn {
            id: turn_id,
            session: self.clone(),
            log,
        })
    }

    /// Input that never runs, such as queued input dropped by an interrupt,
    /// produces no `turn_completed`, so its log ends, and is dropped, when
    /// waiting for the turn fails. The task holds neither the [`Session`] nor the
    /// [`Client`](crate::Client), so it does not keep them open.
    fn end_log_if_never_run(&self, turn_id: &str, log: &Arc<TurnLog>) {
        let shared = self.inner.client.shared.clone();
        let turn_id = turn_id.to_owned();
        let params = WaitParams {
            session: self.session_handle(),
            turn_id: turn_id.clone(),
            timeout_ms: 0,
        };
        let (log, state) = (log.clone(), self.inner.state.clone());
        self.inner.client.shared.runtime.spawn(async move {
            let waited: Result<Value> = shared.request(method::SESSION_WAIT, params).await;
            if waited.is_err() {
                log.end();
                state.remove_turn(&turn_id);
            }
        });
    }

    /// Sends input and waits for the turn to end.
    pub async fn run(&self, input: impl Into<Input>) -> Result<TurnResult> {
        self.send(input).await?.wait().await
    }

    /// Waits for the turn `turn_id` to end; `None` waits without a limit.
    pub async fn wait(&self, turn_id: &str, timeout: Option<Duration>) -> Result<TurnResult> {
        let timeout_ms = timeout.map_or(0, |d| d.as_millis().clamp(1, i64::MAX as u128) as i64);
        self.request(
            method::SESSION_WAIT,
            WaitParams {
                session: self.session_handle(),
                turn_id: turn_id.into(),
                timeout_ms,
            },
        )
        .await
    }

    /// The session's events in arrival order. Buffering starts at the first
    /// call: the stream begins with the `session_started` event, and other
    /// earlier events are not kept for it. They are buffered
    /// without a limit until read; the stream ends when the session closes.
    /// One reader at a time: a second call waits until the first [`Events`]
    /// is dropped.
    pub async fn events(&self) -> Events {
        Events {
            rx: self.inner.state.event_reader().lock_owned().await,
            session: Arc::downgrade(&self.inner),
        }
    }

    /// Calls `listener` with every later event of the session, in order, on
    /// the connection's reader, before the event reaches any stream. It
    /// must not block; a panic is caught and does not stop delivery. The
    /// listener is removed when the [`Subscription`] is dropped.
    pub fn subscribe(
        &self,
        listener: impl Fn(&SessionEvent) + Send + Sync + 'static,
    ) -> Subscription {
        let state = &self.inner.state;
        let id = state.subscribe(Arc::new(listener), Arc::downgrade(&self.inner));
        Subscription {
            state: Arc::downgrade(state),
            id,
        }
    }

    /// Starts a new session that continues this one.
    pub async fn fork(&self, options: impl Into<SessionOptions>) -> Result<Session> {
        let (config, hooks) = options.into().split();
        let handle = self.inner.client.shared.new_id("s");
        let params = ForkParams {
            session: self.session_handle(),
            config,
            handle: handle.clone(),
        };
        Session::start(
            &self.inner.client,
            method::SESSION_FORK,
            handle,
            params,
            hooks,
        )
        .await
    }

    /// Answers a request event with a [`PermissionResponse`],
    /// [`UserInputResponse`], or [`ElicitationResponse`].
    pub async fn respond(&self, request_id: &str, response: impl RequestResponse) -> Result<()> {
        let params = respond_params(self.inner.handle.clone(), request_id.into(), response);
        self.request(method::REQUEST_RESPOND, params).await
    }

    /// Why the session closed: `None` while open or after a clean close.
    pub fn close_error(&self) -> Option<ErrorInfo> {
        self.inner.state.close_error.lock().unwrap().clone()
    }

    /// Closes the session. Its event stream ends after the last event.
    pub async fn close(&self) -> Result<()> {
        if self.inner.state.closed.load(Ordering::Acquire) {
            return Ok(());
        }
        self.request(method::SESSION_CLOSE, self.session_params())
            .await
    }
}

/// A sent turn.
#[derive(Clone)]
pub struct Turn {
    pub id: String,
    session: Session,
    log: Arc<TurnLog>,
}

impl Turn {
    pub fn session(&self) -> &Session {
        &self.session
    }

    /// Waits for the turn to end.
    pub async fn wait(&self) -> Result<TurnResult> {
        self.session.wait(&self.id, None).await
    }

    /// Waits up to `timeout`; fails with [`ErrorKind::Timeout`] if the turn
    /// is still running.
    pub async fn wait_timeout(&self, timeout: Duration) -> Result<TurnResult> {
        self.session.wait(&self.id, Some(timeout)).await
    }

    pub async fn interrupt(&self) -> Result<()> {
        self.session.interrupt_turn(&self.id).await
    }

    /// This turn's events, from its first through `turn_completed`. Each
    /// call starts again at the first event, and several streams may read at
    /// once. Reading takes nothing from [`Session::events`], which still gets
    /// every event, of this turn, of other turns, and of the session. The
    /// stream also ends when the session closes or the input never ran.
    pub async fn events(&self) -> TurnEvents {
        TurnEvents {
            log: self.log.clone(),
            next: 0,
            session: Arc::downgrade(&self.session.inner),
        }
    }
}

/// A session's event stream; see [`Session::events`]. Read it with
/// [`next`](Self::next) or as a [`Stream`].
pub struct Events {
    rx: OwnedMutexGuard<UnboundedReceiver<Delivered>>,
    session: Weak<SessionInner>,
}

impl Events {
    /// The next event; `None` once the session closed and every event was read.
    pub async fn next(&mut self) -> Option<SessionEvent> {
        let delivered = self.rx.recv().await?;
        Some(SessionEvent::new(delivered, &self.session))
    }

    /// The next event if one is queued.
    pub fn try_next(&mut self) -> Option<SessionEvent> {
        let delivered = self.rx.try_recv().ok()?;
        Some(SessionEvent::new(delivered, &self.session))
    }
}

impl Stream for Events {
    type Item = SessionEvent;

    fn poll_next(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<SessionEvent>> {
        let this = self.get_mut();
        let session = &this.session;
        this.rx
            .poll_recv(cx)
            .map(|d| d.map(|d| SessionEvent::new(d, session)))
    }
}

/// One turn's events; see [`Turn::events`]. Read it with
/// [`next`](Self::next) or as a [`Stream`].
pub struct TurnEvents {
    log: Arc<TurnLog>,
    next: usize,
    session: Weak<SessionInner>,
}

impl TurnEvents {
    /// The turn's next event; `None` after its `turn_completed`, or once the
    /// session closed.
    pub async fn next(&mut self) -> Option<SessionEvent> {
        std::future::poll_fn(|cx| self.poll_event(cx)).await
    }

    fn poll_event(&mut self, cx: &mut Context<'_>) -> Poll<Option<SessionEvent>> {
        let event = std::task::ready!(self.log.poll_event(self.next, cx));
        if event.is_some() {
            self.next += 1;
        }
        Poll::Ready(event.map(|d| SessionEvent::new(d, &self.session)))
    }
}

impl Stream for TurnEvents {
    type Item = SessionEvent;

    fn poll_next(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<SessionEvent>> {
        self.get_mut().poll_event(cx)
    }
}

/// Keeps a [`Session::subscribe`] listener registered; dropping it, or
/// [`unsubscribe`](Self::unsubscribe), removes the listener.
#[must_use = "dropping the subscription removes the listener"]
pub struct Subscription {
    state: Weak<SessionState>,
    id: u64,
}

impl Subscription {
    /// Removes the listener; it sees no later event.
    pub fn unsubscribe(self) {}
}

impl Drop for Subscription {
    fn drop(&mut self) {
        if let Some(state) = self.state.upgrade() {
            let mut subscribers = state.subscribers.lock().unwrap();
            *subscribers = subscribers
                .iter()
                .filter(|s| s.id != self.id)
                .cloned()
                .collect();
        }
    }
}

// ---------------------------------------------------------------- session events

/// An event as a stream or subscriber gets it: the [`Event`] (through
/// `Deref`), whether a handler answers it, and a weak link to its session,
/// so a request event is answered with its own helpers such as
/// [`allow`](Self::allow) or [`complete`](Self::complete). The link does
/// not keep the session open.
#[derive(Clone)]
pub struct SessionEvent {
    event: Arc<Event>,
    handled: bool,
    session: Weak<SessionInner>,
}

impl std::fmt::Debug for SessionEvent {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("SessionEvent")
            .field("event", &self.event)
            .field("handled", &self.handled)
            .finish_non_exhaustive()
    }
}

impl std::ops::Deref for SessionEvent {
    type Target = Event;

    fn deref(&self) -> &Event {
        &self.event
    }
}

impl From<SessionEvent> for Event {
    fn from(event: SessionEvent) -> Event {
        event.into_event()
    }
}

impl SessionEvent {
    fn new(delivered: Delivered, session: &Weak<SessionInner>) -> Self {
        Self {
            event: delivered.event,
            handled: delivered.handled,
            session: session.clone(),
        }
    }

    pub fn event(&self) -> &Event {
        &self.event
    }

    /// The event itself, copied only when another stream still holds it.
    pub fn into_event(self) -> Event {
        Arc::try_unwrap(self.event).unwrap_or_else(|shared| Event::clone(&shared))
    }

    /// True for a request event that a [`SessionOptions`] handler answers;
    /// it is shown for information and needs no answer.
    pub fn handled(&self) -> bool {
        self.handled
    }

    /// The session the event came from, if it is still open in this process.
    pub fn session(&self) -> Option<Session> {
        self.session.upgrade().map(|inner| Session { inner })
    }

    /// The kind name and ID of a request event.
    fn request(&self) -> Option<(&'static str, &str)> {
        match &self.event.payload {
            EventPayload::PermissionRequest(r) => Some(("permission_request", &r.id)),
            EventPayload::UserInputRequest(r) => Some(("user_input_request", &r.id)),
            EventPayload::ElicitationRequest(r) => Some(("elicitation_request", &r.id)),
            EventPayload::DialogRequest(r) => Some(("dialog_request", &r.id)),
            EventPayload::NativeRequest(r) => Some(("native_request", &r.id)),
            _ => None,
        }
    }

    /// The request ID of a request event.
    pub fn request_id(&self) -> Option<&str> {
        self.request().map(|(_, id)| id)
    }

    /// Answers this request event in its session. Fails with
    /// [`ErrorKind::InvalidParams`] for an event that is not a request and
    /// [`ErrorKind::Closed`] once the session was dropped.
    pub async fn respond(&self, response: impl RequestResponse) -> Result<()> {
        let id = self
            .request_id()
            .ok_or_else(|| Error::new(ErrorKind::InvalidParams, "not a request event"))?;
        let session = self
            .session()
            .ok_or_else(|| Error::closed("the session was dropped"))?;
        session.respond(id, response).await
    }

    /// The request ID and session, for a request event of `kind`.
    fn target(&self, kind: &'static str) -> Result<(&str, Session)> {
        let id = match self.request() {
            Some((actual, id)) if actual == kind => id,
            _ => {
                let message = format!("not a {kind} event");
                return Err(Error::new(ErrorKind::InvalidParams, message));
            }
        };
        let session = self
            .session()
            .ok_or_else(|| Error::closed("the session was dropped"))?;
        Ok((id, session))
    }

    async fn respond_to(&self, kind: &'static str, response: impl RequestResponse) -> Result<()> {
        let (id, session) = self.target(kind)?;
        session.respond(id, response).await
    }

    /// Allows a permission request's call once.
    pub async fn allow(&self) -> Result<()> {
        let (id, session) = self.target("permission_request")?;
        session.allow(id).await
    }

    /// Denies a permission request's call, telling the model why.
    pub async fn deny(&self, message: &str) -> Result<()> {
        let (id, session) = self.target("permission_request")?;
        session.deny(id, message).await
    }

    /// Answers a user input request by question ID: the selected labels or
    /// text.
    pub async fn answer(&self, answers: HashMap<String, Vec<String>>) -> Result<()> {
        let (id, session) = self.target("user_input_request")?;
        session.answer(id, answers).await
    }

    /// Accepts an elicitation with the form content.
    pub async fn accept(&self, content: Value) -> Result<()> {
        let response = ElicitationResponse {
            action: ElicitationResponseAction::Accept,
            content: Some(content),
            ..Default::default()
        };
        self.respond_to("elicitation_request", response).await
    }

    /// Declines an elicitation.
    pub async fn decline(&self) -> Result<()> {
        let (id, session) = self.target("elicitation_request")?;
        session.decline(id).await
    }

    /// Completes a dialog request with an optional result.
    pub async fn complete(&self, result: Option<Value>) -> Result<()> {
        let response = DialogResponse {
            behavior: DialogResponseBehavior::Completed,
            result,
        };
        self.respond_to("dialog_request", response).await
    }

    /// Cancels a dialog request.
    pub async fn cancel(&self) -> Result<()> {
        let response = DialogResponse {
            behavior: DialogResponseBehavior::Cancelled,
            result: None,
        };
        self.respond_to("dialog_request", response).await
    }

    /// Answers a native request with an optional result.
    pub async fn reply(&self, result: Option<Value>) -> Result<()> {
        let response = NativeResponse {
            result,
            error: None,
        };
        self.respond_to("native_request", response).await
    }

    /// Answers a native request with a JSON-RPC error.
    pub async fn fail(&self, code: i64, message: &str) -> Result<()> {
        let response = NativeResponse {
            result: None,
            error: Some(NativeResponseError {
                code,
                message: message.into(),
            }),
        };
        self.respond_to("native_request", response).await
    }
}
