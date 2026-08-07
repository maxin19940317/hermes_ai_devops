"""mcp_bridge 测试:验证工具白名单与 Runtime 调用翻译(假 httpx 驱动)。

用 httpx.MockTransport 替换 Runtime 调用,不依赖真实 Runtime。
覆盖:工具清单、每个工具的参数翻译、Runtime 错误降级。
"""

import importlib.util
import json
import sys
from pathlib import Path

import pytest

BRIDGE = Path(__file__).with_name("mcp_bridge.py")


def load_module():
    spec = importlib.util.spec_from_file_location("mcp_bridge_under_test", BRIDGE)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["mcp_bridge_under_test"] = mod
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture()
def bridge(monkeypatch):
    mod = load_module()
    # 注入假 Runtime 端点和 token
    monkeypatch.setattr(mod, "RUNTIME_CMD_API_URL", "http://fake-runtime/api/v1/cmd")
    monkeypatch.setattr(mod, "RUNTIME_CMD_API_TOKEN", "test-token")
    return mod


# ---- 工具注册 ----
def test_tools_registered(bridge):
    names = {t.name for t in bridge.mcp._tool_manager.list_tools()}
    expected = {
        "devops_devices", "devops_status", "devops_runs", "devops_result",
        "devops_metrics", "devops_artifacts", "devops_test", "devops_cancel",
        "devops_rerun", "devops_quarantine", "devops_unquarantine",
    }
    assert expected <= names, f"missing tools: {expected - names}"
    # 不允许出现任何直连 ADB 的任意命令工具
    for t in names:
        assert not t.startswith("adb_"), f"违规工具: {t}"


# ---- 参数翻译 ----
def test_devices_no_args(bridge, monkeypatch):
    calls = {}

    def fake_post(url, json=None, headers=None, timeout=None):
        calls["payload"] = json
        return FakeResponse(200, {"reply": "devices ok"})

    monkeypatch.setattr("httpx.post", fake_post)
    assert bridge.devops_devices() == "devices ok"
    assert calls["payload"] == {"command": "devices", "args": []}


def test_test_variant(bridge, monkeypatch):
    calls = {}

    def fake_post(url, json=None, headers=None, timeout=None):
        calls["payload"] = json
        return FakeResponse(200, {"reply": "started"})

    monkeypatch.setattr("httpx.post", fake_post)
    bridge.devops_test("aarch64_Android_SNPE_1.68")
    assert calls["payload"] == {"command": "test", "args": ["aarch64_Android_SNPE_1.68"]}


def test_rerun_with_variant(bridge, monkeypatch):
    calls = {}

    def fake_post(url, json=None, headers=None, timeout=None):
        calls["payload"] = json
        return FakeResponse(200, {"reply": "ok"})

    monkeypatch.setattr("httpx.post", fake_post)
    bridge.devops_rerun("wf-1", "aarch64_Android_SNPE_1.68")
    assert calls["payload"] == {"command": "rerun", "args": ["wf-1", "aarch64_Android_SNPE_1.68"]}


def test_rerun_without_variant(bridge, monkeypatch):
    calls = {}

    def fake_post(url, json=None, headers=None, timeout=None):
        calls["payload"] = json
        return FakeResponse(200, {"reply": "ok"})

    monkeypatch.setattr("httpx.post", fake_post)
    bridge.devops_rerun("wf-1")
    assert calls["payload"] == {"command": "rerun", "args": ["wf-1"]}


def test_quarantine_optional_id(bridge, monkeypatch):
    calls = []

    def fake_post(url, json=None, headers=None, timeout=None):
        calls.append(json)
        return FakeResponse(200, {"reply": "ok"})

    monkeypatch.setattr("httpx.post", fake_post)
    bridge.devops_quarantine()
    bridge.devops_quarantine("dev-1")
    assert calls[0] == {"command": "quarantine", "args": []}
    assert calls[1] == {"command": "quarantine", "args": ["dev-1"]}


# ---- 鉴权头 ----
def test_auth_header(bridge, monkeypatch):
    captured = {}

    def fake_post(url, json=None, headers=None, timeout=None):
        captured["headers"] = headers
        return FakeResponse(200, {"reply": "ok"})

    monkeypatch.setattr("httpx.post", fake_post)
    bridge.devops_status()
    assert captured["headers"]["Authorization"] == "Bearer test-token"


# ---- Runtime 错误降级 ----
def test_runtime_rejects(bridge, monkeypatch):
    def fake_post(url, json=None, headers=None, timeout=None):
        return FakeResponse(401, {"message": "unauthorized"})

    monkeypatch.setattr("httpx.post", fake_post)
    reply = bridge.devops_status()
    assert "401" in reply and "unauthorized" in reply


def test_runtime_unreachable(bridge, monkeypatch):
    import httpx

    def fake_post(url, json=None, headers=None, timeout=None):
        raise httpx.ConnectError("conn refused")

    monkeypatch.setattr("httpx.post", fake_post)
    reply = bridge.devops_devices()
    assert "调用失败" in reply


def test_no_token_configured(bridge, monkeypatch):
    monkeypatch.setattr(bridge, "RUNTIME_CMD_API_TOKEN", "")
    reply = bridge.devops_devices()
    assert "未配置" in reply


class FakeResponse:
    def __init__(self, status_code, body):
        self.status_code = status_code
        self._body = body
        self.text = json.dumps(body)

    def json(self):
        return self._body
