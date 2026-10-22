use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
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
    /// task. The request still appears in the event stream.
    pub fn on_permission<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(PermissionRequest) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = PermissionResponse> + Send + 'static,
    {
        self.hooks.on_permission = Some(boxed_handler(handler));
        self
    }

    /// Answers every user input request with `handler`, as its own tokio task.
    pub fn on_user_input<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(UserInputRequest) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = UserInputResponse> + Send + 'static,
    {
        self.hooks.on_user_input = Some(boxed_handler(handler));
        self
    }

    /// Answers every MCP elicitation with `handler`, as its own tokio task.
    pub fn on_elicitation<F, Fut>(mut self, handler: F) -> Self
    where
        F: Fn(ElicitationRequest) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = ElicitationResponse> + Send + 'static,
    {
        self.hooks.on_elicitation = Some(boxed_handler(handler));
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
}

/// The reader's view of a session: its event queue, turn logs, and hooks.
pub(crate) struct SessionState {
    events_tx: Mutex<Option<UnboundedSender<Arc<Event>>>>,
    /// Held by the one [`Events`] reading the session.
    events_rx: Arc<AsyncMutex<UnboundedReceiver<Arc<Event>>>>,
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
        let (tx, rx) = mpsc::unbounded_channel();
        Self {
            events_tx: Mutex::new(Some(tx)),
            events_rx: Arc::new(AsyncMutex::new(rx)),
            closed: AtomicBool::new(false),
            close_error: Mutex::new(None),
            hooks,
            turns: Mutex::new(HashMap::new()),
            tool_calls: Mutex::new(HashMap::new()),
        }
    }

    /// Queues an event for the session stream and its turn's log, and starts
    /// the matching request handler, if any. Runs on the reader, so it never
    /// blocks.
    pub(crate) fn deliver(&self, shared: &Arc<Shared>, handle: &str, event: Event) {
        let event = Arc::new(event);
        start_handler(&self.hooks, shared, handle, &event);
        if !event.turn_id.is_empty() {
            self.log_turn_event(&event);
        }
        if let Some(tx) = &*self.events_tx.lock().unwrap() {
            let _ = tx.send(event);
        }
    }

    fn log_turn_event(&self, event: &Arc<Event>) {
        let mut turns = self.turns.lock().unwrap();
        let Some(log) = turns.get(&event.turn_id) else {
            return;
        };
        log.add(event.clone());
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
        self.events_tx.lock().unwrap().take();
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

/// Starts the handler for a request event as a task, if the hooks have one.
fn start_handler(hooks: &Hooks, shared: &Arc<Shared>, handle: &str, event: &Event) {
    let answer = match &event.payload {
        EventPayload::PermissionRequest(r) => auto_answer(&hooks.on_permission, r, &r.id, handle),
        EventPayload::UserInputRequest(r) => auto_answer(&hooks.on_user_input, r, &r.id, handle),
        EventPayload::ElicitationRequest(r) => auto_answer(&hooks.on_elicitation, r, &r.id, handle),
        _ => None,
    };
    if let Some(answer) = answer {
        let task_shared = shared.clone();
        shared.runtime.spawn(async move {
            let params = answer.await;
            let _: Result<()> = task_shared.request(method::REQUEST_RESPOND, params).await;
        });
    }
}

/// The events of one turn, kept for every [`TurnEvents`] that reads it.
#[derive(Default)]
pub(crate) struct TurnLog {
    state: Mutex<TurnLogState>,
}

#[derive(Default)]
struct TurnLogState {
    events: Vec<Arc<Event>>,
    ended: bool,
    /// The readers waiting for the next event.
    wakers: Vec<Waker>,
}

impl TurnLog {
    fn add(&self, event: Arc<Event>) {
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

    fn poll_event(&self, index: usize, cx: &mut Context<'_>) -> Poll<Option<Event>> {
        let mut state = self.state.lock().unwrap();
        if let Some(event) = state.events.get(index) {
            return Poll::Ready(Some(Event::clone(event)));
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

/// The answer `handler` gives `request`, computed later as a task.
fn auto_answer<Req, Resp>(
    handler: &Option<Handler<Req, Resp>>,
    request: &Req,
    request_id: &str,
    handle: &str,
) -> Option<BoxFuture<RespondParams>>
where
    Req: Clone + Send + 'static,
    Resp: RequestResponse + 'static,
{
    let handler = handler.clone()?;
    let (request, request_id, handle) = (request.clone(), request_id.to_owned(), handle.to_owned());
    // The handler is called inside the task, so its synchronous part never
    // runs on the reader.
    Some(Box::pin(async move {
        respond_params(handle, request_id, handler(request).await)
    }))
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

/// An answer to a permission, user input, or elicitation request.
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

    /// The session's events in arrival order. They are buffered without a
    /// limit until read; the stream ends when the session closes. One reader
    /// at a time: a second call waits until the first [`Events`] is dropped.
    pub async fn events(&self) -> Events {
        Events {
            rx: self.inner.state.events_rx.clone().lock_owned().await,
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
        }
    }
}

/// A session's event stream; see [`Session::events`]. Read it with
/// [`next`](Self::next) or as a [`Stream`].
pub struct Events {
    rx: OwnedMutexGuard<UnboundedReceiver<Arc<Event>>>,
}

/// The event itself, copied only when a turn log still holds it.
fn into_owned(event: Arc<Event>) -> Event {
    Arc::try_unwrap(event).unwrap_or_else(|shared| Event::clone(&shared))
}

impl Events {
    /// The next event; `None` once the session closed and every event was read.
    pub async fn next(&mut self) -> Option<Event> {
        self.rx.recv().await.map(into_owned)
    }

    /// The next event if one is queued.
    pub fn try_next(&mut self) -> Option<Event> {
        self.rx.try_recv().ok().map(into_owned)
    }
}

impl Stream for Events {
    type Item = Event;

    fn poll_next(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<Event>> {
        self.get_mut()
            .rx
            .poll_recv(cx)
            .map(|event| event.map(into_owned))
    }
}

/// One turn's events; see [`Turn::events`]. Read it with
/// [`next`](Self::next) or as a [`Stream`].
pub struct TurnEvents {
    log: Arc<TurnLog>,
    next: usize,
}

impl TurnEvents {
    /// The turn's next event; `None` after its `turn_completed`, or once the
    /// session closed.
    pub async fn next(&mut self) -> Option<Event> {
        std::future::poll_fn(|cx| self.poll_event(cx)).await
    }

    fn poll_event(&mut self, cx: &mut Context<'_>) -> Poll<Option<Event>> {
        let event = std::task::ready!(self.log.poll_event(self.next, cx));
        if event.is_some() {
            self.next += 1;
        }
        Poll::Ready(event)
    }
}

impl Stream for TurnEvents {
    type Item = Event;

    fn poll_next(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<Event>> {
        self.get_mut().poll_event(cx)
    }
}

// ---------------------------------------------------------------- answering request events

impl PermissionRequest {
    /// Answers this request in `session`, the session whose stream it came from.
    pub async fn respond(&self, session: &Session, response: PermissionResponse) -> Result<()> {
        session.respond(&self.id, response).await
    }

    /// Allows the call once.
    pub async fn allow(&self, session: &Session) -> Result<()> {
        session.allow(&self.id).await
    }

    /// Denies the call, telling the model why.
    pub async fn deny(&self, session: &Session, message: &str) -> Result<()> {
        session.deny(&self.id, message).await
    }
}

impl UserInputRequest {
    /// Answers this request in `session`, the session whose stream it came from.
    pub async fn respond(&self, session: &Session, response: UserInputResponse) -> Result<()> {
        session.respond(&self.id, response).await
    }

    /// Answers by question ID: the selected labels or text.
    pub async fn answer(
        &self,
        session: &Session,
        answers: HashMap<String, Vec<String>>,
    ) -> Result<()> {
        session.answer(&self.id, answers).await
    }
}

impl ElicitationRequest {
    /// Answers this request in `session`, the session whose stream it came from.
    pub async fn respond(&self, session: &Session, response: ElicitationResponse) -> Result<()> {
        session.respond(&self.id, response).await
    }

    /// Accepts with the form content.
    pub async fn accept(&self, session: &Session, content: Value) -> Result<()> {
        session.accept(&self.id, content).await
    }

    pub async fn decline(&self, session: &Session) -> Result<()> {
        session.decline(&self.id).await
    }
}
