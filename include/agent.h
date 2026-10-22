/*
 * agent-cli-connector: one API over coding-agent CLIs (Claude Code, Codex, Pi).
 *
 * The C ABI is an in-process JSON-RPC 2.0 message pipe. Send requests with
 * agent_send and read responses, notifications (session/event,
 * session/closed), and the library's own requests (tool/call) with
 * agent_recv, typically from a dedicated thread. The protocol is documented
 * in README.md and schema/protocol.schema.json.
 */
#ifndef AGENT_CLI_CONNECTOR_H
#define AGENT_CLI_CONNECTOR_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct agent_connection agent_connection;

/* Opens a connection: its own providers and sessions. NULL on failure. */
agent_connection *agent_open(void);

/* Sends one JSON-RPC message (request, notification, or response).
 * Returns 0, or -1 for invalid arguments. Thread-safe. */
int agent_send(agent_connection *conn, const char *msg, size_t len);

/* Waits up to timeout_ms (negative: forever) for the next message.
 * Returns 1 and sets *out to a NUL-terminated JSON text to free with
 * agent_free, 0 on timeout, or -1 once the connection is closed and drained. */
int agent_recv(agent_connection *conn, int timeout_ms, char **out);

/* Like agent_recv, but takes every queued message at once: they are joined
 * by newlines ('\n'; JSON texts contain none) in one NUL-terminated buffer
 * of *len bytes, freed with agent_free. Fewer calls per event. */
int agent_recv_batch(agent_connection *conn, int timeout_ms, char **out, size_t *len);

/* Frees a message from agent_recv or agent_recv_batch. */
void agent_free(char *msg);

/* Closes every session and wakes agent_recv, which then drains and
 * returns -1. */
void agent_close(agent_connection *conn);

/* Frees the connection, closing it first. No thread may use it afterwards. */
void agent_destroy(agent_connection *conn);

/* The library version, such as "0.2.0". */
const char *agent_version(void);

#ifdef __cplusplus
}
#endif

#endif
