"""analyze_bridge — hermes-agent 平台的 Analyzer HTTP 适配层(CLAUDE.md §4/§12 Phase 2)。

Runtime hermesclient POST /analyze → hermes -z 一次性调用(工具集全禁,-t "",
§3 工具白名单)→ stdout 提取 JSON → analysis.schema.json 校验,不过则把校验
错误喂回重试(≤ ANALYZE_MAX_ATTEMPTS 次)→ 原样返回 analysis JSON。
重试耗尽或 CLI 失败返回 502,Runtime 按 §9 降级到规则引擎保底。

部署形态:跑在专用 hermes-agent 实例容器内(同 queinfer_gitlab_bridge),
provider 凭据/模型配置全部由实例持有,本服务不感知。

配置(环境变量):
  ANALYZE_BRIDGE_TOKEN   Bearer 共享密钥(必填;对应 Runtime HERMES_AUTH_TOKEN)
  HERMES_BIN             hermes CLI 路径,缺省 hermes
  HERMES_TIMEOUT_SEC     单次 hermes -z 调用超时,缺省 120(实测 -t "" 约 13s)
  ANALYZE_MAX_ATTEMPTS   Schema 校验打回重试上限,缺省 3
"""

import hmac
import json
import logging
import os
import subprocess
from pathlib import Path

import jsonschema
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from starlette.concurrency import run_in_threadpool

log = logging.getLogger("analyze_bridge")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

HERMES_BIN = os.environ.get("HERMES_BIN", "hermes")
HERMES_TIMEOUT = int(os.environ.get("HERMES_TIMEOUT_SEC", "120"))
MAX_ATTEMPTS = int(os.environ.get("ANALYZE_MAX_ATTEMPTS", "3"))
# 错误内容截断长度,防止日志被刷爆
ERR_SNIPPET = 500

SCHEMA_PATH = Path(__file__).with_name("analysis.schema.json")
ANALYSIS_SCHEMA = json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))

COMMAND_SCHEMA_PATH = Path(__file__).with_name("command.schema.json")
COMMAND_SCHEMA = json.loads(COMMAND_SCHEMA_PATH.read_text(encoding="utf-8"))

TRANSLATE_REQUIRED_FIELDS = ("prompt", "raw_text", "context")

app = FastAPI(title="analyze_bridge", version="1")

REQUIRED_FIELDS = ("task_id", "prompt", "rule_category", "evidence")


class BridgeError(Exception):
    def __init__(self, status: int, msg: str):
        super().__init__(msg)
        self.status = status
        self.msg = msg


def check_auth(req: Request) -> JSONResponse | None:
    token = os.environ.get("ANALYZE_BRIDGE_TOKEN", "")
    if not token:
        return JSONResponse({"error": "ANALYZE_BRIDGE_TOKEN 未配置"}, status_code=500)
    want = "Bearer " + token
    if not hmac.compare_digest(req.headers.get("authorization", ""), want):
        return JSONResponse({"error": "unauthorized"}, status_code=401)
    return None


def build_prompt(payload: dict, prev_errors: list[str], schema_name: str = "analysis.schema.json") -> str:
    """拼一次性 prompt:平台 prompt 模板 + 规则类别 + evidence + (重试时)校验错误。"""
    parts = [
        payload["prompt"],
        "",
        f"规则引擎判定类别(rule_category): {payload['rule_category']}",
        "",
        "evidence JSON:",
        json.dumps(payload["evidence"], ensure_ascii=False),
    ]
    if prev_errors:
        parts += [
            "",
            f"注意:你上一次的输出未通过 {schema_name} 校验,错误如下。",
            "这次只输出修正后的 JSON 对象本身,不要任何其他文本:",
            *[f"- {e}" for e in prev_errors[-2:]],  # 只带最近两条,控制长度
        ]
    return "\n".join(parts)


def build_translate_prompt(payload: dict, prev_errors: list[str]) -> str:
    """翻译 prompt:平台 prompt 模板 + 只读上下文快照 + 用户原文 + (重试时)校验错误。"""
    parts = [
        payload["prompt"],
        "",
        "运行时上下文快照 JSON:",
        json.dumps(payload["context"], ensure_ascii=False),
        "",
        "用户原话:",
        payload["raw_text"],
    ]
    if prev_errors:
        parts += [
            "",
            "注意:你上一次的输出未通过 command.schema.json 校验,错误如下。",
            "这次只输出修正后的 JSON 对象本身,不要任何其他文本:",
            *[f"- {e}" for e in prev_errors[-2:]],
        ]
    return "\n".join(parts)


