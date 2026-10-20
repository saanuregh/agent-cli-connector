import asyncio
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from agentd import Agentd, AgentdError, TurnFailed  # noqa: E402
from agentd import client as client_module  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parents[3]
TIMEOUT = 10


class AgentdTest(unittest.IsolatedAsyncioTestCase):
    @classmethod
    def setUpClass(cls):
        cls.build_dir = tempfile.mkdtemp(prefix="fakeagentd-")
        cls.fakeagentd = str(Path(cls.build_dir) / "fakeagentd")
        subprocess.run(["go", "build", "-o", cls.fakeagentd, "./internal/agentd/fakeagentd"], cwd=REPO_ROOT, check=True)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.build_dir, ignore_errors=True)

    async def asyncSetUp(self):
        self.agentd = await asyncio.wait_for(Agentd.start(self.fakeagentd, providers={"fake": {}}), TIMEOUT)

    async def asyncTearDown(self):
        await asyncio.wait_for(self.agentd.close(), TIMEOUT)

    async def wait(self, awaitable):
        return await asyncio.wait_for(awaitable, TIMEOUT)

    async def test_initialize_and_ping(self):
        self.assertEqual(self.agentd.init["ProtocolVersion"], 1)
        self.assertEqual([p["Name"] for p in self.agentd.init["Providers"]], ["fake"])
        self.assertEqual(await self.wait(self.agentd.call("$/ping")), {})

    async def test_run_echo(self):
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))
        result = await self.wait(session.run("echo hi"))
        self.assertEqual(result["Text"], "echo: hi")
        self.assertEqual(result["Status"], "completed")

    async def test_events_in_order_before_turn_result(self):
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))
        # Callbacks run as each line is read, so the log is the wire order.
        log = []
        self.agentd.on("session/event", lambda e: log.append(("event", e["Seq"], e["Type"])))
        self.agentd.on("turn/result", lambda params: log.append(("result", params["TurnID"], None)))
        iterated = []

        async def collect():
            async for event in session.events():
                iterated.append(event["Type"])

        collector = asyncio.create_task(collect())
        turn_id = await self.wait(session.send("echo hi"))
        await self.wait(self._until(lambda: ("result", turn_id, None) in log))
        await self.wait(session.close())
        await self.wait(collector)
        result_at = log.index(("result", turn_id, None))
        seqs = [seq for kind, seq, _ in log if kind == "event"]
        self.assertEqual(seqs, sorted(seqs))
        # The turn's events, TurnCompleted last, come before turn/result;
        # session events such as going idle may follow it.
        self.assertIn("TurnCompleted", [t for kind, _, t in log[:result_at] if kind == "event"])
        self.assertIn("TurnCompleted", iterated)

    async def test_permission_handler(self):
        requests = []

        async def allow(params):
            requests.append(params)
            return {"Decision": "allow"}

        self.agentd.handle("handler/permission", allow)
        session = await self.wait(self.agentd.start_session({"Provider": "fake", "Handlers": {"Permission": True}}))
        result = await self.wait(session.run("permission"))
        self.assertEqual(result["Text"], "allow")
        self.assertEqual(requests[0]["Request"]["Command"], "ls")

    async def test_undeclared_permission_is_an_event(self):
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))

        async def answer():
            async for event in session.events():
                if event["Type"] == "PermissionRequest":
                    await self.agentd.call(
                        "request/respond",
                        {"Session": session.id, "RequestID": event["Event"]["ID"], "Permission": {"Decision": "deny"}},
                    )
                    return

        answering = asyncio.create_task(answer())
        await asyncio.sleep(0)
        result = await self.wait(session.run("permission"))
        await self.wait(answering)
        self.assertEqual(result["Text"], "deny")

    async def test_host_tool(self):
        calls = []

        async def tool_call(params):
            calls.append(params)
            return {"Content": [{"Type": "text", "Text": "got " + json.dumps(params["Call"]["Arguments"])}]}

        self.agentd.handle("tool/call", tool_call)
        session = await self.wait(
            self.agentd.start_session({"Provider": "fake", "Config": {"Tools": [{"Name": "lookup", "Description": "d"}]}})
        )
        result = await self.wait(session.run('tool lookup {"q":1}'))
        self.assertEqual(result["Text"], 'got {"q": 1}')
        self.assertEqual(calls[0]["Tool"], "lookup")

    async def test_unhandled_tool_call_fails_turn(self):
        session = await self.wait(
            self.agentd.start_session({"Provider": "fake", "Config": {"Tools": [{"Name": "lookup"}]}})
        )
        with self.assertRaises(TurnFailed):
            await self.wait(session.run("tool lookup {}"))

    async def test_failed_turn_raises(self):
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))
        with self.assertRaises(TurnFailed) as caught:
            await self.wait(session.run("fail"))
        self.assertIn("scripted failure", str(caught.exception))
        self.assertIsNotNone(caught.exception.turn)

    async def test_interrupted_turn_returns_result(self):
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))
        running = asyncio.create_task(session.run("wait"))
        await asyncio.sleep(0.2)
        await self.wait(session.interrupt())
        result = await self.wait(running)
        self.assertEqual(result["Status"], "interrupted")

    async def test_cancel_sends_cancel_request(self):
        written = []
        original = self.agentd._write

        async def spy(message):
            written.append(message)
            await original(message)
            if message.get("method") == "session/info":
                # Cancel before the reply can arrive, so the wait is cancelled, not answered.
                asyncio.current_task().cancel()

        self.agentd._write = spy
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))
        pending = asyncio.create_task(self.agentd.call("session/info", {"Session": session.id}))
        with self.assertRaises(asyncio.CancelledError):
            await self.wait(pending)
        cancels = [m for m in written if m.get("method") == "$/cancelRequest"]
        info_ids = [m["id"] for m in written if m.get("method") == "session/info"]
        self.assertEqual([c["params"]["id"] for c in cancels], info_ids)
        self.assertEqual(await self.wait(self.agentd.call("$/ping")), {})
        self.assertEqual(self.agentd._pending, {})

    async def test_reverse_request_cancelled_by_agentd(self):
        started = asyncio.Event()
        cancelled = asyncio.Event()

        async def slow(params):
            started.set()
            try:
                await asyncio.sleep(60)
            except asyncio.CancelledError:
                cancelled.set()
                raise

        self.agentd.handle("handler/permission", slow)
        session = await self.wait(self.agentd.start_session({"Provider": "fake", "Handlers": {"Permission": True}}))
        running = asyncio.create_task(session.run("permission"))
        await self.wait(started.wait())
        await self.wait(session.interrupt())
        await self.wait(cancelled.wait())
        try:
            await self.wait(running)
        except TurnFailed:
            pass

    async def test_unknown_session(self):
        with self.assertRaises(AgentdError) as caught:
            await self.wait(self.agentd.call("session/info", {"Session": "nope"}))
        self.assertEqual(caught.exception.info["Kind"], "not_found")
        self.assertEqual(caught.exception.kind, "not_found")

    async def test_agentd_exit_fails_pending_calls(self):
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))
        running = asyncio.create_task(session.run("wait"))
        await asyncio.sleep(0.2)
        self.agentd._process.kill()
        with self.assertRaises(AgentdError) as caught:
            await self.wait(running)
        self.assertEqual(caught.exception.kind, "closed")
        with self.assertRaises(AgentdError):
            await self.wait(self.agentd.call("$/ping"))

    async def test_events_subscribe_when_called(self):
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))
        results = []
        self.agentd.on("turn/result", results.append)
        events = session.events()
        turn_id = await self.wait(session.send("echo hi"))
        await self.wait(self._until(lambda: any(r["TurnID"] == turn_id for r in results)))
        seen = []

        async def collect():
            async for event in events:
                seen.append(event["Type"])
                if event["Type"] == "TurnCompleted":
                    return

        await self.wait(collect())
        self.assertIn("TurnCompleted", seen)

    async def test_closing_session_ends_running_turn(self):
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))
        running = asyncio.create_task(session.run("wait"))
        await asyncio.sleep(0.2)
        await self.wait(session.close())
        try:
            await asyncio.wait_for(running, 2)
        except AgentdError:
            pass

    async def test_cancelling_run_interrupts_turn(self):
        written = []
        original = self.agentd._write

        async def spy(message):
            written.append(message)
            await original(message)

        self.agentd._write = spy
        results = []
        self.agentd.on("turn/result", results.append)
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))
        running = asyncio.create_task(session.run("wait"))
        await asyncio.sleep(0.2)
        running.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await self.wait(running)
        await self.wait(self._until(lambda: any(r["Result"] and r["Result"]["Status"] == "interrupted" for r in results)))
        interrupts = [m["params"] for m in written if m.get("method") == "session/interruptTurn"]
        self.assertEqual([i["TurnID"] for i in interrupts], [results[0]["TurnID"]])

    async def test_send_result_during_run_is_not_kept(self):
        session = await self.wait(self.agentd.start_session({"Provider": "fake"}))
        results = []
        self.agentd.on("turn/result", results.append)
        echo = await self.wait(session.send("echo hi"))
        running = asyncio.create_task(session.run("wait"))
        await self.wait(self._until(lambda: any(r["TurnID"] == echo for r in results)))
        await asyncio.sleep(0.2)
        await self.wait(session.interrupt())
        await self.wait(running)
        self.assertEqual(getattr(session, "_turns", {}), {})
        self.assertEqual(getattr(session, "_early", {}), {})
        self.assertEqual(getattr(session, "_waiters", {}), {})

    async def _until(self, condition):
        while not condition():
            await asyncio.sleep(0.01)


