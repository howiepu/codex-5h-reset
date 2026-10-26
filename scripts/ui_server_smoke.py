#!/usr/bin/env python3
"""验证浏览器预先建立空闲连接时，调试页面和手动预热仍能响应。"""
import argparse
import json
import select
import signal
import socket
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.request import Request, urlopen

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("library", type=Path)
args = parser.parse_args()
with socket.socket() as reservation:
    reservation.bind(("127.0.0.1", 0))
    port = reservation.getsockname()[1]
process = subprocess.Popen(
    [sys.executable, "-u", str(Path(__file__).with_name("ui_test_server.py")), str(args.library.resolve()), "--port", str(port), "--model-delay", "0.5"],
    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
)
idle = None
try:
    assert select.select([process.stdout], [], [], 5)[0], "调试服务未及时启动"
    ready = process.stdout.readline()
    assert f"127.0.0.1:{port}/" in ready, ready
    idle = socket.create_connection(("127.0.0.1", port), timeout=2)
    base = f"http://127.0.0.1:{port}"
    api = base + "/v0/management/plugins/codex-5h-reset"
    headers = {"Authorization": "Bearer test-management-key", "Content-Type": "application/json"}

    def call(path, body=None):
        request = Request(api + path, data=json.dumps(body).encode() if body is not None else None, headers=headers)
        with urlopen(request, timeout=2) as response:
            return response.status, json.load(response)

    with urlopen(base + "/v0/resource/plugins/codex-5h-reset/status", timeout=2) as response:
        assert response.status == 200 and b"<html" in response.read()
    _, config = call("/config")
    _, plan = call("/token-refresh")
    assert not plan["enabled"] and plan["lead_hours"] == 120 and len(plan["accounts"]) == 3
    _, tested = call("/test-notification", {key: value for key, value in config.items() if key.startswith("notify_")})
    assert tested["results"] == [{"channel": "Webhook", "success": True}], tested
    _, untouched = call("/status")
    assert not untouched["history"].get("last_run"), "通知测试不能执行模型预热"
    request = Request(base + "/v0/management/plugins/codex-5h-reset/run", data=b"{}", headers={"Authorization": "Bearer test-management-key", "Content-Type": "application/json"})
    with urlopen(request, timeout=2) as response:
        results = json.load(response)["results"]
        assert [row["status_code"] for row in results] == [200, 204], results
        assert all(row["success"] for row in results), results
    code, started = call("/run", {"request_id": "http-stop-once", "auth_ids": ["a", "b"], "async": True})
    assert code == 202, started
    deadline = time.monotonic() + 2
    while time.monotonic() < deadline:
        _, status = call("/status")
        if status["history"]["last_results"] and status["history"]["last_results"][0]["status"] == "running":
            break
        time.sleep(0.01)
    else:
        raise AssertionError("未观察到在途请求")
    _, stopping = call("/stop", {"run_id": started["progress"]["id"]})
    assert stopping["status"]["history"]["last_progress"]["stop_requested"]
    while time.monotonic() < deadline:
        _, status = call("/status")
        if not status["running"]:
            break
        time.sleep(0.02)
    else:
        raise AssertionError("停止后未结束")
    assert [r["status"] for r in status["history"]["last_results"]] == ["success", "stopped"], status
    _, duplicate = call("/run", {"request_id": "http-stop-once", "auth_ids": ["b", "a"], "async": True})
    assert duplicate["duplicate"] and duplicate["progress"]["id"] == started["progress"]["id"]
    _, archived = call("/logs?day=" + datetime.now(timezone.utc).strftime("%Y-%m-%d"))
    assert archived["logs"], archived
    with urlopen(Request(base + "/v8/management/config/server", headers=headers), timeout=2) as response:
        assert json.load(response)["port"] == port
    _, cached = call("/connection", {"url": base})
    assert cached["cached"]
    assert Path(config["state_file"] + ".connection.json").stat().st_mode & 0o777 == 0o600
    config.update(token_refresh_enabled=True)
    request = Request(api + "/config", data=json.dumps(config).encode(), headers=headers, method="PUT")
    with urlopen(request, timeout=2) as response:
        assert response.status == 200
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        _, plan = call("/token-refresh")
        if any(row.get("last_result") == "success" for row in plan["accounts"]):
            break
        time.sleep(0.02)
    else:
        raise AssertionError("未观察到模拟 CPA 完成令牌刷新")
    assert plan["next_at"] and all(row["state"] == "scheduled" for row in plan["accounts"] if row["id"] != "disabled")
    assert "fake-access" not in json.dumps(plan) and "test-management-key" not in json.dumps(plan)
    _, status = call("/status")
    assert "token_refresh_management_key" not in status["configuration"] and "token_refresh" not in status["history"]
    assert status["connection"]["cached"] and "test-management-key" not in json.dumps(status)
    print("UI 服务验证通过：通知、预热与归档，以及后台串行调用模拟 CPA 刷新并重新排期。")
finally:
    if idle:
        idle.close()
    if process.poll() is None:
        process.send_signal(signal.SIGINT)
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
    process.stdout.close()
