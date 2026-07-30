"""kanban_bridge — DevOps → PM 升级通道的宿主侧薄桥(设计 §6)。

Runtime Escalate 活动 POST /escalations(信封,escalation.schema.json)
→ Bearer 校验 → Schema 校验 → 渲染 title(≤200)/body(≤8KB,截断标记)
→ 参数数组 subprocess `docker exec <KANBAN_CONTAINER> hermes kanban create`
(严禁 shell 拼接)→ 解析输出取 kanban task id;
幂等键重复时 kanban 返回同一 task id(内建去重),此时追加 comment
(新 pipeline 摘要)→ 返回 {"kanban_task_id": ..., "result": created|existing}。
CLI 非零/超时 → 502。桥无状态,信封之外的能力一律不提供。

配置(环境变量):
  KANBAN_BRIDGE_TOKEN   Bearer 共享密钥(必填;对应 Runtime ESCALATION_TOKEN)
  KANBAN_CONTAINER      kanban 所在容器,缺省 hermes-rocklin
  KANBAN_BOARD          目标 board,缺省 algo_super_sdk
  KANBAN_ASSIGNEE       受理人,缺省 tobias_pm
  KANBAN_TIMEOUT_SEC    单次 docker exec 超时,缺省 30
  KANBAN_DOCKER_BIN     docker CLI 路径,缺省 docker(测试注入 fake)
"""

import hmac
import json
import logging
import os
import re
import subprocess
from pathlib import Path

import jsonschema
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from starlette.concurrency import run_in_threadpool

log = logging.getLogger("kanban_bridge")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

CONTAINER = os.environ.get("KANBAN_CONTAINER", "hermes-rocklin")
BOARD = os.environ.get("KANBAN_BOARD", "algo_super_sdk")
ASSIGNEE = os.environ.get("KANBAN_ASSIGNEE", "tobias_pm")
TIMEOUT = int(os.environ.get("KANBAN_TIMEOUT_SEC", "30"))
DOCKER_BIN = os.environ.get("KANBAN_DOCKER_BIN", "docker")
ERR_SNIPPET = 500

SCHEMA_PATH = Path(__file__).with_name("escalation.schema.json")
ESCALATION_SCHEMA = json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))

TITLE_MAX = 200
BODY_MAX = 8 * 1024
TRUNC_MARK = "\n…(truncated)"

CREATED_RE = re.compile(r"Created\s+(t_[0-9a-zA-Z]+)")
TASK_ID_RE = re.compile(r"(t_[0-9a-zA-Z]+)")

app = FastAPI(title="kanban_bridge", version="1")


class BridgeError(Exception):
    def __init__(self, status: int, msg: str):
        super().__init__(msg)
        self.status = status
        self.msg = msg


def check_auth(req: Request) -> JSONResponse | None:
    token = os.environ.get("KANBAN_BRIDGE_TOKEN", "")
    if not token:
        return JSONResponse({"error": "KANBAN_BRIDGE_TOKEN 未配置"}, status_code=500)
    want = "Bearer " + token
    if not hmac.compare_digest(req.headers.get("authorization", ""), want):
        return JSONResponse({"error": "unauthorized"}, status_code=401)
    return None


def truncate(text: str, limit: int) -> str:
    """按字符截断并附标记(标记计入上限内)。"""
    if len(text) <= limit:
        return text
    return text[: limit - len(TRUNC_MARK)] + TRUNC_MARK


def render_title(env: dict) -> str:
    """title ≤ 200: [{category}] {variant} @ {commit}: {reason}"""
    rule, src = env["rule"], env["source"]
    return truncate(
        f"[{rule['category']}] {src['variant']} @ {src['commit']}: {rule['reason']}",
        TITLE_MAX,
    )


