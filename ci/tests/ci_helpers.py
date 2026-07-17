"""ci/ 测试的路径常量与工具(独立模块,避免 conftest 重名冲突)。"""
import hashlib
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
CI_DIR = REPO_ROOT / "ci"
CONTRACTS_DIR = REPO_ROOT / "contracts"
VARIANTS_FILE = CI_DIR / "variants.yaml"
MANIFEST_SCHEMA = CONTRACTS_DIR / "manifest.schema.json"
BUNDLE_SCHEMA = CONTRACTS_DIR / "bundle.schema.json"

# 假包内容: 相对路径 → (字节内容, mode)
FAKE_FILES = {
    "bin/bench_tool": (b"\x7fELF-fake-binary", 0o755),
    "lib/libfoo.so": (b"\x7fELF-fake-lib", 0o644),
    "run.sh": (b"#!/system/bin/sh\nexit 0\n", 0o755),
    "models/net.bin": (b"MODEL-DATA", 0o644),
}


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()
