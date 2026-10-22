use std::collections::HashMap;
use std::hash::BuildHasher;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use serde_json::value::RawValue;
use tokio::runtime::Handle;
use tokio::sync::{oneshot, watch};

use crate::error::{Error, Result};
use crate::protocol::*;
use crate::session::{Hooks, Session, SessionOptions, SessionState};
use crate::transport::Transport;

/// How to connect: which providers to offer and over which transport.
#[derive(Debug, Clone, Default)]
pub struct Options {
    providers: HashMap<String, Option<Value>>,
    agentd: Option<PathBuf>,
}

impl Options {
    /// Offers the provider `name` with its options (`json!({})` for the
    /// defaults). Without any, the library offers every real harness.
    pub fn provider(mut self, name: impl Into<String>, options: Value) -> Self {
        self.providers.insert(name.into(), Some(options));
        self
    }

    /// Runs the protocol through the `agentd` executable at `path` instead of
    /// the linked library.
    pub fn agentd(mut self, path: impl Into<PathBuf>) -> Self {
        self.agentd = Some(path.into());
        self
    }
}

/// One connection to the library: its providers and sessions. Cloning is
/// cheap and shares the connection, which closes when the last clone and
/// every session are dropped, or on [`Client::close`].
///
/// Host tools and request handlers run as tasks on the tokio runtime that
/// called [`Client::open`].
#[derive(Clone)]
pub struct Client {
    pub(crate) inner: Arc<ClientInner>,
}

pub(crate) struct ClientInner {
    pub(crate) shared: Arc<Shared>,
    init: InitializeResult,
}

impl Client {
    /// Connects and initializes the library with the providers of `options`.
    /// Must run inside a tokio runtime.
    pub async fn open(options: Options) -> Result<Client> {
        let runtime = Handle::try_current()
            .map_err(|_| Error::internal("Client::open must run inside a tokio runtime"))?;
        let (transport, reader) = match &options.agentd {
            Some(path) => Transport::spawn_agentd(path, &runtime)?,
            None => Transport::open_native()?,
        };
        let shared = Arc::new(Shared {
            transport,
            runtime,
            next_id: AtomicU64::new(1),
            id_prefix: format!(
                "{:016x}",
                std::collections::hash_map::RandomState::new().hash_one(())
            ),
            last_id: AtomicU64::new(0),
            pending: Mutex::new(Pending::default()),
            sessions: Mutex::new(HashMap::new()),
            finished: watch::channel(false).0,
        });
        if let Err(e) = reader.start(shared.clone()) {
            shared.transport.close();
            return Err(e);
        }
        let params = InitializeParams {
            providers: (!options.providers.is_empty()).then_some(options.providers),
            ..Default::default()
        };
        let init = match shared.request(method::INITIALIZE, params).await {
            Ok(init) => init,
            Err(e) => {
                shared.close().await;
                return Err(e);
            }
        };
        Ok(Client {
            inner: Arc::new(ClientInner { shared, init }),
        })
    }

    /// The providers the library offers, with their capabilities.
    pub fn providers(&self) -> &[ProviderInfo] {
        &self.inner.init.providers
    }

    /// The library version reported by `initialize`.
    pub fn library_version(&self) -> &str {
        &self.inner.init.library_version
    }

    /// Starts a session. `options` is a [`Config`] or a [`SessionOptions`]
    /// with host tools and request handlers.
    pub async fn start(
        &self,
        provider: &str,
        options: impl Into<SessionOptions>,
    ) -> Result<Session> {
        let (options, hooks) = options.into().split();
        let handle = self.inner.shared.new_id("s");
        let params = StartParams {
            provider: provider.into(),
            config: options,
            handle: handle.clone(),
        };
        Session::start(&self.inner, method::SESSION_START, handle, params, hooks).await
    }

    /// Sends any protocol request and decodes its result.
    pub async fn request<R: DeserializeOwned>(
        &self,
        method: &str,
        params: impl Serialize,
    ) -> Result<R> {
        self.inner.shared.request(method, params).await
    }

    /// Closes every session and the connection, and waits until every
    /// session's event stream has ended. Later calls fail with
    /// [`ErrorKind::Closed`].
    pub async fn close(&self) {
        self.inner.shared.close().await;
    }
}

impl Drop for ClientInner {
    fn drop(&mut self) {
        // Starts closing without waiting; the reader finishes in the background.
        self.shared.transport.close();
    }
}

// ---------------------------------------------------------------- shared state

/// A response's `result`, still encoded; `None` when the response had none.
type Reply = Option<Box<RawValue>>;

