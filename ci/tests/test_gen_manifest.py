"""gen_manifest.py:解包 → 注入 manifest.yaml + files.sha256 → 校验 → 重打包重命名。"""
import tarfile

import pytest
import yaml

import gen_manifest
from ci_helpers import (
    FAKE_FILES,
    MANIFEST_SCHEMA,
    VARIANTS_FILE,
    sha256_bytes,
)

VARIANT = "aarch64_Android_SNPE_2.21"


def inject(fake_package, tmp_path, topdir=None):
    pkg = fake_package(topdir=topdir)
    outdir = tmp_path / "out"
    outdir.mkdir(exist_ok=True)
    info = gen_manifest.inject_manifest(
        package=pkg,
        variant=VARIANT,
        variants_file=VARIANTS_FILE,
        schema_file=MANIFEST_SCHEMA,
        project="algo-super-sdk",
        commit="deadbee1",
        pipeline_iid=42,
        build_type="Release",
        package_name="algo-super-sdk",
        outdir=outdir,
    )
    return info, outdir


def read_manifest(tar_path):
    with tarfile.open(tar_path, "r:gz") as tar:
        names = tar.getnames()
        manifest = yaml.safe_load(tar.extractfile("manifest.yaml").read())
        checksums = tar.extractfile("files.sha256").read().decode()
    return names, manifest, checksums


def test_output_filename_encodes_uniqueness(fake_package, tmp_path):
    info, outdir = inject(fake_package, tmp_path)
    expected = f"algo-super-sdk-{VARIANT}-gdeadbee1-p42.tar.gz"
    assert info["package_file"] == expected
    assert (outdir / expected).exists()


def test_manifest_injected_and_schema_valid(fake_package, tmp_path):
    import json

    from jsonschema import Draft202012Validator

    info, outdir = inject(fake_package, tmp_path)
    names, manifest, checksums = read_manifest(outdir / info["package_file"])
    assert "manifest.yaml" in names
    assert "files.sha256" in names
    with MANIFEST_SCHEMA.open(encoding="utf-8") as f:
        Draft202012Validator(json.load(f)).validate(manifest)
    assert manifest["artifact"] == {
        "project": "algo-super-sdk",
        "commit": "deadbee1",
        "pipeline_id": 42,
        "platform": VARIANT,
        "build_type": "Release",
    }


def test_deploy_files_cover_package_with_correct_sha_and_mode(fake_package, tmp_path):
    info, outdir = inject(fake_package, tmp_path)
    _, manifest, checksums = read_manifest(outdir / info["package_file"])
    entries = {e["src"]: e for e in manifest["deploy"]["files"]}
    # 契约文件本身不部署到设备
    assert set(entries) == set(FAKE_FILES)
    for rel, (data, mode) in FAKE_FILES.items():
        assert entries[rel]["sha256"] == sha256_bytes(data)
        assert entries[rel]["mode"] == ("0755" if mode == 0o755 else "0644")
        assert entries[rel]["dst"] == rel
        assert f"{sha256_bytes(data)}  {rel}" in checksums


def test_topdir_layout_keeps_src_strips_dst(fake_package, tmp_path):
    info, outdir = inject(fake_package, tmp_path, topdir="algo-super-sdk-1.2.3")
    _, manifest, _ = read_manifest(outdir / info["package_file"])
    entries = {e["src"]: e for e in manifest["deploy"]["files"]}
    assert set(entries) == {f"algo-super-sdk-1.2.3/{rel}" for rel in FAKE_FILES}
    for rel in FAKE_FILES:
        assert entries[f"algo-super-sdk-1.2.3/{rel}"]["dst"] == rel


def test_info_digests_match_package(fake_package, tmp_path):
    import hashlib

    info, outdir = inject(fake_package, tmp_path)
    out = outdir / info["package_file"]
    assert info["package_sha256"] == hashlib.sha256(out.read_bytes()).hexdigest()
    assert info["size"] == out.stat().st_size
    with tarfile.open(out, "r:gz") as tar:
        manifest_bytes = tar.extractfile("manifest.yaml").read()
    assert info["manifest_digest"] == hashlib.sha256(manifest_bytes).hexdigest()


def test_unknown_variant_fails(fake_package, tmp_path):
    pkg = fake_package()
    with pytest.raises(SystemExit):
        gen_manifest.inject_manifest(
            package=pkg, variant="aarch64_Android_NOPE_9.9",
            variants_file=VARIANTS_FILE, schema_file=MANIFEST_SCHEMA,
            project="p", commit="deadbee1", pipeline_iid=1,
            build_type="Release", package_name="p", outdir=tmp_path,
        )


def test_invalid_rendered_manifest_fails_pipeline(fake_package, tmp_path):
    """variants.yaml 配置渲染出不合法 Manifest 时必须 fail,不得静默打包。"""
    broken = yaml.safe_load(VARIANTS_FILE.read_text(encoding="utf-8"))
    broken["variants"][VARIANT]["requirements"]["os"] = "windows"
    broken_file = tmp_path / "broken-variants.yaml"
    broken_file.write_text(yaml.safe_dump(broken), encoding="utf-8")
    pkg = fake_package()
    with pytest.raises(SystemExit):
        gen_manifest.inject_manifest(
            package=pkg, variant=VARIANT, variants_file=broken_file,
            schema_file=MANIFEST_SCHEMA, project="p", commit="deadbee1",
            pipeline_iid=1, build_type="Release", package_name="p", outdir=tmp_path,
        )


def test_main_cli_writes_info_file(fake_package, tmp_path):
    pkg = fake_package()
    outdir = tmp_path / "out"
    outdir.mkdir()
    info_out = tmp_path / "manifest-info.json"
    rc = gen_manifest.main([
        "--package", str(pkg),
        "--variant", VARIANT,
        "--variants-file", str(VARIANTS_FILE),
        "--schema", str(MANIFEST_SCHEMA),
        "--project", "algo-super-sdk",
        "--commit", "deadbee1",
        "--pipeline-iid", "42",
        "--build-type", "Release",
        "--package-name", "algo-super-sdk",
        "--outdir", str(outdir),
        "--info-out", str(info_out),
    ])
    assert rc == 0
    assert info_out.exists()
