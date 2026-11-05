#!/usr/bin/env python3
"""Small, hermetic P0 acceptance run for a built Friday binary.

Only Python's standard library is used.  The fake OpenAI-compatible endpoint is
bound to loopback, data is written to a temporary directory, and every child
process is terminated in the finally block.  The script deliberately prints a
short result rather than model responses or configuration secrets.
"""
from __future__ import annotations

import argparse
import http.client
import json
import os
import shutil
import signal
import socket
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


class Failure(RuntimeError):
    pass


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


class FakeModel(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args):
        return

    def do_POST(self):  # noqa: N802
        size = int(self.headers.get("Content-Length", "0"))
        payload = self.rfile.read(size)
        if self.path != "/v1/chat/completions":
            self.send_error(404)
            return
        if self.headers.get("Authorization") != "Bearer smoke-key":
            self._json(401, {"error": {"message": "invalid api key"}})
            return
        try:
            body = json.loads(payload)
        except json.JSONDecodeError:
            self._json(400, {"error": {"message": "invalid json"}})
            return
        messages = body.get("messages") or []
        last = messages[-1] if messages else {}
        content = last.get("content", "")
        if isinstance(content, list):
            content = " ".join(str(part.get("text", "")) for part in content if isinstance(part, dict))
        # A deterministic response is enough for the chat/tool smoke.  Returning
        # a normal assistant response exercises the full configured provider,
        # SSE decoder, persistence, and UI-facing done marker without invoking
        # an external model or guessing provider tool-call dialects.
        text = "smoke-ok"
        wants_time = "时间" in str(content) or "几点" in str(content)
        has_tool_result = any(message.get("role") == "tool" for message in messages if isinstance(message, dict))
        if has_tool_result:
            with self.server.stats_lock:
                self.server.tool_results += 1
        if wants_time and not has_tool_result and body.get("tools"):
            with self.server.stats_lock:
                self.server.tool_calls += 1
            tool = {"index": 0, "id": "call_smoke_time", "type": "function",
                    "function": {"name": "get_time", "arguments": "{}"}}
            raw = json.dumps({"choices": [{"delta": {"tool_calls": [tool]}, "finish_reason": "tool_calls"}],
                               "usage": None}, ensure_ascii=False)
            data = f"data: {raw}\n\ndata: [DONE]\n\n".encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        if wants_time:
            text = "当前时间工具链路已返回 smoke-ok"
        if body.get("stream"):
            raw = json.dumps({"choices": [{"delta": {"content": text}, "finish_reason": None}]}, ensure_ascii=False)
            data = f"data: {raw}\n\ndata: [DONE]\n\n".encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        self._json(200, {"choices": [{"message": {"role": "assistant", "content": text}}]})

    def _json(self, status, value):
        data = json.dumps(value, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def start_fake() -> tuple[ThreadingHTTPServer, int]:
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeModel)
    server.tool_calls = 0
    server.tool_results = 0
    server.stats_lock = threading.Lock()
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, int(server.server_address[1])


class API:
    def __init__(self, port: int):
        self.port = port
        self.token = ""
        self.cookie = ""

    def headers(self, content_type=False):
        # 登录态是 JWT：/api/setup 与 /api/login 在响应体里回 token，之后每个请求
        # 带 Authorization: Bearer（cookie 早就不发了，留着只是给旧形态兜底）。
        headers = {"Accept": "application/json", "Origin": f"http://127.0.0.1:{self.port}"}
        if content_type:
            headers["Content-Type"] = "application/json"
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"
        if self.cookie:
            headers["Cookie"] = self.cookie
        return headers

    def request(self, method, path, body=None, expected=None, timeout=4):
        payload = None if body is None else json.dumps(body, ensure_ascii=False).encode()
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=timeout)
        headers = self.headers(content_type=payload is not None)
        try:
            conn.request(method, path, payload, headers)
            response = conn.getresponse()
            data = response.read()
            status = response.status
            cookies = response.getheader("Set-Cookie")
        finally:
            conn.close()
        if cookies:
            self.cookie = cookies.split(";", 1)[0]
        if expected is not None and status not in expected:
            raise Failure(f"{method} {path} returned HTTP {status}")
        try:
            value = json.loads(data) if data else {}
        except json.JSONDecodeError as exc:
            raise Failure(f"{method} {path} returned invalid JSON") from exc
        if status >= 400:
            return status, value
        unwrapped = value.get("body", value) if isinstance(value, dict) else value
        if isinstance(unwrapped, dict) and unwrapped.get("token"):
            self.token = unwrapped["token"]
        return status, unwrapped

    def json(self, method, path, body=None, expected=(200,)):
        status, value = self.request(method, path, body, expected)
        return value


