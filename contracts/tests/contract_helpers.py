"""契约测试的路径与加载工具(独立模块,避免与其他 tests 目录的 conftest 重名冲突)。"""
import json
from pathlib import Path

import yaml

CONTRACTS_DIR = Path(__file__).resolve().parent.parent
EXAMPLES_DIR = Path(__file__).resolve().parent / "examples"


def load_schema(name: str) -> dict:
    path = CONTRACTS_DIR / f"{name}.schema.json"
    with path.open(encoding="utf-8") as f:
        return json.load(f)


def load_example(path: Path) -> dict:
    with path.open(encoding="utf-8") as f:
        if path.suffix in (".yaml", ".yml"):
            return yaml.safe_load(f)
        return json.load(f)


def example_files(contract: str, kind: str) -> list[Path]:
    """kind: valid | invalid。为空目录/缺目录时报错,防止测试静默变空。"""
    d = EXAMPLES_DIR / contract / kind
    files = sorted(p for p in d.glob("*") if p.suffix in (".json", ".yaml", ".yml"))
    if not files:
        raise FileNotFoundError(f"no {kind} examples for {contract} in {d}")
    return files