def render_body(env: dict) -> str:
    """body ≤ 8KB:信封关键段落的确定性纯文本渲染(PM 侧够不到 devops 的
    DB/MinIO,正文必须自包含)。"""
    src, rule = env["source"], env["rule"]
    lines = [
        f"source: {src['project']} g{src['commit']} p{src['pipeline_iid']} {src['variant']}",
        f"task_id: {src['task_id']}",
        f"rule: {rule['verdict']} / {rule['category']} — {rule['reason']}",
        f"idempotency_key: {env['idempotency_key']}",
    ]
    hermes = env.get("hermes")
    if hermes:
        lines += [
            "",
            f"hermes(confidence={hermes['confidence']}): {hermes['summary']}",
        ]
        if hermes.get("root_cause"):
            lines.append(f"root_cause: {hermes['root_cause']}")
        for action in hermes.get("next_actions") or []:
            lines.append(f"next_action: {action}")
    evidence = env.get("evidence")
    if evidence:
        lines += [
            "",
            f"evidence: {evidence['object_key']} (sha256={evidence['sha256'][:12]}…, "
            f"extractor={evidence['extractor_version']})",
        ]
    for item in env.get("ruled_out") or []:
        lines.append(f"ruled_out: {item}")
    if env.get("reproduce"):
        lines.append(f"reproduce: {env['reproduce']}")
    return truncate("\n".join(lines), BODY_MAX)


def comment_body(env: dict) -> str:
    """幂等命中时追加的 comment:新 pipeline 摘要(设计 §4 单一事实源)。"""
    src, rule = env["source"], env["rule"]
    return (
        f"再次触发: {src['project']} g{src['commit']} p{src['pipeline_iid']} "
        f"{src['variant']} — {rule['verdict']}/{rule['category']}: {rule['reason']}"
    )


def run_cli(args: list[str]) -> str:
    """参数数组执行(非 shell 拼接);非零/超时抛 BridgeError(502)。"""
    try:
        cp = subprocess.run(args, capture_output=True, text=True, timeout=TIMEOUT)
    except subprocess.TimeoutExpired:
        raise BridgeError(502, f"docker exec 超时({TIMEOUT}s)")
    if cp.returncode != 0:
        raise BridgeError(502, f"docker exec 退出码 {cp.returncode}: {cp.stderr[-ERR_SNIPPET:]}")
    return cp.stdout


def kanban(*args: str) -> str:
    return run_cli([DOCKER_BIN, "exec", CONTAINER, "hermes", "kanban", "--board", BOARD, *args])


def create_escalation(env: dict) -> dict:
    title = render_title(env)
    body = render_body(env)
    out = kanban("create", title, "--assignee", ASSIGNEE,
                 "--idempotency-key", env["idempotency_key"], "--body", body)
    m = CREATED_RE.search(out)
    if m:
        task_id, result = m.group(1), "created"
    else:
        # 幂等键重复:kanban 内建去重,返回同一 task id(无 "Created" 字样)
        m = TASK_ID_RE.search(out)
        if not m:
            raise BridgeError(502, f"kanban 输出无法解析 task id: {out[-ERR_SNIPPET:]}")
        task_id, result = m.group(1), "existing"
        comment = comment_body(env)
        kanban("comment", task_id, comment)
        log.info("escalation dedup hit, commented: task=%s key=%s", task_id, env["idempotency_key"])
    log.info("escalation %s: task=%s key=%s", result, task_id, env["idempotency_key"])
    return {"kanban_task_id": task_id, "result": result}


@app.get("/health")
def health():
    return {"status": "ok"}


@app.post("/escalations")
async def escalations(req: Request):
    if err := check_auth(req):
        return err
    try:
        env = await req.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "请求体不是合法 JSON"}, status_code=400)
    try:
        jsonschema.validate(env, ESCALATION_SCHEMA)
    except jsonschema.ValidationError as e:
        return JSONResponse({"error": f"escalation schema: {e.message[:ERR_SNIPPET]}"}, status_code=400)
    try:
        return await run_in_threadpool(create_escalation, env)
    except BridgeError as e:
        log.error("escalation failed: %s", e.msg)
        return JSONResponse({"error": e.msg}, status_code=e.status)
