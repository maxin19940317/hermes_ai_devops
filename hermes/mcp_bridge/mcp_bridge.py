"""mcp_bridge — Hermes Agent 平台与 DevOps Runtime 之间的 MCP 适配层(2026-08-07)。

把 hermes-agent(WebUI/飞书 gateway)里的指令翻译成 Runtime 受控接口调用。
**不直连 ADB、不执行任意命令**——这是对 18765 那个 `tobias_adb_server`
(直连 ADB 的违规 MCP)的合规替代(CLAUDE.md §3.1/§3.3/§14 红线)。

协议: MCP Streamable HTTP(2025-03-26),官方 mcp Python SDK(FastMCP)。
工具(全部翻译成 Runtime POST /api/v1/cmd 调用):
  - devops_devices / devops_status / devops_runs / devops_result /
    devops_metrics / devops_artifacts    只读查询
  - devops_test / devops_cancel / devops_rerun /
    devops_quarantine / devops_unquarantine  副作用(设备操作)

架构: hermes-agent 平台是 MCP 客户端;本 bridge 是 MCP server,部署在
hermes-runtime 网络内的 hermes-devops-analyzer 容器(与 analyze_bridge 同容器),
通过宿主端口暴露给 hermes-rocklin(WebUI :9221)。Runtime 侧执行逻辑全部在
feishucmd.Executor(worker 内),本 bridge 只做信封翻译,无 LLM、无状态。

配置(环境变量):
  RUNTIME_CMD_API_URL     Runtime 受控接口,缺省 http://trigger:8090/api/v1/cmd
  RUNTIME_CMD_API_TOKEN   Runtime CMD_API_TOKEN(Bearer)
  MCP_BRIDGE_PORT         监听端口,缺省 8645
"""

import logging
import os
import ssl

import httpx
from mcp.server.fastmcp import FastMCP
from mcp.server.transport_security import TransportSecuritySettings

log = logging.getLogger("mcp_bridge")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

RUNTIME_CMD_API_URL = os.environ.get("RUNTIME_CMD_API_URL", "https://worker:8091/api/v1/cmd")
RUNTIME_CMD_API_TOKEN = os.environ.get("RUNTIME_CMD_API_TOKEN", "")
# mTLS(Phase 3):Runtime callbacks listener 要求客户端证书(Agent→Runtime 方向
# 18091 强制)。MCP bridge 用专用客户端证书(client-mcp-bridge)访问 worker:8091。
# CA 证书用于校验服务端;cert 是客户端证书+私钥合体。三件套任一空 → 纯 HTTP。
MTLS_CA_FILE = os.environ.get("MTLS_CA_FILE", "")
MTLS_CLIENT_CERT = os.environ.get("MTLS_CLIENT_CERT", "")
PORT = int(os.environ.get("MCP_BRIDGE_PORT", "8645"))
HTTP_TIMEOUT = float(os.environ.get("MCP_BRIDGE_HTTP_TIMEOUT_SEC", "30"))
# hermes-rocklin 经 docker0(172.17.0.1)访问;FastMCP 默认 DNS rebinding 保护
# 只放行 localhost Host,需放行 docker0/局域网 Host(否则 421 Misdirected Request)。
ALLOWED_HOSTS = os.environ.get(
    "MCP_BRIDGE_ALLOWED_HOSTS",
    "127.0.0.1,localhost,172.17.0.1,172.17.0.1:*,10.88.118.251,10.88.118.251:*",
).split(",")

mcp = FastMCP(
    "hermes-devops-mcp",
    instructions=(
        "DevOps 设备测试系统(Hermes AI DevOps)的受控接口。"
        "所有工具都是封闭指令:只读查询或受控副作用,不提供任意命令能力。"
    ),
    transport_security=TransportSecuritySettings(
        enable_dns_rebinding_protection=True,
        allowed_hosts=ALLOWED_HOSTS,
    ),
)


