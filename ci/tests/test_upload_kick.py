"""upload_kick.py 纯函数测试(不触发真实上传/kick)。"""
import hashlib
import sys
import tarfile
import yaml

sys.path.insert(0, "ci")
import upload_kick  # noqa: E402


def _mk_package(tmp_path, name="demo.tar.gz"):
    """构造含 manifest.yaml 的最小 tar.gz。"""
    manifest = {
        "manifest_version": 1,
        "artifact": {"project": "aios/demo",
                     "commit": "5c885dbb", "pipeline_id": 1,
                     "platform": "aarch64_Linux_smoke", "build_type": "Release"},
        "requirements": {"os": "linux", "abi": "arm64-v8a", "soc": ["QCS6490"]},
        "deploy": {"workdir": "/tmp/demo", "files": [
            {"src": "run.sh", "dst": "run.sh", "mode": "0755",
             "sha256": "0" * 64}]},
        "test": {"entry": "./run.sh", "timeout_sec": 60,
                 "success": {"exit_code": 0},
                 "failure_signatures": [
                     {"id": "native_crash", "where": "stderr",
                      "pattern": "Segmentation fault", "classify": "CODE"}]},
        "collect": ["results/*"],
    }
    pkg = tmp_path / name
    with tarfile.open(pkg, "w:gz") as tar:
        raw = yaml.safe_dump(manifest).encode("utf-8")
        info = tarfile.TarInfo("manifest.yaml")
        info.size = len(raw)
        tar.addfile(info, __import__("io").BytesIO(raw))
        # 载荷文件(打包格式:manifest 在顶层,载荷在同级目录)
        payload = b"#!/bin/sh\necho demo\n"
        pinfo = tarfile.TarInfo("run.sh")
        pinfo.size = len(payload)
        tar.addfile(pinfo, __import__("io").BytesIO(payload))
    return pkg, manifest, raw


def test_extract_manifest(tmp_path):
    pkg, manifest, raw = _mk_package(tmp_path)
    got, got_raw = upload_kick.extract_manifest(pkg)
    assert got["requirements"]["soc"] == ["QCS6490"]
    assert got_raw == raw  # manifest_digest 基于原始字节


def test_build_kick(tmp_path):
    pkg, manifest, raw = _mk_package(tmp_path)
    payload = upload_kick.build_kick(
        package=pkg, manifest=manifest, manifest_bytes=raw,
        variant="aarch64_Linux_smoke", project="aios/demo", version="1.0.2",
        commit="5c885dbb", pipeline_iid=1, pipeline_global_id=2,
        minio_public_base="http://10.88.118.251:9000", bucket="hermes-packages")
    assert payload["url"] == f"http://10.88.118.251:9000/hermes-packages/{pkg.name}"
    assert payload["manifest_digest"] == hashlib.sha256(raw).hexdigest()
    assert payload["sha256"] == upload_kick._sha256_file(pkg)
    assert payload["requirements"]["soc"] == ["QCS6490"]
    assert payload["failure_signatures"][0]["id"] == "native_crash"
    assert payload["size"] == pkg.stat().st_size


def test_validate_manifest_passes(tmp_path, monkeypatch):
    pkg, manifest, raw = _mk_package(tmp_path)
    schema = upload_kick.REPO_ROOT / "contracts/manifest.schema.json"
    # 不报错即通过
    upload_kick.validate_manifest(manifest, schema)


def test_validate_manifest_rejects(tmp_path, monkeypatch):
    pkg, manifest, raw = _mk_package(tmp_path)
    bad = dict(manifest)
    bad["requirements"] = {"os": "linux"}  # 缺 abi
    schema = upload_kick.REPO_ROOT / "contracts/manifest.schema.json"
    import pytest
    with pytest.raises(Exception):
        upload_kick.validate_manifest(bad, schema)
