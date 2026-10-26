#!/usr/bin/env python3
"""本地 UI 测试宿主：模拟账号与超长日志，不发送真实请求。"""
import argparse
import base64
import json
import tempfile
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

from abi_smoke import FakeCPA

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("library", type=Path)
parser.add_argument("--port", type=int, default=47445)
parser.add_argument("--model-delay", type=float, default=0)
parser.add_argument("--fail-second", action="store_true")
args = parser.parse_args()
cpa = FakeCPA(args.library)
cpa.model_delay = args.model_delay
if args.fail_second:
    cpa.status_codes["b"] = 429
temp = tempfile.TemporaryDirectory(prefix="codex-5h-reset-ui-")
state = Path(temp.name) / "history.json"
state.write_text(json.dumps({
    "schema_version": 1,
    "last_results": [],
    "logs": [
        {"time": "2026-10-24T06:00:00Z", "level": "info", "event": "fixture.long-content", "message": "超长字段测试：展开详情后应自动换行，完整内容可在区域内滚动。", "fields": {"auth_id": "sample-account-" + "x" * 400, "message": "long-unbroken-content-" * 300}},
        {"time": "2026-10-24T06:00:01Z", "level": "error", "event": "fixture." + "unbroken-event-" * 30, "message": "unbroken-message-" * 70, "fields": {"note": "仅用于验证长文本，不是真实错误"}},
        {"time": "2026-10-24T06:00:02Z", "level": "warn", "event": "fixture.schedule-skipped", "message": "模拟：上一轮仍在运行，本次计划跳过。", "fields": {"schedule_id": "早"}},
    ],
}), encoding="utf-8")
config = {
    "enabled": True, "priority": 7, "store": {"repository": "https://github.com/howiepu/codex-5h-reset", "version": "0.1.0"},
    "model": "gpt-5.6-luna", "prompt": "UI test only", "timezone": "Asia/Singapore",
    "schedules": [{"id": "早", "at": "05:00", "enabled": True}, {"id": "中", "at": "10:05", "enabled": True}, {"id": "晚", "at": "15:10", "enabled": True}],
    "max_log_entries": 200, "state_file": str(state),
    "token_refresh_enabled": False, "token_refresh_before_hours": 120,
    "history_retention_days": 30, "notify_webhook_url": "", "notify_bark_url": "",
    "notify_webhook_method": "POST", "notify_webhook_format": "json",
    "notify_webhook_headers": {"Authorization": "Bearer local-preview-only"},
    "notify_webhook_body": '{\n  "body": "{{notification}}",\n  "format": "text"\n}',
}


def configure():
    cpa.call("plugin.reconfigure", {"config_yaml": base64.b64encode(json.dumps(config).encode()).decode()})


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.respond()

    def do_POST(self):
        self.respond()

    def do_PUT(self):
        self.respond()

    def do_PATCH(self):
        self.respond()

    def do_DELETE(self):
        self.respond()

    def respond(self):
        global config
        parsed = urlsplit(self.path)
        if parsed.path == "/mock/webhook":
            self.rfile.read(int(self.headers.get("Content-Length", 0)))
            self.send(204, b"")
            print("本地模拟 Webhook 已接收测试消息；未向外部投递", flush=True)
            return
        if parsed.path == "/narrow":
            theme = "dark" if parse_qs(parsed.query).get("theme") == ["dark"] else "light"
            html = f'<!doctype html><html lang="zh-CN" data-theme="{theme}"><meta charset="utf-8"><title>375px UI test</title><body style="margin:0;background:#eee"><iframe title="375px viewport" src="/v0/resource/plugins/codex-5h-reset/status" style="display:block;width:375px;max-width:100%;height:100vh;border:0"></iframe></body></html>'
            self.send(200, html.encode(), "text/html")
            return
        if self.path.startswith(("/v0/management/", "/v8/management/")) and self.headers.get("Authorization") != "Bearer test-management-key":
            self.send(401, b'{"error":"use test-management-key"}')
            return
        if parsed.path == "/v8/management/config/server" and self.command == "GET":
            self.send(200, json.dumps({"host": "127.0.0.1", "port": server.server_port, "tls": {"enable": False}}).encode())
            return
        if parsed.path == "/v0/management/auth-files/refresh" and self.command == "POST":
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            id_ = body["auth_index"]
            assert body["name"] == id_ + ".json" and id_ in cpa.credentials
            now = datetime.now(timezone.utc)
            cpa.credentials[id_] = {**cpa.credentials[id_], "access_token": "fake-rotated-access-" + now.isoformat(), "refresh_token": "fake-rotated-refresh-" + now.isoformat(), "expired": (now + timedelta(days=10)).isoformat(), "last_refresh": now.isoformat()}
            self.send(200, b'{"ok":true}')
            print("本地模拟 CPA 已刷新账号令牌；未访问真实账号", flush=True)
            return
        if self.path.endswith("/config"):
            if self.command == "GET":
                self.send(200, json.dumps(config).encode())
                return
            if self.command == "PUT":
                updated = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
                assert updated["priority"] == 7 and updated["store"] == config["store"] and updated["enabled"] is True, "CPA 管理字段被覆盖"
                config = updated
                configure()
                self.send(200, b'{"status":"ok"}')
                print("UI 保存通过：enabled、priority、store 均保留", flush=True)
                return
        request_body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        code, body = cpa.management(self.command, parsed.path, request_body, parse_qs(parsed.query), {key.title(): [value] for key, value in self.headers.items()})
        self.send(code, body, "text/html" if body.startswith(b"<!doctype") else "application/json")

    def send(self, code, body, content_type="application/json"):
        self.send_response(code)
        self.send_header("Content-Type", content_type + "; charset=utf-8")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
config["notify_webhook_url"] = f"http://127.0.0.1:{server.server_port}/mock/webhook"
configure()
print(f"UI 测试：http://127.0.0.1:{server.server_port}/v0/resource/plugins/codex-5h-reset/status", flush=True)
print(f"窄屏测试：http://127.0.0.1:{server.server_port}/narrow；模拟密钥：test-management-key", flush=True)
try:
    server.serve_forever()
except KeyboardInterrupt:
    pass
finally:
    server.server_close()
    cpa.api.shutdown()
    temp.cleanup()