#[derive(Default)]
struct Pending {
    closed: bool,
    waiters: HashMap<u64, oneshot::Sender<Result<Reply>>>,
}

/// What the reader and every caller share. The transport is freed with the
/// last reference.
pub(crate) struct Shared {
    pub(crate) transport: Transport,
    /// Where host tools and request handlers run.
    pub(crate) runtime: Handle,
    next_id: AtomicU64,
    /// With `last_id`, makes the session handles and turn IDs this client
    /// chooses, so their state exists before the library uses them.
    id_prefix: String,
    last_id: AtomicU64,
    pending: Mutex<Pending>,
    sessions: Mutex<HashMap<String, Arc<SessionState>>>,
    /// True once the reader delivered the last message.
    finished: watch::Sender<bool>,
}

#[derive(Serialize)]
struct Request<'a, P> {
    jsonrpc: &'static str,
    id: u64,
    method: &'a str,
    params: P,
}

#[derive(Serialize)]
struct Response<R> {
    jsonrpc: &'static str,
    id: Value,
    result: R,
}

#[derive(Deserialize)]
struct Incoming {
    id: Option<Value>,
    method: Option<String>,
    params: Option<Box<RawValue>>,
    result: Reply,
    error: Option<RpcError>,
}

/// Decodes a message's `params` or `result`, where a missing one reads as
/// `null`.
fn decode<T: DeserializeOwned>(raw: &Option<Box<RawValue>>) -> serde_json::Result<T> {
    serde_json::from_str(raw.as_deref().map_or("null", RawValue::get))
}

#[derive(Deserialize)]
struct RpcError {
    #[serde(default)]
    code: i64,
    #[serde(default)]
    message: String,
    data: Option<Value>,
}

impl Shared {
    fn encode(&self, method: &str, params: impl Serialize) -> Result<(u64, String)> {
        let id = self.next_id.fetch_add(1, Ordering::Relaxed);
        let msg = serde_json::to_string(&Request {
            jsonrpc: "2.0",
            id,
            method,
            params,
        })
        .map_err(|e| Error::internal(format!("cannot encode {method}: {e}")))?;
        Ok((id, msg))
    }

    /// Sends a request and waits for its response. `params` is encoded and
    /// dropped before the first await, so the future does not hold it.
    pub(crate) async fn request<P: Serialize, R: DeserializeOwned>(
        &self,
        method: &str,
        params: P,
    ) -> Result<R> {
        let (id, msg) = self.encode(method, params)?;
        let (tx, rx) = oneshot::channel();
        {
            let mut pending = self.pending.lock().unwrap();
            if pending.closed {
                return Err(Error::closed("the connection is closed"));
            }
            pending.waiters.insert(id, tx);
        }
        if let Err(e) = self.transport.send(&msg) {
            self.pending.lock().unwrap().waiters.remove(&id);
            return Err(e);
        }
        let result = rx
            .await
            .map_err(|_| Error::closed("the connection closed"))??;
        decode(&result).map_err(|e| Error::protocol(format!("invalid {method} result: {e}")))
    }

    /// Sends a request whose response nobody waits for. Never blocks, so
    /// `Drop` can use it.
    pub(crate) fn send_without_reply(&self, method: &str, params: impl Serialize) {
        if let Ok((_, msg)) = self.encode(method, params) {
            let _ = self.transport.send(&msg);
        }
    }

    /// Starts closing the connection and waits until the reader finished.
    async fn close(&self) {
        self.transport.close();
        let _ = self.finished.subscribe().wait_for(|done| *done).await;
    }

    fn reply(&self, id: Value, result: impl Serialize) {
        if let Ok(msg) = serde_json::to_string(&Response {
            jsonrpc: "2.0",
            id,
            result,
        }) {
            let _ = self.transport.send(&msg);
        }
    }

    fn reply_error(&self, id: Value, code: i64, message: &str) {
        let msg = serde_json::json!({"jsonrpc": "2.0", "id": id, "error": {"code": code, "message": message}});
        let _ = self.transport.send(&msg.to_string());
    }

    /// A session handle unique to this client.
    pub(crate) fn new_id(&self, kind: &str) -> String {
        let n = self.last_id.fetch_add(1, Ordering::Relaxed) + 1;
        format!("{}-{kind}{n}", self.id_prefix)
    }

    fn find_session(&self, handle: &str) -> Option<Arc<SessionState>> {
        self.sessions.lock().unwrap().get(handle).cloned()
    }

    /// Adds the state of a session about to be started under `handle`.
    pub(crate) fn add_session(&self, handle: &str, hooks: Hooks) -> Arc<SessionState> {
        let state = Arc::new(SessionState::new(hooks));
        self.sessions
            .lock()
            .unwrap()
            .insert(handle.into(), state.clone());
        state
    }

