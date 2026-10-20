"""Asyncio client for agentd: JSON-RPC 2.0, one message per line on agentd's stdin and stdout."""

from __future__ import annotations

import asyncio
import json
import logging
import os
from collections.abc import AsyncIterator, Awaitable, Callable, Mapping, Sequence
from typing import Any

from .protocol import (
    PROTOCOL_VERSION,
    AgentInput,
    AgentSessionInfo,
    ErrorInfo,
    EventNotification,
    InitializeResult,
    ProviderInit,
    StartParams,
    TurnResultNotification,
    TurnResultWire,
)

__all__ = ["Agentd", "AgentdError", "Session", "TurnFailed"]

log = logging.getLogger("agentd")

LINE_LIMIT = 64 * 1024 * 1024
CANCELLED_CODE = -32800
METHOD_NOT_FOUND_CODE = -32601
LIBRARY_ERROR_CODE = -32000
# Seconds close() waits for agentd to exit after closing its stdin before killing it.
CLOSE_TIMEOUT = 5.0

NotificationCallback = Callable[[Any], None]
RequestHandler = Callable[[Any], Awaitable[Any]]


class AgentdError(Exception):
    """A JSON-RPC error from agentd, or agentd exiting. `info` is the ErrorInfo."""

    def __init__(self, code: int, message: str, info: ErrorInfo | None = None) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.info: ErrorInfo = info or {}

    @property
    def kind(self) -> str:
        return self.info.get("Kind", "")


class TurnFailed(AgentdError):
    """A turn ended without a result, or with Result.Status "failed".

    `turn` is the whole turn/result notification. `info` is the turn's
    Error, else Result.Err, else empty.
    """

    def __init__(self, turn: TurnResultNotification) -> None:
        result = turn.get("Result") or {}
        info = turn.get("Error") or result.get("Err") or {}
        message = info.get("Message") or f"turn {turn.get('TurnID', '')} failed"
        super().__init__(LIBRARY_ERROR_CODE, message, info)
        self.turn = turn


