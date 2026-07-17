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
