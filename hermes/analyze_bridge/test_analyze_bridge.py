"""analyze_bridge 的 pytest 套件。

用可执行脚本假冒 hermes CLI(HERMES_BIN 注入),覆盖:鉴权、成功、
markdown 围栏容忍、Schema 校验打回重试、重试耗尽 502、CLI 失败 502、
模型透传、契约漂移(schema 副本必须与 contracts/ 一致)。
"""

import json
import os
import stat
import subprocess
import sys
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

sys.path.insert(0, str(Path(__file__).parent))

import analyze_bridge as ab  # noqa: E402

VALID_ANALYSIS = {
    "analysis_version": 1,
    "summary": "cpu fallback 导致性能下降",
    "root_cause": "签名命中 cpu_fallback",
    "suggested_category": "DELEGATE",
    "confidence": 0.85,
    "next_actions": ["检查 delegate 分区"],
    "disagrees_with_rule": False,
}

PAYLOAD = {
    "task_id": "wf:t1:a1",
    "prompt_version": "analyze_v1",
    "prompt": "你是设备测试分析器……",
    "rule_category": "MODEL",
    "evidence": {"evidence_version": 1, "task_id": "wf:t1:a1"},
}


@pytest.fixture()
def fake_hermes(tmp_path, monkeypatch):
    """写一个假的 hermes 可执行文件:按调用序号从响应目录取 stdout。

    环境变量 FAKE_RESP_DIR 下的 resp1.json/resp2.json/... 依次作为各次
    调用的 stdout;序号超出后回落 resp_default.json;内容为 "EXIT:n" 前缀
    时以退出码 n 失败。调用参数追加记录到 calls.log。
    """
    resp_dir = tmp_path / "resp"
    resp_dir.mkdir()
    calls_log = tmp_path / "calls.log"
    counter = tmp_path / "counter"
    script = tmp_path / "fake-hermes"
    script.write_text(
        "#!/bin/sh\n"
        f'for a in "$@"; do echo "ARG:$a"; done >> "{calls_log}"\n'
        f'echo "END-CALL" >> "{calls_log}"\n'
        f'n=$(cat "{counter}" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "{counter}"\n'
        f'resp="{resp_dir}/resp$n.json"\n'
        f'[ -f "$resp" ] || resp="{resp_dir}/resp_default.json"\n'
        'body=$(cat "$resp")\n'
        'case "$body" in EXIT:*) echo "fake cli failure" >&2; exit "${body#EXIT:}";; esac\n'
        'printf "%s" "$body"\n',
        encoding="utf-8",
    )
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setattr(ab, "HERMES_BIN", str(script))
    return resp_dir


def put(resp_dir: Path, name: str, body) -> None:
    resp_dir.joinpath(name).write_text(
        body if isinstance(body, str) else json.dumps(body), encoding="utf-8"
    )


@pytest.fixture()
def client(monkeypatch):
    monkeypatch.setenv("ANALYZE_BRIDGE_TOKEN", "test-token")
    return TestClient(ab.app)


def auth():
    return {"Authorization": "Bearer test-token"}


def test_health(client):
    assert client.get("/health").status_code == 200


def test_auth_required(client, fake_hermes):
    assert client.post("/analyze", json=PAYLOAD).status_code == 401
    assert client.post(
        "/analyze", json=PAYLOAD, headers={"Authorization": "Bearer wrong"}
    ).status_code == 401


def test_missing_token_config(client, fake_hermes, monkeypatch):
    monkeypatch.delenv("ANALYZE_BRIDGE_TOKEN")
    r = client.post("/analyze", json=PAYLOAD, headers=auth())
    assert r.status_code == 500


def test_missing_fields(client, fake_hermes):
    r = client.post("/analyze", json={"task_id": "t"}, headers=auth())
    assert r.status_code == 400