class Agentd:
    """One agentd child process. Create with `await Agentd.start(...)`."""

    init: InitializeResult

    def __init__(self, process: asyncio.subprocess.Process) -> None:
        self._process = process
        self._next_id = 0
        self._pending: dict[int, asyncio.Future[Any]] = {}
        self._listeners: dict[str, list[NotificationCallback]] = {}
        self._handlers: dict[str, RequestHandler] = {}
        self._incoming: dict[Any, asyncio.Task[None]] = {}
        self._sessions: dict[str, Session] = {}
        self._write_lock = asyncio.Lock()
        self._closed_error: AgentdError | None = None
        self._background: set[asyncio.Task[Any]] = set()
        self._reader = asyncio.create_task(self._read_loop())

    @classmethod
    async def start(
        cls,
        command: str = "agentd",
        args: Sequence[str] = (),
        providers: Mapping[str, ProviderInit] | None = None,
        include_raw: bool = False,
        env: Mapping[str, str] | None = None,
    ) -> Agentd:
        """Spawn agentd and send initialize. `providers` maps provider names to their initialize entries."""
        process = await asyncio.create_subprocess_exec(
            command,
            *args,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            env=None if env is None else {**os.environ, **env},
            limit=LINE_LIMIT,
        )
        client = cls(process)
        try:
            client.init = await client.call(
                "initialize",
                {
                    "ProtocolVersion": PROTOCOL_VERSION,
                    "Providers": dict(providers or {}),
                    "IncludeRaw": include_raw,
                },
            )
        except BaseException:
            await client.close()
            raise
        return client

    async def __aenter__(self) -> Agentd:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()

    async def call(self, method: str, params: Mapping[str, Any] | None = None) -> Any:
        """Send a request and return its result. Cancelling the awaiting task sends $/cancelRequest."""
        if self._closed_error is not None:
            raise self._closed_error
        self._next_id += 1
        request_id = self._next_id
        future: asyncio.Future[Any] = asyncio.get_running_loop().create_future()
        self._pending[request_id] = future
        try:
            await self._write({"jsonrpc": "2.0", "id": request_id, "method": method, "params": dict(params or {})})
            return await future
        except asyncio.CancelledError:
            # Cancelling the task cancels the future too, so a cancelled future means no reply yet.
            if (future.cancelled() or not future.done()) and self._closed_error is None:
                try:
                    await asyncio.shield(self.notify("$/cancelRequest", {"id": request_id}))
                except (OSError, AgentdError):
                    pass
            raise
        finally:
            self._pending.pop(request_id, None)

    async def notify(self, method: str, params: Mapping[str, Any] | None = None) -> None:
        await self._write({"jsonrpc": "2.0", "method": method, "params": dict(params or {})})

    def on(self, method: str, callback: NotificationCallback) -> Callable[[], None]:
        """Call `callback(params)` for each notification `method`. Returns an unsubscribe function."""
        self._listeners.setdefault(method, []).append(callback)

        def unsubscribe() -> None:
            callbacks = self._listeners.get(method, [])
            if callback in callbacks:
                callbacks.remove(callback)

        return unsubscribe

    def handle(self, method: str, handler: RequestHandler | None) -> None:
        """Answer agentd's reverse request `method` with `await handler(params)`; None removes it.

        A handler that raises sends an error reply, which agentd reads as deny, cancel, or no decision.
        """
        if handler is None:
            self._handlers.pop(method, None)
        else:
            self._handlers[method] = handler

    async def start_session(self, params: StartParams) -> Session:
        result = await self.call("session/start", params)
        session = Session(self, result)
        self._sessions[session.id] = session
        return session

    async def close(self) -> None:
        """Close agentd's stdin, which closes every session, and wait for it to exit.

        Kills agentd if it has not exited CLOSE_TIMEOUT seconds later.
        """
        stdin = self._process.stdin

        async def exit_gracefully() -> None:
            if stdin is not None and not stdin.is_closing():
                stdin.close()
                try:
                    await stdin.wait_closed()
                except (BrokenPipeError, ConnectionResetError):
                    pass
            await self._process.wait()

        try:
            await asyncio.wait_for(exit_gracefully(), CLOSE_TIMEOUT)
        except TimeoutError:
            self._kill()
            await self._process.wait()
        await self._reader

    def _kill(self) -> None:
        try:
            self._process.kill()
        except ProcessLookupError:
            pass

    def _call_in_background(self, method: str, params: Mapping[str, Any]) -> None:
        """Send a request without waiting for or reporting its reply."""
        task = asyncio.get_running_loop().create_task(self.call(method, params))
        self._background.add(task)

        def done(task: asyncio.Task[Any]) -> None:
            self._background.discard(task)
            if not task.cancelled():
                task.exception()

        task.add_done_callback(done)

    @property
    def returncode(self) -> int | None:
        return self._process.returncode

    async def _write(self, message: dict[str, Any]) -> None:
        if self._closed_error is not None:
            raise self._closed_error
        stdin = self._process.stdin
        assert stdin is not None
        data = json.dumps(message, separators=(",", ":")).encode() + b"\n"
        async with self._write_lock:
            try:
                stdin.write(data)
                await stdin.drain()
            except (BrokenPipeError, ConnectionResetError) as err:
                raise AgentdError(LIBRARY_ERROR_CODE, f"agentd: {err}", {"Kind": "closed", "Message": str(err)}) from err

    async def _read_loop(self) -> None:
        stdout = self._process.stdout
        assert stdout is not None
        reason = "agentd exited"
        try:
            while line := await stdout.readline():
                try:
                    message = json.loads(line)
                except ValueError:
                    log.warning("agentd sent a line that is not JSON: %.200r", line)
                    continue
                if not isinstance(message, dict):
                    log.warning("agentd sent a line that is not a JSON object: %.200r", line)
                    continue
                try:
                    self._dispatch(message)
                except Exception:
                    log.exception("agentd sent a message that could not be handled: %.200r", line)
        except (ValueError, asyncio.LimitOverrunError) as err:
            # The stream cannot be framed any more, so agentd is unusable and its stdout would go undrained.
            reason = f"agentd: {err}"
            self._kill()
        finally:
            self._shut_down(reason)

    def _shut_down(self, reason: str) -> None:
        self._closed_error = AgentdError(LIBRARY_ERROR_CODE, reason, {"Kind": "closed", "Message": reason})
        for future in self._pending.values():
            if not future.done():
                future.set_exception(self._closed_error)
        for task in self._incoming.values():
            task.cancel()
        for session in list(self._sessions.values()):
            session._closed(self._closed_error)

    def _dispatch(self, message: dict[str, Any]) -> None:
        method = message.get("method")
        if method is None:
            future = self._pending.get(message.get("id"))  # type: ignore[arg-type]
            if future is None or future.done():
                return
            error = message.get("error")
            if error is not None:
                future.set_exception(AgentdError(error.get("code", 0), error.get("message", ""), error.get("data")))
            else:
                future.set_result(message.get("result"))
        elif "id" in message:
            request_id = message["id"]
            self._incoming[request_id] = asyncio.create_task(self._answer(request_id, method, message.get("params")))
        else:
            self._notified(method, message.get("params"))

    def _notified(self, method: str, params: Any) -> None:
        if method == "$/cancelRequest":
            task = self._incoming.get(params.get("id"))
            if task is not None:
                task.cancel()
        elif method in ("session/event", "turn/result", "session/closed"):
            session = self._sessions.get(params.get("Session"))
            if session is not None:
                session._notified(method, params)
                if method == "session/closed":
                    self._sessions.pop(session.id, None)
        for callback in list(self._listeners.get(method, ())):
            try:
                callback(params)
            except Exception:
                log.exception("agentd: %s listener failed", method)

    async def _answer(self, request_id: Any, method: str, params: Any) -> None:
        reply: dict[str, Any] = {"jsonrpc": "2.0", "id": request_id}
        handler = self._handlers.get(method)
        try:
            if handler is None:
                message = f"no handler for {method}"
                reply["error"] = {"code": METHOD_NOT_FOUND_CODE, "message": message, "data": {"Kind": "unknown_method", "Message": message}}
            else:
                try:
                    reply["result"] = await handler(params)
                except asyncio.CancelledError:
                    if self._closed_error is not None:
                        return
                    reply["error"] = {"code": CANCELLED_CODE, "message": "cancelled", "data": {"Kind": "cancelled", "Message": "cancelled"}}
                except Exception as err:
                    reply["error"] = {"code": LIBRARY_ERROR_CODE, "message": str(err) or type(err).__name__, "data": {"Message": str(err)}}
            try:
                await self._write(reply)
            except AgentdError:
                pass
        finally:
            self._incoming.pop(request_id, None)


