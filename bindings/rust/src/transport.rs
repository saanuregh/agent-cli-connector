//! Moves JSON-RPC messages between the client and the library: in process
//! through the C ABI, or through an `agentd` subprocess, one message per line.
//!
//! Sending never blocks, so it is safe in `Drop` and on the reader. Incoming
//! messages go to [`Shared::dispatch`], on a dedicated thread for the C ABI
//! and on a tokio task for `agentd`.
use std::ffi::{CStr, c_char};
use std::path::Path;
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::thread;
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStdin, ChildStdout, Command};
use tokio::runtime::Handle;
use tokio::sync::mpsc::{self, UnboundedReceiver, UnboundedSender};

use crate::client::Shared;
use crate::error::Error;
use crate::sys;

pub(crate) enum Transport {
    Native(Arc<Native>),
    Subprocess(Subprocess),
}

/// The receiving half of a transport, started once [`Shared`] exists.
pub(crate) enum Reader {
    Native(Arc<Native>),
    Subprocess(ChildStdout),
}

impl Transport {
    pub(crate) fn open_native() -> Result<(Self, Reader), Error> {
        let native = Arc::new(Native::open()?);
        Ok((Self::Native(native.clone()), Reader::Native(native)))
    }

    pub(crate) fn spawn_agentd(program: &Path, runtime: &Handle) -> Result<(Self, Reader), Error> {
        let (subprocess, stdout) = Subprocess::spawn(program, runtime)?;
        Ok((Self::Subprocess(subprocess), Reader::Subprocess(stdout)))
    }

    /// Queues one message. Never blocks.
    pub(crate) fn send(&self, msg: &str) -> Result<(), Error> {
        match self {
            Self::Native(native) => native.send(msg),
            Self::Subprocess(subprocess) => subprocess.send(msg),
        }
    }

    /// Starts closing the connection: the reader drains what is left, then
    /// calls [`Shared::finish`]. Never blocks; later calls do nothing.
    pub(crate) fn close(&self) {
        match self {
            Self::Native(native) => native.close(),
            Self::Subprocess(subprocess) => subprocess.close(),
        }
    }
}

impl Reader {
    /// Delivers every incoming message to `shared` until the connection
    /// closes, then finishes it.
    pub(crate) fn start(self, shared: Arc<Shared>) -> Result<(), Error> {
        match self {
            Self::Native(native) => {
                thread::Builder::new()
                    .name("agent-cli-connector reader".into())
                    .spawn(move || {
                        while let Some(batch) = native.recv() {
                            for text in batch.split('\n').filter(|t| !t.is_empty()) {
                                shared.dispatch(text);
                            }
                        }
                        shared.finish();
                        native.destroy();
                    })
                    .map_err(|e| Error::internal(format!("cannot start the reader thread: {e}")))?;
            }
            Self::Subprocess(stdout) => {
                let runtime = shared.runtime.clone();
                runtime.spawn(async move {
                    let mut lines = BufReader::new(stdout).lines();
                    while let Ok(Some(line)) = lines.next_line().await {
                        if !line.trim().is_empty() {
                            shared.dispatch(&line);
                        }
                    }
                    shared.finish();
                });
            }
        }
        Ok(())
    }
}

/// The library linked into this process.
pub(crate) struct Native {
    conn: *mut sys::agent_connection,
    /// False once the reader thread destroyed `conn`. Every other use holds
    /// the read lock, so the handle is never used after it is freed.
    alive: RwLock<bool>,
    close_started: AtomicBool,
}

// The C ABI is thread-safe; `alive` guards the handle's lifetime.
unsafe impl Send for Native {}
unsafe impl Sync for Native {}

/// How long one `agent_recv_batch` call waits before the reader loops.
const RECV_TIMEOUT_MS: i32 = 250;

impl Native {
    fn open() -> Result<Self, Error> {
        let conn = unsafe { sys::agent_open() };
        if conn.is_null() {
            return Err(Error::internal("agent_open failed"));
        }
        Ok(Self {
            conn,
            alive: RwLock::new(true),
            close_started: AtomicBool::new(false),
        })
    }

    fn send(&self, msg: &str) -> Result<(), Error> {
        let alive = self.alive.read().unwrap();
        if !*alive {
            return Err(Error::closed("the connection is closed"));
        }
        let rc = unsafe { sys::agent_send(self.conn, msg.as_ptr().cast::<c_char>(), msg.len()) };
        if rc != 0 {
            return Err(Error::internal("agent_send rejected the message"));
        }
        Ok(())
    }