# A scripted agentd for wire conditions fakeagentd cannot produce. Its first argument is the mode.
SCRIPTED_AGENTD = r"""
import json, sys, time
mode = sys.argv[1]
out = sys.stdout.buffer

def send(*messages):
    out.write(b"".join(json.dumps(m).encode() + b"\n" for m in messages))
    out.flush()

for line in sys.stdin.buffer:
    message = json.loads(line)
    method, request_id = message.get("method"), message.get("id")
    if request_id is None:
        continue
    reply = {"jsonrpc": "2.0", "id": request_id, "result": {}}
    if method == "initialize":
        reply["result"] = {"ProtocolVersion": 1, "Providers": []}
    elif method == "session/start":
        reply["result"] = {"Session": "s1", "Info": {}, "Methods": []}
    elif method == "session/send":
        reply["result"] = {"TurnID": "t1"}
        if mode == "closed-with-reply":
            send(reply, {"jsonrpc": "2.0", "method": "session/closed", "params": {"Session": "s1", "Error": None}})
            continue
    elif method == "$/ping" and mode == "not-object":
        out.write(b'[1,2]\n"text"\nnull\n{"jsonrpc":"2.0","method":"session/event","params":7}\n')
    elif method == "$/ping" and mode == "long-line":
        out.write(b'"' + b"x" * (2 * 1024 * 1024) + b'"\n')
        out.flush()
        continue
    send(reply)
if mode in ("ignore-eof", "long-line"):
    time.sleep(60)
"""