def extract_json(text: str) -> dict:
    """从 hermes -z 的 stdout 提取 JSON 对象。容忍 markdown 代码围栏与首尾杂讯。"""
    s = text.strip()
    if s.startswith("```"):
        lines = [ln for ln in s.splitlines() if not ln.strip().startswith("```")]
        s = "\n".join(lines).strip()
    try:
        doc = json.loads(s)
    except json.JSONDecodeError:
        lo, hi = s.find("{"), s.rfind("}")
        if lo < 0 or hi <= lo:
            raise ValueError(f"stdout 中找不到 JSON 对象: {s[:ERR_SNIPPET]}")
        doc = json.loads(s[lo : hi + 1])
    if not isinstance(doc, dict):
        raise ValueError("输出不是 JSON 对象")
    return doc


def run_hermes(prompt: str, model: str | None) -> str:
    """调一次 hermes -z(工具集全禁,§3 工具白名单);返回 stdout。"""
    cmd = [HERMES_BIN, "-z", prompt, "-t", ""]
    if model:
        cmd += ["-m", model]
    try:
        cp = subprocess.run(cmd, capture_output=True, text=True, timeout=HERMES_TIMEOUT)
    except subprocess.TimeoutExpired:
        raise BridgeError(502, f"hermes -z 超时({HERMES_TIMEOUT}s)")
    if cp.returncode != 0:
        raise BridgeError(502, f"hermes -z 退出码 {cp.returncode}: {cp.stderr[-ERR_SNIPPET:]}")
    return cp.stdout


def run_with_schema(payload: dict, schema: dict, prompt_builder, log_ok, log_retry, fallback_msg: str) -> dict:
    """调 hermes 并做 Schema 校验打回重试;全部失败抛 BridgeError(502)。
    校验用哪份 schema 由路由写死,不接受请求方指定(契约选择权不外放)。
    log_ok(attempt) / log_retry(attempt, err) 由各路由提供,
    使每条路由保留自己的日志文案(analyze 的文案不得因抽取而改变)。"""
    errors: list[str] = []
    for attempt in range(1, MAX_ATTEMPTS + 1):
        stdout = run_hermes(prompt_builder(payload, errors), payload.get("model") or None)
        try:
            doc = extract_json(stdout)
            jsonschema.validate(doc, schema)
            log_ok(attempt)
            return doc
        except (ValueError, jsonschema.ValidationError) as e:
            log_retry(attempt, str(e)[:ERR_SNIPPET])
            errors.append(str(e)[:ERR_SNIPPET])
    raise BridgeError(502, f"输出连续 {MAX_ATTEMPTS} 次未通过 Schema 校验,{fallback_msg}")


def run_analysis(payload: dict) -> dict:
    def log_ok(attempt: int) -> None:
        log.info("analyze ok: task=%s attempt=%d", payload.get("task_id"), attempt)

    def log_retry(attempt: int, err: str) -> None:
        log.warning("analyze attempt %d 校验失败: %s", attempt, err)

    return run_with_schema(payload, ANALYSIS_SCHEMA, build_prompt, log_ok, log_retry, "降级规则引擎")


def run_translation(payload: dict) -> dict:
    def log_ok(attempt: int) -> None:
        log.info("translate ok: attempt=%d", attempt)

    def log_retry(attempt: int, err: str) -> None:
        log.warning("translate attempt %d 校验失败: %s", attempt, err)

    return run_with_schema(payload, COMMAND_SCHEMA, build_translate_prompt, log_ok, log_retry, "降级调用方保底")


@app.get("/health")
def health():
    return {"status": "ok"}


@app.post("/analyze")
async def analyze(req: Request):
    if err := check_auth(req):
        return err
    try:
        payload = await req.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "请求体不是合法 JSON"}, status_code=400)
    if not isinstance(payload, dict) or any(k not in payload for k in REQUIRED_FIELDS):
        return JSONResponse({"error": f"缺少必填字段: {REQUIRED_FIELDS}"}, status_code=400)
    try:
        # subprocess 阻塞调用放线程池,不卡事件循环
        return await run_in_threadpool(run_analysis, payload)
    except BridgeError as e:
        log.error("analyze failed: task=%s %s", payload.get("task_id"), e.msg)
        return JSONResponse({"error": e.msg}, status_code=e.status)


@app.post("/translate")
async def translate(req: Request):
    """飞书指令层意图翻译(设计文档 §3.3):自然语言 + 只读上下文快照 → 一条封闭
    枚举指令。与 /analyze 同构:工具集全禁、服务端定形校验、打回重试同一上限。"""
    if err := check_auth(req):
        return err
    try:
        payload = await req.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "请求体不是合法 JSON"}, status_code=400)
    if not isinstance(payload, dict) or any(k not in payload for k in TRANSLATE_REQUIRED_FIELDS):
        return JSONResponse({"error": f"缺少必填字段: {TRANSLATE_REQUIRED_FIELDS}"}, status_code=400)
    try:
        return await run_in_threadpool(run_translation, payload)
    except BridgeError as e:
        log.error("translate failed: %s", e.msg)
        return JSONResponse({"error": e.msg}, status_code=e.status)
