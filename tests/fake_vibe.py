#!/usr/bin/env python3
"""Fake Vibe console for hermetic tests.

Serves the plan shapes the plugin understands, plus a Vibe-CLI-shaped
`/api/vibe/whoami` for tenant discovery. Nothing here talks to the network.

    python3 tests/fake_vibe.py 18099
"""

from __future__ import annotations

import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# The real /api/vibe/whoami shape, captured from console.mistral.ai.
WHOAMI = {
    "plan_type": "CHAT",
    "plan_name": "EDU",
    "prompt_switching_to_pro_plan": True,
    "organization_kind": "S",
    "customer_id": "8c29510f-2300-40a4-a837-94bd159edd84",
    "api_base": "",

    "vibe_base": "https://chat.mistral.ai",
    "primitive_access_scope": "personal_and_shared",
}

PLAN = {
    "plan": {"title": "Vibe Pro", "kind": "chat"},
    "budget": {
        "usagePercentage": 38.0,
        "total": 100.0,
        "currency": "USD",
        "resetAt": "2026-12-01T00:00:00Z",
    },
    "windows": [
        {"group": "claude", "window": "5h", "remainingFraction": 0.62, "reset_at": "2026-11-18T18:00:00Z"},
        {"group": "mistral", "window": "weekly", "remaining_amount": 75000, "total_amount": 100000},
    ],
    "expires_at": "2026-12-01T00:00:00Z",
}


# Set to 1 to keep the legacy /api/vibe/plan route for parser tests.
OPTIONAL_LEGACY_PLAN = False

BILLING_BUDGET = {
    "vibe_budget": {"usage_percentage": 5.668426738666667, "initial_budget": 150.0,
                    "currency": "USD", "reset_at": "2026-12-01T00:00:00Z", "payg_enabled": False},
    "api_budget": {"usage_percentage": 0.001918, "initial_budget": 15.0,
                   "currency": "USD", "reset_at": "2026-12-01T00:00:00Z", "payg_enabled": False},
}
BILLING_SUBSCRIPTION = {"plan_name": "EDU", "payg_enabled": False, "vibe_payg_enabled": False, "enterprise": False}


def chat_completion(body: dict) -> dict:
    """Answer like Mistral does, including tool_calls when tools are declared."""
    tools = body.get("tools") or []
    wants_tool = bool(tools) and body.get("tool_choice") != "none"
    message: dict = {"role": "assistant"}
    finish = "stop"
    if wants_tool:
        name = tools[0]["function"]["name"]
        message["content"] = None
        message["tool_calls"] = [{
            "id": "call_fake_1",
            "type": "function",
            "function": {"name": name, "arguments": json.dumps({"command": "ls"})},
        }]
        finish = "tool_calls"
    else:
        message["content"] = "PONG"
    return {
        "id": "chatcmpl-fake",
        "object": "chat.completion",
        "created": 1700000000,
        "model": body.get("model", "mistral-vibe-cli-latest"),
        "choices": [{"index": 0, "message": message, "finish_reason": finish}],
        "usage": {"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7},
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        return

    def _send(self, payload, code=200):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):  # noqa: N802
        path = self.path.split("?")[0]
        length = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(length) or b"{}")
        if path == "/v1/chat/completions":
            if not self.headers.get("Authorization"):
                return self._send({"message": "Unauthorized"}, 401)
            if body.get("stream"):
                frames = [
                    {"id": "c1", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]},
                    {"id": "c1", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {"content": "PO"}, "finish_reason": None}]},
                    {"id": "c1", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {"content": "NG"}, "finish_reason": None}]},
                    {"id": "c1", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
                ]
                payload = "".join("data: " + json.dumps(f) + "\n\n" for f in frames) + "data: [DONE]\n\n"
                data = payload.encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                return
            return self._send(chat_completion(body))
        return self._send({"message": "not found"}, 404)

    def do_GET(self):  # noqa: N802
        path = self.path.split("?")[0]
        if path in ("/api/vibe/plan", "/api/vibe/subscription"):
            # The real console has no such routes for a personal key; only
            # whoami exists and billing lives on the inference host.
            if not OPTIONAL_LEGACY_PLAN:
                return self._send({"detail": None}, 401)
            if self.headers.get("x-api-key") != "sk-plan" and not self.headers.get("Authorization"):
                return self._send({"error": "unauthorized"}, 401)
            return self._send(PLAN)
        if path == "/api/vibe/whoami":
            body = dict(WHOAMI)
            body["api_base"] = f"http://{self.headers.get('Host', '127.0.0.1')}"
            return self._send(body)
        if path == "/v1/billing/budget":
            return self._send(BILLING_BUDGET)
        if path == "/v1/billing/subscription":
            return self._send(BILLING_SUBSCRIPTION)
        return self._send({"error": "not found"}, 404)


def main() -> int:
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 0
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print(f"fake vibe console on http://127.0.0.1:{server.server_address[1]}", flush=True)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        threading.Event().wait()
    except KeyboardInterrupt:
        server.shutdown()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
