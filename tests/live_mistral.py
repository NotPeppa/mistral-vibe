#!/usr/bin/env python3
"""Drive the built plugin against the REAL Mistral API, when a key is present.

Checks the two things plan B promises: the plan's month reads back, and the key
can be published to CPA's AI-provider list in the entry shape CPA accepts.

    MISTRAL_API_KEY=mstrl_... python3 tests/live_mistral.py
"""

from __future__ import annotations

import base64
import ctypes
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


class HostAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("host_ctx", ctypes.c_void_p),
                ("call", ctypes.c_void_p), ("free_buffer", ctypes.c_void_p)]


class PluginAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("call", ctypes.c_void_p),
                ("free_buffer", ctypes.c_void_p), ("shutdown", ctypes.c_void_p)]


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


def load(path: str):
    lib = ctypes.CDLL(path)
    host, api = HostAPI(abi_version=1), PluginAPI()
    if lib.cliproxy_plugin_init(ctypes.byref(host), ctypes.byref(api)) != 0:
        raise SystemExit("plugin init failed")
    lib.cliproxy_plugin_call.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_size_t, ctypes.POINTER(Buffer)]
    lib.cliproxy_plugin_call.restype = ctypes.c_int

    def call(method: str, payload) -> dict:
        body = json.dumps(payload).encode()
        buf = Buffer()
        lib.cliproxy_plugin_call(method.encode(), body, len(body), ctypes.byref(buf))
        return json.loads(ctypes.string_at(buf.ptr, buf.len))

    return call


def b64(value) -> str:
    return base64.b64encode(json.dumps(value).encode()).decode()


def body_of(envelope: dict) -> str:
    raw = envelope["result"]["Body"]
    if isinstance(raw, str):
        try:
            return base64.b64decode(raw).decode()
        except ValueError:
            return raw
    return json.dumps(raw)


class FakeManagement(BaseHTTPRequestHandler):
    entries: list = []
    written = None

    def log_message(self, *_a):
        return

    def _json(self, payload, code=200):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):  # noqa: N802
        if not self.headers.get("Authorization"):
            return self._json({"error": "unauthorized"}, 401)
        self._json({"openai-compatibility": FakeManagement.entries})

    def do_PUT(self):  # noqa: N802
        length = int(self.headers.get("Content-Length") or 0)
        FakeManagement.written = self.rfile.read(length)
        FakeManagement.entries = json.loads(FakeManagement.written)
        self._json({"status": "ok"})


def main() -> int:
    key = os.environ.get("MISTRAL_API_KEY")
    if not key:
        print("  skip  MISTRAL_API_KEY is not exported")
        return 0
    lib_path = os.environ.get("PLUGIN_LIB") or str(ROOT / "dist" / "cpa-mistral-vibe.dylib")
    if not Path(lib_path).exists():
        print(f"  skip  no built plugin at {lib_path}")
        return 0

    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeManagement)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{server.server_address[1]}"

    call = load(lib_path)
    failures = 0

    def check(label: str, ok: bool, detail: str = "") -> None:
        nonlocal failures
        print(f"  {'ok  ' if ok else 'FAIL'}  {label} {'' if ok else detail}")
        if not ok:
            failures += 1

    print("  ..    talking to api.mistral.ai and a fake CPA management API")

    quota = call("quota.fetch", {
        "provider": "mistral-vibe",
        "auth_index": "live",
        "storage_json": b64({"plan_key": key}),
    })
    check("quota.fetch succeeds", quota.get("ok") is True, json.dumps(quota)[:400])
    if quota.get("ok"):
        result = quota["result"]
        plan = (result.get("subscription") or {}).get("plan")
        summary = result.get("summary") or []
        check("plan name read", bool(plan), json.dumps(result)[:300])
        check("monthly balance read", len(summary) == 1 and summary[0].get("currency") == "USD",
              json.dumps(summary)[:300])
        if summary:
            print(f"        plan={plan} remaining={summary[0].get('value'):.2f} {summary[0].get('currency')}")

    connect = call("management.handle", {
        "method": "POST",
        "path": "/v0/management/mistral-vibe/connect",
        "headers": {"X-Management-Key": ["mk"]},
        "query": {},
        "body": b64({"plan_key": key, "base_url": "https://api.mistral.ai", "management_base": base}),
    })
    payload = body_of(connect)
    check("connect succeeds", '"ok":true' in payload.replace(" ", ""), payload[:300])
    entry = json.loads(FakeManagement.written) if FakeManagement.written else []
    check("an entry was written", len(entry) == 1, payload[:200])
    if entry:
        written = entry[0]
        check("entry name", written.get("name") == "Mistral Vibe", json.dumps(written)[:200])
        check("entry base url", written.get("base-url") == "https://api.mistral.ai/v1", str(written.get("base-url")))
        check("entry carries the key", written.get("api-key-entries", [{}])[0].get("api-key") == key, "key mismatch")
        models = [m.get("name") for m in written.get("models", [])]
        check("entry carries the plan's models",
              models == ["mistral-vibe-cli-latest", "mistral-vibe-cli-with-tools", "mistral-vibe-cli-fast"], str(models))

    server.shutdown()
    print()
    if failures:
        print(f"LIVE MISTRAL: {failures} check(s) failed")
        return 1
    print("LIVE MISTRAL PASSED")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
