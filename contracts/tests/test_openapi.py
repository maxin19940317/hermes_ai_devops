"""两个 OpenAPI 契约 (CLAUDE.md §8) 的规范合法性测试。"""
from pathlib import Path

import pytest
import yaml
from openapi_spec_validator import validate as validate_openapi

from contract_helpers import CONTRACTS_DIR

SPECS = ["client-agent-api.openapi.yaml", "callbacks-api.openapi.yaml"]


@pytest.mark.parametrize("spec_name", SPECS)
def test_openapi_spec_is_valid(spec_name):
    path = CONTRACTS_DIR / spec_name
    with path.open(encoding="utf-8") as f:
        spec = yaml.safe_load(f)
    validate_openapi(spec)


def test_client_agent_api_covers_required_paths():
    """§8.1 要求的端点必须全部存在。"""
    with (CONTRACTS_DIR / "client-agent-api.openapi.yaml").open(encoding="utf-8") as f:
        spec = yaml.safe_load(f)
    paths = spec["paths"]
    assert "post" in paths["/api/v1/tasks"]
    assert "delete" in paths["/api/v1/tasks/{task_id}"]
    assert "get" in paths["/api/v1/tasks/{task_id}"]
    assert "get" in paths["/api/v1/devices"]
    assert "post" in paths["/api/v1/diagnostics"]
    assert "get" in paths["/healthz"]
    # 派单仅确认"已入本地队列",必须是 202
    assert "202" in paths["/api/v1/tasks"]["post"]["responses"]


def test_callbacks_api_covers_required_paths():
    """§8.2 要求的回调端点必须全部存在。"""
    with (CONTRACTS_DIR / "callbacks-api.openapi.yaml").open(encoding="utf-8") as f:
        spec = yaml.safe_load(f)
    paths = spec["paths"]
    assert "post" in paths["/callbacks/v1/heartbeat"]
    assert "post" in paths["/callbacks/v1/task-events"]
    assert "post" in paths["/callbacks/v1/results"]


def _load_callbacks_spec():
    with (CONTRACTS_DIR / "callbacks-api.openapi.yaml").open(encoding="utf-8") as f:
        return yaml.safe_load(f)


def _resolve_local_refs(schema, spec):
    """把 '#/components/schemas/X' 形式的本地 $ref 就地展开(测试用,非通用解析器)。"""
    import copy

    schema = copy.deepcopy(schema)
    if isinstance(schema, dict):
        ref = schema.get("$ref")
        if isinstance(ref, str) and ref.startswith("#/components/schemas/"):
            name = ref.rsplit("/", 1)[-1]
            return _resolve_local_refs(spec["components"]["schemas"][name], spec)
        return {k: _resolve_local_refs(v, spec) for k, v in schema.items()}
    if isinstance(schema, list):
        return [_resolve_local_refs(v, spec) for v in schema]
    return schema


def test_heartbeat_active_tasks_dual_format():
    """心跳 active_task_ids 过渡期双格式(差距 #15)正反例:
    对象凭据格式与旧字符串格式都必须合法;凭据缺字段/类型错必须被拒。"""
    from jsonschema import Draft202012Validator, ValidationError

    spec = _load_callbacks_spec()
    schema = _resolve_local_refs(spec["components"]["schemas"]["Heartbeat"], spec)
    validator = Draft202012Validator(schema)

    def heartbeat(active_task_ids):
        return {
            "client_id": "c1",
            "agent_version": "0.2.0",
            "ts": "2026-07-24T08:00:00.000Z",
            "devices": [],
            "active_task_ids": active_task_ids,
        }

    cred = {"task_id": "w:t:a1", "attempt": 1, "lease_id": "w:t:a1", "lease_generation": 3}
    # 正例:新凭据格式 / 旧字符串格式 / 混排
    validator.validate(heartbeat([cred]))
    validator.validate(heartbeat(["w:t:a1"]))
    validator.validate(heartbeat([cred, "w:t:a2"]))
    # 反例:凭据缺 lease_generation / generation 非整数 / 数组元素为无关对象
    for bad in (
        [{k: v for k, v in cred.items() if k != "lease_generation"}],
        [{**cred, "lease_generation": "3"}],
        [{"foo": "bar"}],
        [42],
    ):
        try:
            validator.validate(heartbeat(bad))
        except ValidationError:
            pass
        else:
            raise AssertionError(f"非法心跳被接受: {bad}")


def test_heartbeat_ack_not_owned_shape():
    """HeartbeatAck.not_owned(LEASE_NOT_OWNED)结构正反例(§10)。"""
    from jsonschema import Draft202012Validator, ValidationError

    spec = _load_callbacks_spec()
    schema = _resolve_local_refs(spec["components"]["schemas"]["HeartbeatAck"], spec)
    validator = Draft202012Validator(schema)

    validator.validate({"ok": True})  # 无 not_owned = 全部续租成功
    validator.validate({"ok": True, "not_owned": [{"task_id": "w:t:a1", "code": "LEASE_NOT_OWNED"}]})
    for bad in (
        {"ok": True, "not_owned": [{"task_id": "w:t:a1", "code": "OTHER_CODE"}]},
        {"ok": True, "not_owned": [{"task_id": "w:t:a1"}]},
    ):
        try:
            validator.validate(bad)
        except ValidationError:
            pass
        else:
            raise AssertionError(f"非法 ack 被接受: {bad}")