def runtime_cmd(command: str, args: list[str] | None = None) -> str:
    """调用 Runtime 受控接口 POST /api/v1/cmd,返回 reply 文本(同步包装)。"""
    if not RUNTIME_CMD_API_TOKEN:
        return "Runtime 受控接口未配置(RUNTIME_CMD_API_TOKEN 空),无法执行。"
    payload = {"command": command, "args": args or []}
    headers = {"Authorization": "Bearer " + RUNTIME_CMD_API_TOKEN}
    try:
        # mTLS 证书(cert/verify)必须在 Client 级别配置,httpx.post 顶层不接受。
        # CA 是自签且缺 Authority Key Identifier,Python OpenSSL 默认
        # VERIFY_X509_STRICT 会拒绝;放宽 strict 仅跳过 AKI 要求,CA 链校验
        # 仍生效(服务端身份由 CA 文件保证,不做 verify=False)。
        client_kwargs: dict = {"timeout": HTTP_TIMEOUT}
        if MTLS_CA_FILE and MTLS_CLIENT_CERT:
            ssl_ctx = ssl.create_default_context(cafile=MTLS_CA_FILE)
            ssl_ctx.verify_flags &= ~ssl.VERIFY_X509_STRICT
            client_kwargs["verify"] = ssl_ctx
            client_kwargs["cert"] = MTLS_CLIENT_CERT
        with httpx.Client(**client_kwargs) as client:
            resp = client.post(RUNTIME_CMD_API_URL, json=payload, headers=headers)
    except httpx.HTTPError as e:
        log.error("runtime cmd %s failed: %s", command, e)
        return f"Runtime 调用失败: {e}"
    if resp.status_code != 200:
        try:
            err = resp.json().get("message", resp.text)
        except Exception:
            err = resp.text
        log.error("runtime cmd %s status=%d: %s", command, resp.status_code, err)
        return f"Runtime 拒绝({resp.status_code}): {err}"
    return resp.json().get("reply", "")


# ---- 只读查询 ----
@mcp.tool()
def devops_devices() -> str:
    """列出 DevOps 设备测试系统的设备(serial/soc/状态/fail_streak)。无参数。"""
    return runtime_cmd("devices")


@mcp.tool()
def devops_status() -> str:
    """运行概览:运行中 workflow 数、活跃租约、设备状态汇总。无参数。"""
    return runtime_cmd("status")


@mcp.tool()
def devops_runs(n: int = 5) -> str:
    """最近运行历史(n 条,1-20,缺省 5):commit/variant/verdict/时间。"""
    return runtime_cmd("runs", [str(max(1, min(20, n)))])


@mcp.tool()
def devops_result(workflow_id: str) -> str:
    """单次运行的各变体结论。workflow_id 必填(来自 devops_runs)。"""
    return runtime_cmd("result", [workflow_id])


@mcp.tool()
def devops_metrics(variant: str) -> str:
    """某变体性能基线(最近 5 次 PASSED 中位数)。variant 必填。"""
    return runtime_cmd("metrics", [variant])


@mcp.tool()
def devops_artifacts(variant: str) -> str:
    """某变体最近构建历史(项目/版本/commit/pipeline)。variant 必填。"""
    return runtime_cmd("artifacts", [variant])


# ---- 副作用指令(设备操作,hermes-agent 侧应二次确认) ----
@mcp.tool()
def devops_test(variant: str) -> str:
    """触发指定变体的设备测试(副作用:会占用设备跑测试)。variant 必填。"""
    return runtime_cmd("test", [variant])


@mcp.tool()
def devops_cancel(workflow_id: str) -> str:
    """取消运行中的 workflow(副作用)。workflow_id 必填(来自 devops_runs)。"""
    return runtime_cmd("cancel", [workflow_id])


@mcp.tool()
def devops_rerun(source_workflow_id: str, variant: str | None = None) -> str:
    """重跑某次权威终态运行的失败变体(副作用)。source_workflow_id 必填;variant 可选。"""
    args = [source_workflow_id]
    if variant:
        args.append(variant)
    return runtime_cmd("rerun", args)


@mcp.tool()
def devops_quarantine(device_id: str | None = None) -> str:
    """手动隔离设备(副作用)。device_id 可选(单台设备时可省略)。"""
    return runtime_cmd("quarantine", [device_id] if device_id else [])


@mcp.tool()
def devops_unquarantine(device_id: str | None = None) -> str:
    """解除设备隔离(副作用)。device_id 可选(单台设备时可省略)。"""
    return runtime_cmd("unquarantine", [device_id] if device_id else [])


def main():
    import uvicorn

    log.info("mcp_bridge serving on port %d", PORT)
    # FastMCP 的 streamable_http_app 是 Starlette ASGI 应用(2025-03-26 协议)。
    # Host 校验由 FastMCP 的 transport_security 处理(见上面 ALLOWED_HOSTS)。
    uvicorn.run(mcp.streamable_http_app, host="0.0.0.0", port=PORT, log_level="info")


if __name__ == "__main__":
    main()