def wait_ready(binary, data_dir: Path, log_path: Path) -> tuple[subprocess.Popen, API]:
    port = free_port()
    # Ordinary server mode, isolated cwd, and a pipe kept open for the REPL.
    # Never inherit deployment keys, account overrides, or notification config.
    env = {key: value for key, value in os.environ.items() if not key.startswith("FRIDAY_")}
    with log_path.open("w", encoding="utf-8") as log:
        process = subprocess.Popen(
            [binary, "--config", str(data_dir / "config.yaml"), "--host", "127.0.0.1", "--port", str(port)],
            cwd=str(data_dir), env=env, stdin=subprocess.PIPE, stdout=log, stderr=subprocess.STDOUT,
            text=True,
        )
    deadline = time.monotonic() + 10
    api = API(port)
    try:
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise Failure(f"Friday exited before ready ({process.returncode})")
            try:
                api.request("GET", "/api/setup/status", expected=(200,), timeout=0.5)
                return process, api
            except (OSError, TimeoutError):
                pass
            time.sleep(0.05)
        raise Failure("Friday API did not become ready within 10 seconds")
    except BaseException:
        stop_process(process)
        raise


def stop_process(process):
    if process is None:
        return
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=4)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=2)
    if process.stdin:
        process.stdin.close()


def await_value(query, predicate, message, timeout=10):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = query()
        if predicate(result):
            return result
        time.sleep(0.05)
    raise Failure(message)


def sse_chat(api: API, message: str, attachments=None, session="web") -> str:
    body = {"message": message, "session": session}
    if attachments:
        body["attachments"] = attachments
    payload = json.dumps(body, ensure_ascii=False).encode()
    conn = http.client.HTTPConnection("127.0.0.1", api.port, timeout=8)
    headers = api.headers(content_type=True)
    conn.request("POST", "/api/chat", payload, headers)
    response = conn.getresponse()
    if response.status != 200:
        conn.close()
        raise Failure(f"chat returned HTTP {response.status}")
    raw = response.read().decode("utf-8", "replace")
    conn.close()
    chunks = []
    done = False
    for line in raw.splitlines():
        if not line.startswith("data:"):
            continue
        try:
            event = json.loads(line[5:].strip())
        except json.JSONDecodeError:
            continue
        if event.get("error"):
            raise Failure("chat SSE returned an error")
        if event.get("delta"):
            chunks.append(event["delta"])
        done = done or bool(event.get("done"))
    if not done:
        raise Failure("chat SSE did not finish")
    return "".join(chunks)


def expect(condition, message):
    if not condition:
        raise Failure(message)