_END = object()


class Session:
    """A session started with `Agentd.start_session`."""

    def __init__(self, client: Agentd, start: Mapping[str, Any]) -> None:
        self._client = client
        self.id: str = start["Session"]
        self.info: AgentSessionInfo = start.get("Info") or {}
        self.methods: list[str] = start.get("Methods") or []
        self._queues: list[asyncio.Queue[Any]] = []
        # Futures of runs waiting for their turn/result, by TurnID.
        self._waiters: dict[str, asyncio.Future[TurnResultNotification]] = {}
        # turn/result notifications that arrive while a run waits for session/send's reply.
        self._early: dict[str, TurnResultNotification] = {}
        self._sending = 0
        self._end: AgentdError | None = None

    def events(self) -> AsyncIterator[EventNotification]:
        """Return an iterator over this session's events from this call until it closes, in Seq order.

        It subscribes when called, so events after `it = session.events()` are kept for it even before
        iteration starts. Close the iterator (`aclose()`) to unsubscribe early.
        """
        queue: asyncio.Queue[Any] = asyncio.Queue()
        if self._end is None:
            self._queues.append(queue)
        else:
            queue.put_nowait(_END)
        return self._iterate(queue)

    async def _iterate(self, queue: asyncio.Queue[Any]) -> AsyncIterator[EventNotification]:
        try:
            while (item := await queue.get()) is not _END:
                yield item
        finally:
            if queue in self._queues:
                self._queues.remove(queue)

    async def send(self, input: AgentInput | str, provider_options: Any = None) -> str:
        """Start a turn and return its TurnID; events and turn/result follow."""
        params: dict[str, Any] = {"Session": self.id, "Input": _input(input)}
        if provider_options is not None:
            params["ProviderOptions"] = provider_options
        result = await self._client.call("session/send", params)
        return result["TurnID"]

    async def run(self, input: AgentInput | str, provider_options: Any = None) -> TurnResultWire:
        """Send a turn and return its Result.

        Raises TurnFailed when the turn/result has an Error, has no Result,
        or has Result.Status "failed". An interrupted turn returns its Result.
        Raises AgentdError with kind "closed" when the session closes or agentd
        exits before the turn's result arrives. Cancelling the awaiting task
        after the turn started sends session/interruptTurn without waiting for
        its reply, then raises CancelledError.
        """
        self._sending += 1
        try:
            turn_id = await self.send(input, provider_options)
        finally:
            self._sending -= 1
        turn = self._early.pop(turn_id, None)
        if self._sending == 0:
            self._early.clear()
        if turn is None:
            if self._end is not None:
                raise self._end
            future: asyncio.Future[TurnResultNotification] = asyncio.get_running_loop().create_future()
            self._waiters[turn_id] = future
            try:
                turn = await future
            except asyncio.CancelledError:
                if future.cancelled() and self._end is None:
                    self._client._call_in_background("session/interruptTurn", {"Session": self.id, "TurnID": turn_id})
                raise
            finally:
                self._waiters.pop(turn_id, None)
        result = turn.get("Result")
        if turn.get("Error") or result is None or result.get("Status") == "failed":
            raise TurnFailed(turn)
        return result

    async def interrupt(self) -> None:
        await self._client.call("session/interrupt", {"Session": self.id})

    async def close(self) -> None:
        await self._client.call("session/close", {"Session": self.id})
        self._client._sessions.pop(self.id, None)
        self._closed(AgentdError(LIBRARY_ERROR_CODE, "session closed", {"Kind": "closed", "Message": "session closed"}))

    def _notified(self, method: str, params: Any) -> None:
        if method == "session/event":
            for queue in self._queues:
                queue.put_nowait(params)
        elif method == "turn/result":
            turn_id = params.get("TurnID", "")
            future = self._waiters.get(turn_id)
            if future is not None:
                if not future.done():
                    future.set_result(params)
            elif self._sending:
                # turn/result can arrive before run() reads session/send's reply.
                self._early[turn_id] = params
        elif method == "session/closed":
            error = params.get("Error") or {}
            message = error.get("Message") or "session closed"
            self._closed(AgentdError(LIBRARY_ERROR_CODE, message, {**error, "Kind": "closed", "Message": message}))

    def _closed(self, error: AgentdError) -> None:
        if self._end is not None:
            return
        self._end = error
        for queue in self._queues:
            queue.put_nowait(_END)
        self._queues.clear()
        for future in self._waiters.values():
            if not future.done():
                future.set_exception(error)


def _input(input: AgentInput | str) -> AgentInput:
    if isinstance(input, str):
        return {"Parts": [{"Type": "text", "Text": input}]}
    return input
