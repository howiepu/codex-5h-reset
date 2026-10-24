#!/usr/bin/env python3
"""用模拟 CPA 回调加载真实共享库；不会访问网络或真实账号。"""
import argparse
import base64
import ctypes as C
import json
import re
import tempfile
import time
from pathlib import Path


class Buffer(C.Structure):
    _fields_ = [("ptr", C.c_void_p), ("len", C.c_size_t)]


HostCall = C.CFUNCTYPE(C.c_int, C.c_void_p, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
Free = C.CFUNCTYPE(None, C.c_void_p, C.c_size_t)
PluginCall = C.CFUNCTYPE(C.c_int, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
Shutdown = C.CFUNCTYPE(None)


class HostAPI(C.Structure):
    _fields_ = [("abi_version", C.c_uint32), ("host_ctx", C.c_void_p), ("call", HostCall), ("free_buffer", Free)]


class PluginAPI(C.Structure):
    _fields_ = [("abi_version", C.c_uint32), ("call", PluginCall), ("free_buffer", Free), ("shutdown", Shutdown)]


class FakeCPA:
    def __init__(self, path):
        self.buffers, self.requests, self.errors = {}, [], []
        self.allocations = self.frees = 0
        self.model_delay = 0
        self.status_codes = {"a": 200, "b": 204}
        self.callback = HostCall(self.host_call)
        self.free_callback = Free(self.host_free)
        self.host = HostAPI(1, None, self.callback, self.free_callback)
        self.api = PluginAPI()
        self.library = C.CDLL(str(Path(path).resolve()))
        init = self.library.cliproxy_plugin_init
        init.argtypes, init.restype = [C.POINTER(HostAPI), C.POINTER(PluginAPI)], C.c_int
        assert init(None, C.byref(self.api)) != 0
        assert init(C.byref(self.host), C.byref(self.api)) == 0
        assert self.api.abi_version == 1

    def host_call(self, _, method, raw, length, out):
        try:
            method = method.decode()
            request = json.loads(C.string_at(raw, length))
            if method == "host.auth.list":
                result = {"files": [{"id": "b", "label": "模拟账号 B", "provider": "codex", "unavailable": True, "status": "cooldown"}, {"id": "a", "label": "模拟账号 A", "provider": "codex", "status": "active"}, {"id": "disabled", "label": "已停用模拟账号", "provider": "codex", "disabled": True}, {"id": "other", "provider": "gemini"}]}
            elif method == "host.auth.models":
                result = {"credentials": [{"id": "a", "provider": "codex", "models": ["gpt-5.6-luna", "gpt-6-luna"]}, {"id": "b", "provider": "codex", "models": ["gpt-6-luna"]}, {"id": "disabled", "provider": "codex", "models": []}]}
            elif method == "host.model.execute":
                self.requests.append(request)
                time.sleep(self.model_delay)
                result = {"status_code": self.status_codes[request["auth_id"]], "body": base64.b64encode(b"not a generated completion").decode()}
            elif method == "host.log":
                result = {}
            else:
                raise AssertionError("不应增加其他宿主调用：" + method)
            response = {"ok": True, "result": result}
        except Exception as exc:
            self.errors.append(str(exc))
            response = {"ok": False, "error": {"code": "mock_error", "message": "模拟 CPA 错误"}}
        buffer = C.create_string_buffer(json.dumps(response).encode())
        ptr = C.addressof(buffer)
        self.buffers[ptr] = buffer
        self.allocations += 1
        out[0].ptr, out[0].len = ptr, len(buffer.value)
        return 0

    def host_free(self, ptr, _):
        if ptr not in self.buffers:
            self.errors.append("宿主缓冲区被重复释放")
        else:
            del self.buffers[ptr]
            self.frees += 1

    def call(self, method, request=None):
        payload = json.dumps(request or {}).encode()
        raw, response = C.create_string_buffer(payload), Buffer()
        code = self.api.call(method.encode(), raw, len(payload), C.byref(response))
        try:
            envelope = json.loads(C.string_at(response.ptr, response.len))
        finally:
            self.api.free_buffer(response.ptr, response.len)
        assert code == 0 and envelope["ok"], envelope
        assert not self.errors, self.errors
        return envelope["result"]

    def management(self, method, path, body=None, query=None):
        request = {"Method": method, "Path": path, "Query": query or {}}
        if body is not None:
            request["Body"] = base64.b64encode(body).decode()
        response = self.call("management.handle", request)
        body = base64.b64decode(response["Body"])
        return response["StatusCode"], body


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("library", type=Path)
    parser.add_argument("expected_version", nargs="?")
    args = parser.parse_args()
    cpa = FakeCPA(args.library)
    with tempfile.TemporaryDirectory(prefix="codex-5h-reset-abi-") as temp:
        state = Path(temp) / "history.json"
        config = "enabled: true\nmodel: gpt-5.6-luna\nprompt: ABI-smoke-only\nstate_file: " + json.dumps(str(state)) + "\n"
        lifecycle = {"config_yaml": base64.b64encode(config.encode()).decode()}
        metadata = cpa.call("plugin.register", lifecycle)["metadata"]
        assert metadata["Name"] == "Codex 5H Reset"
        if args.expected_version:
            assert metadata["Version"] == args.expected_version, metadata
        routes = cpa.call("management.register")
        assert len(routes["routes"]) == 6 and len(routes["resources"]) == 1
        base = "/v0/management/plugins/codex-5h-reset"
        code, html = cpa.management("GET", "/v0/resource/plugins/codex-5h-reset/status")
        assert code == 200 and b"<html" in html and b"ABI-smoke-only" not in html
        assert not cpa.requests, "注册插件时不得自动预热"
        assert cpa.management("POST", base + "/test-notification", b"{}")[0] == 400
        assert not cpa.requests, "测试通知不得执行模型"
        code, raw = cpa.management("GET", base + "/accounts")
        accounts = json.loads(raw)["accounts"]
        assert code == 200 and [a["id"] for a in accounts] == ["a", "b", "disabled"]
        assert accounts[0]["models"] == ["gpt-5.6-luna", "gpt-6-luna"] and accounts[2]["models"] == []
        assert "gpt-5.6-luna" not in accounts[1]["models"], "未注册模型的账号仍应保留并交给 CPA 执行"
        code, raw = cpa.management("POST", base + "/run")
        data = json.loads(raw)
        assert code == 200 and len(data["results"]) == 2, data
        assert all(row["success"] for row in data["results"]), data
        assert [req["auth_id"] for req in cpa.requests] == ["a", "b"]
        for req in cpa.requests:
            assert req["entry_protocol"] == req["exit_protocol"] == "openai"
            assert req["forced_provider"] == "codex" and req["stream"] is False
            assert req["model"] == "gpt-5.6-luna"
            assert req["headers"] == {"Content-Type": ["application/json"]}
            body = json.loads(base64.b64decode(req["body"]))
            assert body == {"model": "gpt-5.6-luna", "stream": False, "messages": [{"role": "user", "content": "ABI-smoke-only"}]}, body
        code, raw = cpa.management("POST", base + "/run", b'{"request_id":"abi-selected-once","auth_ids":["b"]}')
        assert code == 200 and [r["auth_id"] for r in json.loads(raw)["results"]] == ["b"]
        before = len(cpa.requests)
        code, raw = cpa.management("POST", base + "/run", b'{"request_id":"abi-selected-once","auth_ids":["b"]}')
        assert code == 200 and json.loads(raw)["duplicate"] and len(cpa.requests) == before
        stored = json.loads(state.read_text())
        assert stored["schema_version"] == 1 and len(stored["last_results"]) == 1
        assert len(list((state.parent / "logs").glob("*.jsonl"))) == 1
        random_config = config.replace("prompt: ABI-smoke-only", "prompt: ''") + "random_arithmetic: true\n"
        cpa.call("plugin.reconfigure", {"config_yaml": base64.b64encode(random_config.encode()).decode()})
        before = len(cpa.requests)
        assert cpa.management("POST", base + "/run")[0] == 200
        assert len(cpa.requests) == before + 2
        for req in cpa.requests[before:]:
            assert req["entry_protocol"] == req["exit_protocol"] == "openai-response"
            assert req["auth_id"] in {"a", "b"} and req["forced_provider"] == "codex" and req["stream"] is False
            assert req["headers"]["X-Openai-Internal-Codex-Responses-Lite"] == ["true"]
            body = json.loads(base64.b64decode(req["body"]))
            assert re.fullmatch(r"[1-9][+*/-][1-9]|[1-9] [+*/-] [1-9]", body["input"]), body
            assert body["reasoning"] == {"effort": "low", "context": "all_turns"}
            assert body["text"] == {"verbosity": "low"} and body["tool_choice"] == "none" and body["tools"] == []
            assert body["store"] is False and body["stream"] is False and "messages" not in body
        cpa.call("plugin.quiesce")
        assert cpa.management("POST", base + "/run")[0] == 409
        cpa.call("plugin.reconfigure", lifecycle)
        assert json.loads(cpa.management("GET", base + "/status")[1])["history"]["last_run"]
        cpa.call("plugin.shutdown")
        cpa.api.shutdown()
        assert not cpa.buffers and cpa.allocations == cpa.frees and not cpa.errors, cpa.errors
    print("ABI 验证通过：真实共享库注册、管理路由、逐账号消息、2xx 判定、持久化与停止。")


if __name__ == "__main__":
    main()
