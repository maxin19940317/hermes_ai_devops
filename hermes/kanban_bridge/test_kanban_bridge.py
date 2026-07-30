"""kanban_bridge 的 pytest 套件。

用可执行脚本假冒 docker CLI(KANBAN_DOCKER_BIN 注入),覆盖:鉴权 401、
Schema 400、create 成功渲染(title/body/参数数组)、幂等命中 → existing +
comment、CLI 非零 502、body 8KB 截断、title 200 截断、契约漂移。
"""

import json
import os
import stat
import sys
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

sys.path.insert(0, str(Path(__file__).parent))

os.environ.setdefault("KANBAN_BRIDGE_TOKEN", "test-token")
import kanban_bridge as kb  # noqa: E402

TOKEN = "test-token"
AUTH = {"Authorization": f"Bearer {TOKEN}"}

ENVELOPE = {
    "escalation_version": 1,
    "idempotency_key": "devops-escalation:aios/algo_super_sdk:def41bec:aarch64_Android_SNPE_2.21:dsp_unavailable",
    "source": {
        "project": "aios/algo_super_sdk",
        "commit": "def41bec",
        "pipeline_iid": 53,
        "variant": "aarch64_Android_SNPE_2.21",
        "task_id": "device-test-aios/algo_super_sdk-gdef41bec-p53:aarch64_Android_SNPE_2.21:a1",
    },
    "rule": {"verdict": "TEST_FAILED", "category": "DELEGATE", "reason": "signature hit: dsp_unavailable"},
    "hermes": {
        "summary": "gesture DSP 不支持 unsigned PD",
        "root_cause": "SNPE 2.21 DSP 委派失败",
        "suggested_category": "DELEGATE",
        "confidence": 0.92,
        "next_actions": ["检查 delegate 分区"],
    },
    "evidence": {
        "snapshot_id": "snap-1",
        "object_key": "evidence/w:t:a1/evidence.json",
        "sha256": "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0",
        "extractor_version": "1",
    },
}


@pytest.fixture()
def fake_docker(tmp_path, monkeypatch):
    """假 docker:按 FAKE_OUT 内容应答;EXIT:n 前缀 → 退出码 n。
    参数逐行追加到 calls.log(每个参数一行,调用间以 END-CALL 分隔)。"""
    calls_log = tmp_path / "calls.log"
    out_file = tmp_path / "stdout.txt"
    script = tmp_path / "fake-docker"
    script.write_text(
        "#!/bin/sh\n"
        # 每个参数 base64 单行记录(body 含换行,原文记录会破坏行结构)
        f'for a in "$@"; do printf %s "$a" | base64 -w0; echo; done >> "{calls_log}"\n'
        f'echo "END-CALL" >> "{calls_log}"\n'
        f'out=$(cat "{out_file}" 2>/dev/null || echo "Created t_01343ec8")\n'
        'case "$out" in EXIT:*) code=${out#EXIT:}; echo "fake error" >&2; exit "$code";; esac\n'
        'echo "$out"\n'
    )
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setattr(kb, "DOCKER_BIN", str(script))
    return {"calls": calls_log, "out": out_file}


def call_args(calls_log: Path) -> list[list[str]]:
    """解析 calls.log → 每次调用的参数列表(参数按 base64 单行存储)。"""
    import base64

    if not calls_log.exists():
        return []
    calls, cur = [], []
    for line in calls_log.read_text().splitlines():
        if line == "END-CALL":
            calls.append(cur)
            cur = []
        else:
            cur.append(base64.b64decode(line).decode())
    return calls


def client() -> TestClient:
    return TestClient(kb.app)


def test_auth_required(fake_docker):
    resp = client().post("/escalations", json=ENVELOPE)
    assert resp.status_code == 401


def test_schema_violation_400(fake_docker):
    bad = dict(ENVELOPE)
    bad["rule"] = {"verdict": "TEST_FAILED", "category": "HARDWARE", "reason": "x"}
    resp = client().post("/escalations", json=bad, headers=AUTH)
    assert resp.status_code == 400
    assert not fake_docker["calls"].exists()


def test_create_success_renders(fake_docker):
    resp = client().post("/escalations", json=ENVELOPE, headers=AUTH)
    assert resp.status_code == 200
    assert resp.json() == {"kanban_task_id": "t_01343ec8", "result": "created"}
    calls = call_args(fake_docker["calls"])
    assert len(calls) == 1
    args = calls[0]
    # 参数数组形态:docker exec <container> hermes kanban --board <B> create <title> ...
    assert args[:5] == ["exec", "hermes-rocklin", "hermes", "kanban", "--board"]
    assert args[5] == "algo_super_sdk" and args[6] == "create"
    title = args[7]
    assert title == "[DELEGATE] aarch64_Android_SNPE_2.21 @ def41bec: signature hit: dsp_unavailable"
    assert "--assignee" in args and args[args.index("--assignee") + 1] == "tobias_pm"
    assert args[args.index("--idempotency-key") + 1] == ENVELOPE["idempotency_key"]
    body = args[args.index("--body") + 1]
    assert "p53" in body and "unsigned PD" in body and "evidence/w:t:a1/evidence.json" in body


def test_existing_key_comments_instead(fake_docker):
    # kanban 内建去重:重复 key 返回同一 task id(无 "Created" 字样)
    fake_docker["out"].write_text("Task t_01343ec8 already exists for key\n")
    resp = client().post("/escalations", json=ENVELOPE, headers=AUTH)
    assert resp.status_code == 200
    assert resp.json() == {"kanban_task_id": "t_01343ec8", "result": "existing"}
    calls = call_args(fake_docker["calls"])
    assert len(calls) == 2, "create + comment 两次调用"
    assert calls[1][6] == "comment" and calls[1][7] == "t_01343ec8"
    assert "p53" in calls[1][8], "comment 应含新 pipeline 摘要"


def test_cli_failure_502(fake_docker):
    fake_docker["out"].write_text("EXIT:1")
    resp = client().post("/escalations", json=ENVELOPE, headers=AUTH)
    assert resp.status_code == 502


def test_unparsable_output_502(fake_docker):
    fake_docker["out"].write_text("something unexpected\n")
    resp = client().post("/escalations", json=ENVELOPE, headers=AUTH)
    assert resp.status_code == 502


def test_body_truncated_at_8k(fake_docker):
    env = json.loads(json.dumps(ENVELOPE))
    env["reproduce"] = "x" * 20000
    resp = client().post("/escalations", json=env, headers=AUTH)
    assert resp.status_code == 200
    body = call_args(fake_docker["calls"])[0][-1]
    assert len(body) <= 8 * 1024
    assert body.endswith("…(truncated)")


def test_title_truncated_at_200(fake_docker):
    env = json.loads(json.dumps(ENVELOPE))
    env["rule"]["reason"] = "r" * 500
    resp = client().post("/escalations", json=env, headers=AUTH)
    assert resp.status_code == 200
    title = call_args(fake_docker["calls"])[0][7]
    assert len(title) <= 200
    assert title.endswith("…(truncated)")


def test_schema_copy_matches_contracts():
    """防契约漂移:bridge 内嵌副本必须与 contracts/escalation.schema.json 一致。"""
    contracts = (
        Path(__file__).resolve().parents[2] / "contracts" / "escalation.schema.json"
    )
    assert json.loads(contracts.read_text(encoding="utf-8")) == json.loads(
        kb.SCHEMA_PATH.read_text(encoding="utf-8")
    ), "escalation.schema.json 与 contracts/ 不一致,请重新拷贝"