class ScriptedAgentdTest(unittest.IsolatedAsyncioTestCase):
    @classmethod
    def setUpClass(cls):
        cls.script_dir = tempfile.mkdtemp(prefix="scripted-agentd-")
        cls.script = str(Path(cls.script_dir) / "scripted_agentd.py")
        Path(cls.script).write_text(SCRIPTED_AGENTD)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.script_dir, ignore_errors=True)

    async def start(self, mode):
        agentd = await asyncio.wait_for(Agentd.start(sys.executable, ["-I", self.script, mode]), TIMEOUT)

        async def close():
            await asyncio.wait_for(agentd.close(), TIMEOUT)

        self.addAsyncCleanup(close)
        return agentd

    async def test_run_fails_when_session_closes_with_send_reply(self):
        agentd = await self.start("closed-with-reply")
        session = await asyncio.wait_for(agentd.start_session({"Provider": "fake"}), TIMEOUT)
        with self.assertRaises(AgentdError) as caught:
            await asyncio.wait_for(session.run("hi"), 2)
        self.assertEqual(caught.exception.kind, "closed")
        with self.assertRaises(AgentdError):
            await asyncio.wait_for(session.run("again"), 2)

    async def test_non_object_lines_are_ignored(self):
        agentd = await self.start("not-object")
        with self.assertLogs("agentd", "WARNING"):
            self.assertEqual(await asyncio.wait_for(agentd.call("$/ping"), TIMEOUT), {})
            self.assertEqual(await asyncio.wait_for(agentd.call("$/ping"), TIMEOUT), {})

    async def test_over_long_line_fails_calls_and_kills_agentd(self):
        with mock.patch.object(client_module, "LINE_LIMIT", 1024 * 1024):
            agentd = await self.start("long-line")
        with self.assertRaises(AgentdError) as caught:
            await asyncio.wait_for(agentd.call("$/ping"), TIMEOUT)
        self.assertEqual(caught.exception.kind, "closed")
        await asyncio.wait_for(agentd.close(), 3)
        self.assertIsNotNone(agentd.returncode)

    async def test_close_kills_agentd_that_does_not_exit(self):
        agentd = await self.start("ignore-eof")
        with mock.patch.object(client_module, "CLOSE_TIMEOUT", 0.5, create=True):
            await asyncio.wait_for(agentd.close(), 3)
        self.assertIsNotNone(agentd.returncode)


if __name__ == "__main__":
    unittest.main()
