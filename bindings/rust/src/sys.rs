//! Raw declarations of the C ABI in `include/agent.h`.
#![allow(non_camel_case_types)]

use std::ffi::{c_char, c_int};

/// An opaque connection handle.
#[repr(C)]
pub struct agent_connection {
    _private: [u8; 0],
}

unsafe extern "C" {
    /// Opens a connection: its own providers and sessions. Null on failure.
    pub fn agent_open() -> *mut agent_connection;
    /// Sends one JSON-RPC message. Returns 0, or -1 for invalid arguments. Thread-safe.
    pub fn agent_send(conn: *mut agent_connection, msg: *const c_char, len: usize) -> c_int;
    /// Waits up to `timeout_ms` (negative: forever) for the next message. Returns 1 and sets
    /// `*out` to a NUL-terminated JSON text to free with `agent_free`, 0 on timeout, or -1 once
    /// the connection is closed and drained.
    pub fn agent_recv(
        conn: *mut agent_connection,
        timeout_ms: c_int,
        out: *mut *mut c_char,
    ) -> c_int;
    /// Like `agent_recv`, but takes every queued message at once: they are joined by newlines
    /// (`'\n'`; JSON texts contain none) in one NUL-terminated buffer of `*len` bytes, freed with
    /// `agent_free`.
    pub fn agent_recv_batch(
        conn: *mut agent_connection,
        timeout_ms: c_int,
        out: *mut *mut c_char,
        len: *mut usize,
    ) -> c_int;
    /// Frees a message from `agent_recv` or `agent_recv_batch`.
    pub fn agent_free(msg: *mut c_char);
    /// Closes every session and wakes `agent_recv`, which then drains and returns -1.
    pub fn agent_close(conn: *mut agent_connection);
    /// Frees the connection, closing it first. No thread may use it afterwards.
    pub fn agent_destroy(conn: *mut agent_connection);
    /// The library version, such as "0.2.0".
    pub fn agent_version() -> *const c_char;
}
