"""契约校验测试的共享 fixtures。

目录约定:
    contracts/
    ├── {plan,manifest,result,bundle}.schema.json
    ├── client-agent-api.openapi.yaml / callbacks-api.openapi.yaml
    └── tests/examples/<contract>/{valid,invalid}/*.{json,yaml}

反例文件名即失败原因(如 missing_plan_version.json),便于定位。
"""
import pytest

from contract_helpers import example_files, load_schema


def pytest_generate_tests(metafunc):
    """按 fixtures 目录自动参数化 valid_case / invalid_case。"""
    contract = getattr(metafunc.cls, "contract", None)
    if contract is None:
        return
    if "valid_case" in metafunc.fixturenames:
        files = example_files(contract, "valid")
        metafunc.parametrize("valid_case", files, ids=[p.stem for p in files])
    if "invalid_case" in metafunc.fixturenames:
        files = example_files(contract, "invalid")
        metafunc.parametrize("invalid_case", files, ids=[p.stem for p in files])


@pytest.fixture(scope="session")
def validators():
    """按契约名返回已编译的 Draft 2020-12 validator。"""
    from jsonschema import Draft202012Validator

    result = {}
    for name in ("plan", "manifest", "result", "bundle", "evidence", "analysis"):
        schema = load_schema(name)
        Draft202012Validator.check_schema(schema)  # schema 本身必须合法
        result[name] = Draft202012Validator(schema)
    return result
