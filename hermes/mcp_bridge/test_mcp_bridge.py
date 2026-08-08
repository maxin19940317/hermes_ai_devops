"""mcp_bridge 测试:验证工具白名单与 Runtime 调用翻译。

用 httpx.MockTransport 替换 Runtime 调用,不依赖真实 Runtime。

注入方式(A11):生产代码在函数内部直接构造 client
(`with httpx.Client(**client_kwargs) as client:`),没有参数能把 transport
传进去,所以单独创建 MockTransport 进不了调用路径。这里 patch `httpx.Client`
构造器,让它返回一个**真实的** Client(带 MockTransport)——而不是 fake 对象:

  * 真 Client 会照常处理 client_kwargs,mTLS 的 verify/cert 仍在被测路径上,
    fake 对象则会把 mcp_bridge.py 里那段 ssl_ctx/cert 组装整段旁路掉;
  * 必须先把原始类存进 _REAL_CLIENT 再 patch,否则替换函数内部再次调用
    httpx.Client(...) 会调到自己 → 无限递归。

此前这些用例 patch 的是模块级函数 `httpx.post`,而生产代码为支持 mTLS 早已
改用 httpx.Client,桩根本不在调用路径上(装好依赖后仍 7 failed / 3 passed)。
"""

import importlib.util
import json
import sys
from pathlib import Path

import httpx
import pytest

BRIDGE = Path(__file__).with_name("mcp_bridge.py")

# 先存原始类再 patch,防止替换函数内部调用 httpx.Client 时递归(见模块 docstring)
_REAL_CLIENT = httpx.Client


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


class Recorder:
    """记录被 MockTransport 拦下的请求,供断言取用。"""

    def __init__(self):
        self.payloads = []
        self.headers = []
        self.client_kwargs = []

    @property
    def payload(self):
        assert self.payloads, "Runtime 未被调用(桩没进调用路径?)"
        return self.payloads[-1]

    @property
    def last_headers(self):
        assert self.headers, "Runtime 未被调用(桩没进调用路径?)"
        return self.headers[-1]


def install_transport(monkeypatch, handler):
    """把 httpx.Client 换成"带 MockTransport 的真 Client",并记录构造参数。

    返回 Recorder:除请求体/请求头外,还留下 client_kwargs,
    使 mTLS 参数(verify/cert/timeout)可被断言——这是不用 fake Client 的原因。
    """
    rec = Recorder()

    def record_and_handle(request: httpx.Request) -> httpx.Response:
        rec.headers.append(dict(request.headers))
        body = request.content.decode() or "{}"
        rec.payloads.append(json.loads(body))
        return handler(request)

    def fake_client(**kwargs):
        rec.client_kwargs.append(kwargs)
        return _REAL_CLIENT(transport=httpx.MockTransport(record_and_handle), **kwargs)

    monkeypatch.setattr("httpx.Client", fake_client)
    return rec


def ok(body):
    return lambda request: httpx.Response(200, json=body)


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
    rec = install_transport(monkeypatch, ok({"reply": "devices ok"}))
    assert bridge.devops_devices() == "devices ok"
    assert rec.payload == {"command": "devices", "args": []}


def test_test_variant(bridge, monkeypatch):
    rec = install_transport(monkeypatch, ok({"reply": "started"}))
    bridge.devops_test("aarch64_Android_SNPE_1.68")
    assert rec.payload == {"command": "test", "args": ["aarch64_Android_SNPE_1.68"]}


def test_rerun_with_variant(bridge, monkeypatch):
    rec = install_transport(monkeypatch, ok({"reply": "ok"}))
    bridge.devops_rerun("wf-1", "aarch64_Android_SNPE_1.68")
    assert rec.payload == {"command": "rerun", "args": ["wf-1", "aarch64_Android_SNPE_1.68"]}


def test_rerun_without_variant(bridge, monkeypatch):
    rec = install_transport(monkeypatch, ok({"reply": "ok"}))
    bridge.devops_rerun("wf-1")
    assert rec.payload == {"command": "rerun", "args": ["wf-1"]}


def test_quarantine_optional_id(bridge, monkeypatch):
    rec = install_transport(monkeypatch, ok({"reply": "ok"}))
    bridge.devops_quarantine()
    bridge.devops_quarantine("dev-1")
    assert rec.payloads[0] == {"command": "quarantine", "args": []}
    assert rec.payloads[1] == {"command": "quarantine", "args": ["dev-1"]}


# ---- 鉴权头 ----
def test_auth_header(bridge, monkeypatch):
    rec = install_transport(monkeypatch, ok({"reply": "ok"}))
    bridge.devops_status()
    assert rec.last_headers["authorization"] == "Bearer test-token"


# ---- mTLS 参数(A11 验收项)----
def test_mtls_params_reach_client_when_configured(bridge, monkeypatch, tmp_path):
    """mTLS 两个配置项齐全时,verify(ssl context)与 cert 必须传进 client 构造。

    只有两项(A12b):MTLS_CA_FILE = 校验服务端的 CA;
    MTLS_CLIENT_CERT = 客户端证书与私钥的合体文件。
    本用例防止改 mock 的过程中把这段 mTLS 组装逻辑悄悄旁路掉。
    """
    import ssl as _ssl

    # 需要一份真实可解析的 PEM:ssl.create_default_context(cafile=...) 会解析它。
    # 复用 certifi 的根证书包(httpx 的硬依赖,装了 httpx 就一定有)。
    import certifi

    monkeypatch.setattr(bridge, "MTLS_CA_FILE", certifi.where())
    monkeypatch.setattr(bridge, "MTLS_CLIENT_CERT", str(tmp_path / "client.pem"))

    rec = install_transport(monkeypatch, ok({"reply": "ok"}))
    bridge.devops_status()

    kwargs = rec.client_kwargs[-1]
    assert isinstance(kwargs.get("verify"), _ssl.SSLContext), \
        f"verify 未以 ssl context 传入 client: {kwargs}"
    assert kwargs.get("cert") == str(tmp_path / "client.pem"), \
        f"客户端证书未传入 client: {kwargs}"


def test_plain_http_when_mtls_not_configured(bridge, monkeypatch):
    """两项任一为空 → 纯 HTTP,不带 verify/cert(A12b:注释里的"三件套"实为两项)。"""
    monkeypatch.setattr(bridge, "MTLS_CA_FILE", "")
    monkeypatch.setattr(bridge, "MTLS_CLIENT_CERT", "")
    rec = install_transport(monkeypatch, ok({"reply": "ok"}))
    bridge.devops_status()

    kwargs = rec.client_kwargs[-1]
    assert "cert" not in kwargs and "verify" not in kwargs, \
        f"未配置 mTLS 时不应传 verify/cert: {kwargs}"


# ---- Runtime 错误降级 ----
def test_runtime_rejects(bridge, monkeypatch):
    install_transport(
        monkeypatch,
        lambda request: httpx.Response(401, json={"message": "unauthorized"}),
    )
    reply = bridge.devops_status()
    assert "401" in reply and "unauthorized" in reply


def test_runtime_unreachable(bridge, monkeypatch):
    def boom(request):
        raise httpx.ConnectError("conn refused")

    install_transport(monkeypatch, boom)
    reply = bridge.devops_devices()
    assert "调用失败" in reply


def test_no_token_configured(bridge, monkeypatch):
    monkeypatch.setattr(bridge, "RUNTIME_CMD_API_TOKEN", "")
    reply = bridge.devops_devices()
    assert "未配置" in reply