    /// Blocks for the next messages and returns every queued one, joined by
    /// newlines; `None` once the connection is closed and drained. Only the
    /// reader thread calls it, and only it destroys `conn`.
    fn recv(&self) -> Option<String> {
        loop {
            let mut out: *mut c_char = std::ptr::null_mut();
            let mut len = 0usize;
            match unsafe { sys::agent_recv_batch(self.conn, RECV_TIMEOUT_MS, &mut out, &mut len) } {
                1 => {
                    let bytes = unsafe { std::slice::from_raw_parts(out.cast::<u8>(), len) };
                    let text = String::from_utf8_lossy(bytes).into_owned();
                    unsafe { sys::agent_free(out) };
                    return Some(text);
                }
                0 => continue,
                _ => return None,
            }
        }
    }

    /// `agent_close` closes every session and may wait for harness processes,
    /// so it runs on its own thread instead of the caller's.
    fn close(self: &Arc<Self>) {
        if self.close_started.swap(true, Ordering::AcqRel) {
            return;
        }
        let native = self.clone();
        let closer = move || native.close_now();
        let spawned = thread::Builder::new()
            .name("agent-cli-connector close".into())
            .spawn(closer);
        if spawned.is_err() {
            // Without a thread, closing here is the only way to stop the reader.
            self.close_now();
        }
    }

    fn close_now(&self) {
        // The guard is held across the call, so `destroy` waits for it.
        let alive = self.alive.read().unwrap();
        if *alive {
            unsafe { sys::agent_close(self.conn) };
        }
    }

    fn destroy(&self) {
        let mut alive = self.alive.write().unwrap();
        if *alive {
            *alive = false;
            unsafe { sys::agent_destroy(self.conn) };
        }
    }
}

impl Drop for Native {
    fn drop(&mut self) {
        // Reached with a live handle only when the reader never started.
        self.destroy();
    }
}

/// An `agentd` subprocess. A tokio task writes its input and, once the input
/// closes, waits for it to exit.
pub(crate) struct Subprocess {
    input: Mutex<Option<UnboundedSender<String>>>,
}

/// How long `agentd` may take to exit after its input closed.
const EXIT_GRACE: Duration = Duration::from_secs(5);

impl Subprocess {
    fn spawn(program: &Path, runtime: &Handle) -> Result<(Self, ChildStdout), Error> {
        let _entered = runtime.enter();
        let mut child = Command::new(program)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .kill_on_drop(true)
            .spawn()
            .map_err(|e| Error::internal(format!("cannot start {}: {e}", program.display())))?;
        let stdin = child.stdin.take().expect("stdin is piped");
        let stdout = child.stdout.take().expect("stdout is piped");
        let (tx, rx) = mpsc::unbounded_channel();
        runtime.spawn(write_and_reap(child, stdin, rx));
        Ok((
            Self {
                input: Mutex::new(Some(tx)),
            },
            stdout,
        ))
    }

    fn send(&self, msg: &str) -> Result<(), Error> {
        let closed = || Error::closed("agentd input is closed");
        let input = self.input.lock().unwrap();
        let input = input.as_ref().ok_or_else(closed)?;
        let mut line = String::with_capacity(msg.len() + 1);
        line.push_str(msg);
        line.push('\n');
        input.send(line).map_err(|_| closed())
    }

    fn close(&self) {
        // agentd shuts down once its input closes.
        self.input.lock().unwrap().take();
    }
}

async fn write_and_reap(
    mut child: Child,
    mut stdin: ChildStdin,
    mut lines: UnboundedReceiver<String>,
) {
    while let Some(line) = lines.recv().await {
        if stdin.write_all(line.as_bytes()).await.is_err() || stdin.flush().await.is_err() {
            break;
        }
    }
    drop(lines);
    drop(stdin);
    if tokio::time::timeout(EXIT_GRACE, child.wait())
        .await
        .is_err()
    {
        let _ = child.kill().await;
    }
}

/// The version of the linked library, such as "0.2.0".
pub fn library_version() -> &'static str {
    unsafe { CStr::from_ptr(sys::agent_version()) }
        .to_str()
        .unwrap_or("")
}
