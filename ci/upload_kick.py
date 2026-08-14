#!/usr/bin/env python3
"""upload_kick.py — 本地测试包一条龙:上传 MinIO → 解析 manifest → kick 登记 → 触发测试。

解决"包在服务器本地、未在 artifacts 表登记、想测它"的缺口(2026-08-14):
Hermes 的 devops_test 只接受变体名并从 artifacts 表找包;本脚本把本地包
变为可测的已登记产物。

流程:
    1. 从本地 tar.gz 提取 manifest.yaml(拿 requirements/failure_signatures/
       artifact.project/version 等),校验 manifest.schema.json
    2. 上传包到 MinIO hermes-packages 桶(公开读,匿名探活/下载)
    3. 构造 kick 载荷(url 指向 MinIO,requirements/signatures 来自 manifest)
    4. POST Trigger /kick → 起单变体 workflow

之后 Hermes 侧仍用 devops_test <variant> 即可(包已登记为该变体最近产物)。
需要 minio python 客户端或 docker + mc。

用法:
    python3 upload_kick.py --package /tmp/xxx.tar.gz \
        --variant aarch64_Linux_QCS6490_SNPE_2.21 \
        --project aios/algo_super_sdk --version 1.0.2 --commit 5c885dbb \
        --pipeline-iid 9993 --pipeline-global-id 88882 \
        [--minio-bucket hermes-packages] [--minio-public-base http://10.88.118.251:9000]
        [--schema contracts/manifest.schema.json]
"""
import argparse
import hashlib
import json
import os
import subprocess
import sys
import tarfile
from pathlib import Path
from typing import Optional

import yaml

REPO_ROOT = Path(__file__).resolve().parent.parent

try:
    from jsonschema import Draft202012Validator
except ImportError:  # pragma: no cover
    sys.exit("upload_kick.py requires jsonschema: pip install jsonschema")


def _sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def extract_manifest(package: Path):
    """从 tar.gz 提取 manifest.yaml(顶层或任意层级,取第一个)。

    返回 (解析后的 dict, 原始文件字节)。原始字节用于计算 manifest_digest
    (kick 要求 manifest.yaml 文件本身的 sha256,不能重序列化——重排会导致
    字节不同)。
    """
    with tarfile.open(package, "r:gz") as tf:
        for m in tf.getmembers():
            if m.name.endswith("manifest.yaml") and m.isfile():
                raw = tf.extractfile(m)
                if raw is None:
                    continue
                content = raw.read()
                return yaml.safe_load(content.decode("utf-8")), content
    raise SystemExit(f"package {package} 内无 manifest.yaml")


def validate_manifest(manifest: dict, schema_path: Path):
    schema = json.loads(schema_path.read_text(encoding="utf-8"))
    Draft202012Validator(schema).validate(manifest)


def minio_upload(package: Path, mc_image: str, target: str):
    """上传到 MinIO。target 形如 hermes/hermes-packages/xxx.tar.gz。"""
    container = "hermes-runtime-minio-1"
    # 用 docker exec 复用容器内 mc 别名(hermes),避免服务器上装 mc。
    tmp = f"/tmp/{package.name}"
    r = subprocess.run(
        ["docker", "cp", str(package), f"{container}:{tmp}"],
        capture_output=True, text=True)
    if r.returncode != 0:
        raise SystemExit(f"docker cp 失败: {r.stderr.strip()}")
    r = subprocess.run(
        ["docker", "exec", container, "mc", "cp", tmp, target],
        capture_output=True, text=True)
    if r.returncode != 0:
        raise SystemExit(f"mc cp 失败: {r.stderr.strip() or r.stdout.strip()}")
    print(f"uploaded: {target}")


def build_kick(*, package: Path, manifest: dict, manifest_bytes: bytes, variant: str,
               project: str, version: str, commit: str, pipeline_iid: int,
               pipeline_global_id: int, minio_public_base: str, bucket: str) -> dict:
    url = f"{minio_public_base.rstrip('/')}/{bucket}/{package.name}"
    return {
        "variant": variant,
        "package_file": package.name,
        "url": url,
        "sha256": _sha256_file(package),
        "size": package.stat().st_size,
        "manifest_digest": hashlib.sha256(manifest_bytes).hexdigest(),
        "version": version,
        "project": project,
        "commit": commit,
        "pipeline_id": pipeline_iid,
        "pipeline_global_id": pipeline_global_id,
        "requirements": manifest.get("requirements", {}),
        "failure_signatures": manifest.get("test", {}).get("failure_signatures", []),
    }


class UploadKickError(Exception):
    """upload_kick 业务错误(HTTP 服务映射为 4xx/5xx)。"""


def kick(payload: dict, trigger_url: str, token: str) -> str:
    import urllib.error
    import urllib.request

    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        trigger_url, data=data,
        headers={"Content-Type": "application/json", "X-Gitlab-Token": token})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return f"{resp.status} {resp.read().decode('utf-8', 'replace')}"
    except urllib.error.HTTPError as e:
        raise UploadKickError(
            f"kick: HTTP {e.code}: {e.read().decode('utf-8', 'replace')[:300]}")
    except urllib.error.URLError as e:
        raise UploadKickError(f"kick: 连接失败: {e.reason}")