    /// Drops the state of a session that did not start.
    pub(crate) fn remove_session(&self, handle: &str) {
        self.sessions.lock().unwrap().remove(handle);
    }

    /// Runs once the reader delivered the last message: fails the requests
    /// still waiting and ends every session's event stream.
    pub(crate) fn finish(&self) {
        let waiters = {
            let mut pending = self.pending.lock().unwrap();
            pending.closed = true;
            std::mem::take(&mut pending.waiters)
        };
        drop(waiters);
        let sessions = std::mem::take(&mut *self.sessions.lock().unwrap());
        for state in sessions.values() {
            state.mark_closed(Some(*Error::closed("the connection closed").info));
        }
        self.transport.close();
        self.finished.send_replace(true);
    }

    /// Handles one incoming message without blocking.
    pub(crate) fn dispatch(self: &Arc<Self>, text: &str) {
        let Ok(msg) = serde_json::from_str::<Incoming>(text) else {
            return;
        };
        let Some(name) = msg.method else {
            let Some(id) = msg.id.as_ref().and_then(Value::as_u64) else {
                return;
            };
            let Some(waiter) = self.pending.lock().unwrap().waiters.remove(&id) else {
                return;
            };
            let _ = waiter.send(match msg.error {
                Some(e) => Err(rpc_error(e)),
                None => Ok(msg.result),
            });
            return;
        };
        match name.as_str() {
            method::SESSION_EVENT => {
                if let Ok(n) = decode::<EventNotification>(&msg.params)
                    && let Some(state) = self.find_session(&n.session)
                {
                    state.deliver(self, &n.session, n.event);
                }
            }
            method::SESSION_CLOSED => {
                let Ok(n) = decode::<ClosedNotification>(&msg.params) else {
                    return;
                };
                let state = self.sessions.lock().unwrap().remove(&n.session);
                if let Some(state) = state {
                    state.mark_closed(n.error);
                }
            }
            method::TOOL_CANCEL => {
                if let Ok(p) = decode::<ToolCancelParams>(&msg.params)
                    && let Some(state) = self.find_session(&p.session)
                {
                    state.cancel_tool(&p.call_id);
                }
            }
            method::TOOL_CALL => {
                let Some(id) = msg.id else { return };
                let p = match decode::<ToolCallParams>(&msg.params) {
                    Ok(p) => p,
                    Err(e) => {
                        let error = format!("invalid tool call: {e}");
                        return self.reply(id, ToolResult::error(error));
                    }
                };
                let Some(state) = self.find_session(&p.session) else {
                    let error = format!("unknown session {}", p.session);
                    return self.reply(id, ToolResult::error(error));
                };
                let call = p.call;
                // Registered here on the reader, before any later `tool/cancel`.
                let cancel = state.register_tool_call(&call.id);
                let shared = self.clone();
                self.runtime.spawn(async move {
                    let result = state.run_tool(call, cancel).await;
                    shared.reply(id, result);
                });
            }
            _ => {
                if let Some(id) = msg.id {
                    self.reply_error(id, METHOD_NOT_FOUND, &format!("unknown method {name}"));
                }
            }
        }
    }
}

/// The JSON-RPC code for a method the receiver does not know.
const METHOD_NOT_FOUND: i64 = -32601;

fn rpc_error(e: RpcError) -> Error {
    let info = e
        .data
        .and_then(|d| serde_json::from_value::<ErrorInfo>(d).ok());
    let info = info.unwrap_or(ErrorInfo {
        kind: ErrorKind::Protocol,
        message: e.message,
        ..Default::default()
    });
    Error {
        info: Box::new(info),
        code: e.code,
    }
}

/// A random RFC 4122 version 4 UUID; Claude requires turn IDs to be UUIDs.
/// `RandomState` seeds are random per process and per instance, which is
/// enough for uniqueness; the IDs need not be secret.
pub(crate) fn new_uuid() -> String {
    let state = std::collections::hash_map::RandomState::new();
    let high = state.hash_one(0u8).to_be_bytes();
    let low = state.hash_one(1u8).to_be_bytes();
    let mut b = [0u8; 16];
    b[..8].copy_from_slice(&high);
    b[8..].copy_from_slice(&low);
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    let h: String = b.iter().map(|x| format!("{x:02x}")).collect();
    format!(
        "{}-{}-{}-{}-{}",
        &h[0..8],
        &h[8..12],
        &h[12..16],
        &h[16..20],
        &h[20..]
    )
}