def test_success(client, fake_hermes):
    put(fake_hermes, "resp_default.json", VALID_ANALYSIS)
    r = client.post("/analyze", json=PAYLOAD, headers=auth())
    assert r.status_code == 200
    got = r.json()
    assert got["summary"] == VALID_ANALYSIS["summary"]
    assert got["analysis_version"] == 1


def test_markdown_fence_tolerated(client, fake_hermes):
    put(fake_hermes, "resp_default.json", "```json\n" + json.dumps(VALID_ANALYSIS) + "\n```")
    r = client.post("/analyze", json=PAYLOAD, headers=auth())
    assert r.status_code == 200


def test_retry_on_schema_invalid_then_success(client, fake_hermes):
    put(fake_hermes, "resp1.json", {"summary": "缺字段、analysis_version 错"})
    put(fake_hermes, "resp2.json", VALID_ANALYSIS)
    r = client.post("/analyze", json=PAYLOAD, headers=auth())
    assert r.status_code == 200
    # 第二次调用的 prompt 应携带上次的校验错误(打回重试);
    # 首次 prompt 不含该提示,出现即证明重试生效
    calls = fake_hermes.parent.joinpath("calls.log").read_text(encoding="utf-8")
    assert calls.count("ARG:-z\n") == 2
    assert "未通过 analysis.schema.json 校验" in calls


def test_retry_exhausted_returns_502(client, fake_hermes, monkeypatch):
    monkeypatch.setattr(ab, "MAX_ATTEMPTS", 2)
    put(fake_hermes, "resp_default.json", "this is not json at all")
    r = client.post("/analyze", json=PAYLOAD, headers=auth())
    assert r.status_code == 502
    assert "Schema" in r.json()["error"]


def test_cli_failure_returns_502(client, fake_hermes):
    put(fake_hermes, "resp_default.json", "EXIT:1")
    r = client.post("/analyze", json=PAYLOAD, headers=auth())
    assert r.status_code == 502
    assert "退出码 1" in r.json()["error"]


def test_model_passthrough(client, fake_hermes):
    put(fake_hermes, "resp_default.json", VALID_ANALYSIS)
    payload = {**PAYLOAD, "model": "deepseek/deepseek-v4-pro"}
    r = client.post("/analyze", json=payload, headers=auth())
    assert r.status_code == 200
    calls = fake_hermes.parent.joinpath("calls.log").read_text(encoding="utf-8")
    assert "ARG:-m\nARG:deepseek/deepseek-v4-pro\n" in calls


def test_toolsets_disabled(client, fake_hermes):
    """§3 工具白名单:每次调用必须 -t ""(空工具集)。"""
    put(fake_hermes, "resp_default.json", VALID_ANALYSIS)
    r = client.post("/analyze", json=PAYLOAD, headers=auth())
    assert r.status_code == 200
    calls = fake_hermes.parent.joinpath("calls.log").read_text(encoding="utf-8")
    # 空工具集参数呈现为 ARG:-t 紧跟一个空 ARG
    assert "ARG:-t\nARG:\n" in calls


def test_timeout_returns_502(client, tmp_path, monkeypatch):
    slow = tmp_path / "slow-hermes"
    slow.write_text("#!/bin/sh\nsleep 5\n", encoding="utf-8")
    slow.chmod(slow.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setattr(ab, "HERMES_BIN", str(slow))
    monkeypatch.setattr(ab, "HERMES_TIMEOUT", 1)
    r = client.post("/analyze", json=PAYLOAD, headers=auth())
    assert r.status_code == 502
    assert "超时" in r.json()["error"]


def test_schema_copy_matches_contracts():
    """防契约漂移:bridge 内嵌副本必须与 contracts/analysis.schema.json 一致。"""
    contracts = (
        Path(__file__).resolve().parents[2] / "contracts" / "analysis.schema.json"
    )
    assert json.loads(contracts.read_text(encoding="utf-8")) == json.loads(
        ab.SCHEMA_PATH.read_text(encoding="utf-8")
    ), "analysis.schema.json 与 contracts/ 不一致,请重新拷贝"