class UploadKickConfig:
    """upload_kick 的输入参数(CLI 与 HTTP 服务共用)。"""

    def __init__(self, *, package: Path, variant: str, project: Optional[str] = None,
                 version: Optional[str] = None, commit: str = "",
                 pipeline_iid: int = 0, pipeline_global_id: int = 0,
                 minio_bucket: str = "hermes-packages",
                 minio_public_base: str = "",
                 schema: Optional[Path] = None,
                 trigger_url: str = "",
                 token_env: str = "TRIGGER_WEBHOOK_SECRET"):
        self.package = package
        self.variant = variant
        self.project = project
        self.version = version
        self.commit = commit
        self.pipeline_iid = pipeline_iid
        self.pipeline_global_id = pipeline_global_id
        self.minio_bucket = minio_bucket
        self.minio_public_base = minio_public_base or os.environ.get(
            "MINIO_PUBLIC_ENDPOINT", "http://10.88.118.251:9000")
        self.schema = schema or (REPO_ROOT / "contracts/manifest.schema.json")
        self.trigger_url = trigger_url or os.environ.get(
            "TRIGGER_URL", "http://127.0.0.1:18090/kick")
        self.token_env = token_env


def do_upload_kick(cfg: UploadKickConfig) -> str:
    """执行完整流程:校验 manifest → 上传 MinIO → kick。返回 kick 响应文本。

    CLI 与 HTTP 服务共用;业务错误抛 UploadKickError。
    """
    if not cfg.package.exists():
        raise UploadKickError(f"package 不存在: {cfg.package}")

    manifest, manifest_bytes = extract_manifest(cfg.package)
    validate_manifest(manifest, cfg.schema)
    print("manifest OK (schema 校验通过)")

    project = cfg.project or manifest.get("artifact", {}).get("project", "")
    if not project:
        raise UploadKickError("缺 project(传 --project 或 manifest.artifact.project)")
    version = cfg.version or manifest.get("artifact", {}).get("version", "0.0.1")
    if not cfg.commit:
        commit = manifest.get("artifact", {}).get("commit", "")
        if not commit:
            raise UploadKickError("缺 commit(传 --commit 或 manifest.artifact.commit)")
    else:
        commit = cfg.commit
    if cfg.pipeline_iid <= 0 or cfg.pipeline_global_id <= 0:
        raise UploadKickError("缺 pipeline_iid / pipeline_global_id(均为正整数)")

    target = f"hermes/{cfg.minio_bucket}/{cfg.package.name}"
    minio_upload(cfg.package, "minio/mc", target)

    payload = build_kick(
        package=cfg.package, manifest=manifest, manifest_bytes=manifest_bytes,
        variant=cfg.variant, project=project, version=version, commit=commit,
        pipeline_iid=cfg.pipeline_iid, pipeline_global_id=cfg.pipeline_global_id,
        minio_public_base=cfg.minio_public_base, bucket=cfg.minio_bucket)

    token = os.environ.get(cfg.token_env, "")
    if not token:
        raise UploadKickError(f"缺少环境变量 {cfg.token_env}(Trigger 共享密钥)")
    return kick(payload, cfg.trigger_url, token)


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", required=True, type=Path,
                        help="本地 tar.gz 测试包路径")
    parser.add_argument("--variant", required=True)
    parser.add_argument("--project", default=None, help="缺省取 manifest.artifact.project")
    parser.add_argument("--version", default=None, help="缺省取 manifest.artifact.version(无则 0.0.1)")
    parser.add_argument("--commit", default="", help="short sha;缺省取 manifest.artifact.commit")
    parser.add_argument("--pipeline-iid", required=True, type=int)
    parser.add_argument("--pipeline-global-id", required=True, type=int)
    parser.add_argument("--minio-bucket", default="hermes-packages")
    parser.add_argument("--minio-public-base",
                        default=os.environ.get("MINIO_PUBLIC_ENDPOINT",
                                               "http://10.88.118.251:9000"))
    parser.add_argument("--schema", type=Path,
                        default=REPO_ROOT / "contracts/manifest.schema.json")
    parser.add_argument("--trigger-url",
                        default=os.environ.get("TRIGGER_URL", "http://127.0.0.1:18090/kick"))
    parser.add_argument("--token-env", default="TRIGGER_WEBHOOK_SECRET",
                        help="承载 Trigger 共享密钥的环境变量名")
    args = parser.parse_args(argv)

    cfg = UploadKickConfig(
        package=args.package, variant=args.variant, project=args.project,
        version=args.version, commit=args.commit,
        pipeline_iid=args.pipeline_iid, pipeline_global_id=args.pipeline_global_id,
        minio_bucket=args.minio_bucket, minio_public_base=args.minio_public_base,
        schema=args.schema, trigger_url=args.trigger_url, token_env=args.token_env)
    try:
        print("kick:", do_upload_kick(cfg))
    except UploadKickError as e:
        raise SystemExit(str(e))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
