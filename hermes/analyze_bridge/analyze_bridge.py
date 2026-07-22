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
import tempfile
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


def build_prompt(payload: dict, prev_errors: list[str]) -> str:
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
            "注意:你上一次的输出未通过 analysis.schema.json 校验,错误如下。",
            "这次只输出修正后的 JSON 对象本身,不要任何其他文本:",
            *[f"- {e}" for e in prev_errors[-2:]],  # 只带最近两条,控制长度
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
    with tempfile.NamedTemporaryFile(suffix=".usage.json", delete=False) as f:
        usage_path = f.name
    cmd += ["--usage-file", usage_path]
    try:
        cp = subprocess.run(cmd, capture_output=True, text=True, timeout=HERMES_TIMEOUT)
    except subprocess.TimeoutExpired:
        raise BridgeError(502, f"hermes -z 超时({HERMES_TIMEOUT}s)")
    finally:
        # usage 报告(成本/token)只落日志,不进响应(契约固定)
        try:
            usage = Path(usage_path).read_text(encoding="utf-8")
            if usage.strip():
                log.info("hermes usage: %s", usage.strip()[:ERR_SNIPPET])
        except OSError:
            pass
        Path(usage_path).unlink(missing_ok=True)
    if cp.returncode != 0:
        raise BridgeError(502, f"hermes -z 退出码 {cp.returncode}: {cp.stderr[-ERR_SNIPPET:]}")
    return cp.stdout


def run_analysis(payload: dict) -> dict:
    """执行分析并做 Schema 校验打回重试;全部失败抛 BridgeError(502)。"""
    errors: list[str] = []
    for attempt in range(1, MAX_ATTEMPTS + 1):
        stdout = run_hermes(build_prompt(payload, errors), payload.get("model") or None)
        try:
            doc = extract_json(stdout)
            jsonschema.validate(doc, ANALYSIS_SCHEMA)
            log.info("analyze ok: task=%s attempt=%d", payload.get("task_id"), attempt)
            return doc
        except (ValueError, jsonschema.ValidationError) as e:
            log.warning("analyze attempt %d 校验失败: %s", attempt, str(e)[:ERR_SNIPPET])
            errors.append(str(e)[:ERR_SNIPPET])
    raise BridgeError(502, f"输出连续 {MAX_ATTEMPTS} 次未通过 Schema 校验,降级规则引擎")


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