def run(binary: str, output_dir: str | None) -> dict:
    fake, fake_port = start_fake()
    parent = Path(output_dir).resolve() if output_dir else None
    if parent:
        parent.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="friday-p0-", dir=str(parent) if parent else None))
    data_dir = root / "data"
    data_dir.mkdir()
    log_path = root / "server.log"
    process = None
    result = {"status": "PASS", "checks": []}
    if output_dir:
        result["artifacts"] = str(root)
    try:
        process, api = wait_ready(binary, data_dir, log_path)
        status = api.json("GET", "/api/setup/status")
        expect(status.get("accountRequired") is True, "first boot did not require account setup")
        result["checks"].append("firstboot")

        api.json("POST", "/api/setup", {"username": "smoke", "password": "smoke-password"})
        expect(api.json("GET", "/api/session").get("authenticated") is True, "setup did not create session")
        result["checks"].append("setup")

        base = f"http://127.0.0.1:{fake_port}/v1"
        test = api.json("POST", "/api/provider/test", {"baseURL": base, "model": "smoke-model", "apiKey": "smoke-key", "checkVision": True})
        expect(test.get("ok") is True and test.get("text", {}).get("status") == "supported", "provider text test failed")
        expect(test.get("vision", {}).get("status") == "supported", "provider vision test failed")
        bad_status, bad_test = api.request("POST", "/api/provider/test", {"baseURL": base, "model": "smoke-model", "apiKey": "bad-key"}, expected=(200,))
        expect(bad_status == 200 and bad_test.get("ok") is False, "invalid provider credentials were accepted")
        api.json("POST", "/api/provider", {"baseURL": base, "model": "smoke-model", "apiKey": "smoke-key"})
        result["checks"].append("provider")

        reply = sse_chat(api, "请调用时间工具后告诉我现在几点")
        expect(reply, "chat returned no streamed text")
        expect(fake.tool_calls == 1 and fake.tool_results == 1, "chat did not execute get_time tool")
        result["checks"].append("chat-sse")

        # Life mode keeps the single main session; work mode can add independent
        # sessions that share nothing but the same assistant behind them.
        sessions = api.json("GET", "/api/sessions").get("sessions", [])
        expect(any(item.get("key") == "web" and item.get("kind") == "life" for item in sessions),
               "main session missing from the session list")
        work_key = api.json("POST", "/api/sessions", {"title": "smoke work session"}).get("session", {}).get("key", "")
        expect(work_key.startswith("work-"), "created session key lacks the work- prefix")
        expect(sse_chat(api, "note this for the smoke run", session=work_key), "work session chat returned no text")
        expect(api.json("GET", f"/api/chat/history?session={work_key}").get("messages"),
               "work session history is not readable")
        sessions = api.json("GET", "/api/sessions").get("sessions", [])
        entry = next((item for item in sessions if item.get("key") == work_key), {})
        expect(entry.get("kind") == "work" and entry.get("messages", 0) >= 2,
               "work session is not listed with its messages")
        main_texts = [item.get("text", "") for item in api.json("GET", "/api/chat/history?session=web").get("messages", [])]
        expect(not any("note this for the smoke run" in text for text in main_texts),
               "work session leaked into the main session")
        status, body = api.request("POST", "/api/chat", {"message": "x", "session": "work-unregistered"}, expected=(400,))
        expect(status == 400 and (body.get("body", body).get("error") or ""),
               "an unregistered session key was accepted by chat")
        api.json("POST", f"/api/sessions/{work_key}", {"title": "renamed smoke session"})
        keep_key = api.json("POST", "/api/sessions", {}).get("session", {}).get("key", "")
        drop_key = api.json("POST", "/api/sessions", {}).get("session", {}).get("key", "")
        api.json("DELETE", f"/api/sessions/{drop_key}")
        sessions = api.json("GET", "/api/sessions").get("sessions", [])
        expect(any(item.get("key") == keep_key and item.get("title") == "新会话" for item in sessions),
               "an unnamed session should fall back to the placeholder title")
        expect(not any(item.get("key") == drop_key for item in sessions), "deleted session is still listed")
        status, body = api.request("DELETE", "/api/sessions/web", expected=(400,))
        expect(status == 400, "the main session was deletable")
        result["checks"].append("chat-sessions")

        task = api.json("POST", "/api/tasks", {"text": "smoke task", "in": "60s", "status": "paused"})
        tasks = task.get("tasks", [])
        expect(tasks, "task creation returned no task")
        task_id = tasks[-1]["id"]
        api.json("POST", f"/api/tasks/{task_id}/pause", {})
        paused_task_id = task_id
        api.json("PUT", f"/api/tasks/{task_id}", {"text": "smoke task updated", "when": tasks[-1]["when"], "status": "paused"})
        api.json("POST", f"/api/tasks/{task_id}/resume", {})
        api.json("POST", f"/api/tasks/{task_id}/pause", {})
        result["checks"].append("task-lifecycle")

        now = time.time()
        due = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now + 90))
        remind = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now + 60))
        todo = api.json("POST", "/api/entries", {"text": "smoke deadline", "dueAt": due, "remindAt": remind})
        todo_id = todo.get("entry", {}).get("id")
        expect(todo_id, "todo creation failed")
        linked_id = todo.get("entry", {}).get("reminderTaskId")
        expect(any(item.get("id") == linked_id and item.get("todoId") == todo_id for item in api.json("GET", "/api/tasks").get("tasks", [])), "todo did not create linked future reminder")
        api.json("POST", f"/api/entries/{todo_id}", {"done": True})
        expect(not any(item.get("id") == linked_id for item in api.json("GET", "/api/tasks").get("tasks", [])), "completion did not cancel future reminder")
        # A normal web reminder is snoozable; linked Todo reminders are
        # intentionally controlled from the Todo page and reject snooze.
        api.json("POST", "/api/tasks", {"text": "standalone smoke reminder", "in": "1s", "channel": "web"})
        notices = await_value(lambda: api.json("GET", "/api/notifications").get("notifications", []),
                              lambda items: any(item.get("text") == "standalone smoke reminder" and item.get("status") == "delivered" for item in items),
                              "reminder notification was not delivered and persisted")
        notice = next(item for item in notices if item.get("text") == "standalone smoke reminder")
        expect("web" in notice.get("deliveredTo", []), "notification did not reach web center")
        api.json("POST", f"/api/notifications/{notice['id']}/read", {})
        snooze = api.json("POST", f"/api/notifications/{notice['id']}/snooze", {"minutes": 1}).get("task", {})
        expect(snooze.get("notificationParentId") == notice["id"] and snooze.get("channel") == "web", "snooze lost parent or channel")
        result["checks"].append("todo-notification")

        stop_process(process)
        process = None
        process2, api2 = wait_ready(binary, data_dir, root / "server-restart.log")
        process = process2
        api2.json("GET", "/api/setup/status")
        # A fresh login is required after restart; persisted files are checked
        # through the real authenticated API rather than by reading secrets.
        api2.json("POST", "/api/login", {"username": "smoke", "password": "smoke-password"})
        restored = api2.json("GET", "/api/entries").get("entries", [])
        expect(any(item.get("id") == todo_id and item.get("done") for item in restored), "completed todo was not restored")
        restored_tasks = api2.json("GET", "/api/tasks").get("tasks", [])
        expect(any(item.get("id") == paused_task_id and item.get("status") == "paused" for item in restored_tasks), "paused task was not restored")
        restored_sessions = api2.json("GET", "/api/sessions").get("sessions", [])
        renamed = next((item for item in restored_sessions if item.get("key") == work_key), {})
        expect(renamed.get("title") == "renamed smoke session", "renamed work session was not restored")
        expect(any(item.get("key") == keep_key for item in restored_sessions), "idle work session was not restored")
        expect(not any(item.get("key") == drop_key for item in restored_sessions), "deleted session came back after restart")
        restored_notice = api2.json("GET", "/api/notifications").get("notifications", [])
        persisted = next((item for item in restored_notice if item.get("id") == notice["id"]), None)
        expect(persisted is not None and persisted.get("readAt"), "notification read state was not restored")
        expect(any(item.get("id") == snooze.get("id") and item.get("notificationParentId") == notice["id"] for item in restored_tasks), "snooze task was not restored")
        result["checks"].append("restart-persistence")
        stop_process(process)
        process = None
        # Build a historical profile from this test's own valid account JSON;
        # no user data is read, copied, or modified.
        legacy = root / "legacy"
        legacy.mkdir()
        shutil.copy2(data_dir / "config.yaml", legacy / "config.yaml")
        old_account = json.loads((data_dir / "admin-account.json").read_text())
        old_account.pop("setupRequired", None)
        old_account["mustChange"] = True
        (legacy / "admin-account.json").write_text(json.dumps(old_account))
        (legacy / "todos.json").write_text(json.dumps([{"id": "legacy-todo", "text": "legacy todo", "done": False}]))
        process, old_api = wait_ready(binary, legacy, root / "server-legacy.log")
        expect(old_api.json("GET", "/api/setup/status").get("accountRequired") is False, "old account format forced setup")
        old_api.json("POST", "/api/login", {"username": "smoke", "password": "smoke-password"})
        expect(any(item.get("id") == "legacy-todo" for item in old_api.json("GET", "/api/entries").get("entries", [])), "old todo JSON was not compatible")
        result["checks"].append("legacy-profile")
        return result
    finally:
        stop_process(process)
        fake.shutdown()
        fake.server_close()
        if output_dir is None:
            shutil.rmtree(root, ignore_errors=True)


def main() -> int:
    parser = argparse.ArgumentParser(description="Run Friday's local P0 smoke test")
    parser.add_argument("--binary", required=True, help="absolute path to the Friday binary")
    parser.add_argument("--output-dir", help="retain smoke data and short logs in this directory")
    args = parser.parse_args()
    binary = os.path.abspath(args.binary)
    if not os.path.isfile(binary) or not os.access(binary, os.X_OK):
        print(json.dumps({"status": "FAIL", "error": "binary is not executable"}))
        return 2
    try:
        result = run(binary, args.output_dir)
    except Exception as exc:  # noqa: BLE001
        print(json.dumps({"status": "FAIL", "error": str(exc)}, ensure_ascii=False))
        return 1
    print(json.dumps(result, ensure_ascii=False, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
