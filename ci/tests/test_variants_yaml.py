"""variants.yaml:全部变体齐全,且每个变体渲染出的 Manifest 都能过 Schema。"""
import json

import yaml
from jsonschema import Draft202012Validator

import gen_manifest
from ci_helpers import MANIFEST_SCHEMA, VARIANTS_FILE

# 12 个构建变体(pipeline 1109 起)。2026-08-06 包名变更:SNPE/TFLite 变体名
# 编码目标 SoC(Android QCM* / Linux QCS*,TFLite Android 为 Qualcomm);
# RKNN 按 SoC 型号拆分(RK3562/3568/3576)维持不变。
EXPECTED_VARIANTS = {
    "aarch64_Android_QCM6125_SNPE_1.68",
    "aarch64_Android_QCM6490_SNPE_2.21",
    "aarch64_Android_RK3562_RKNN_2.3.2",
    "aarch64_Android_RK3568_RKNN_2.3.2",
    "aarch64_Android_RK3576_RKNN_2.3.2",
    "aarch64_Android_Qualcomm_TFLite_2.21.0",
    "aarch64_Linux_QCS6125_SNPE_1.68",
    "aarch64_Linux_QCS6490_SNPE_2.21",
    "aarch64_Linux_RK3562_RKNN_2.3.2",
    "aarch64_Linux_RK3568_RKNN_2.3.2",
    "aarch64_Linux_RK3576_RKNN_2.3.2",
    "aarch64_Linux_TFLite_2.21.0",
}

DUMMY_FILES = [
    {
        "src": "run.sh",
        "dst": "run.sh",
        "mode": "0755",
        "sha256": "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
    }
]


def test_all_variants_present():
    defaults, variants = gen_manifest.load_variants(VARIANTS_FILE)
    assert set(variants) == EXPECTED_VARIANTS


def test_every_variant_renders_schema_valid_manifest():
    with MANIFEST_SCHEMA.open(encoding="utf-8") as f:
        validator = Draft202012Validator(json.load(f))
    defaults, variants = gen_manifest.load_variants(VARIANTS_FILE)
    for variant, vcfg in variants.items():
        manifest = gen_manifest.render_manifest(
            variant=variant,
            vcfg=vcfg,
            defaults=defaults,
            file_entries=DUMMY_FILES,
            project="algo-super-sdk",
            commit="deadbee1",
            pipeline_iid=42,
            build_type="Release",
        )
        errors = list(validator.iter_errors(manifest))
        assert not errors, f"{variant}: {[e.message for e in errors]}"


def test_every_variant_uses_packaged_smoke_runner_without_args():
    defaults, variants = gen_manifest.load_variants(VARIANTS_FILE)
    for variant, vcfg in variants.items():
        manifest = gen_manifest.render_manifest(
            variant=variant, vcfg=vcfg, defaults=defaults, file_entries=DUMMY_FILES,
            project="p", commit="deadbee1", pipeline_iid=1, build_type="Release",
        )
        assert manifest["test"]["entry"] == "./run.sh", variant
        assert manifest["test"]["args"] == [], variant


def test_android_variants_use_packaged_native_libraries():
    _, variants = gen_manifest.load_variants(VARIANTS_FILE)
    for variant, vcfg in variants.items():
        if vcfg["requirements"]["os"] != "android":
            continue
        assert vcfg["env"]["LD_LIBRARY_PATH"] == "{workdir}/lib", variant
        if "SNPE" in variant:
            assert "{workdir}/lib/dsp" in vcfg["env"]["ADSP_LIBRARY_PATH"], variant


def test_android_variants_carry_native_crash_signature():
    defaults, variants = gen_manifest.load_variants(VARIANTS_FILE)
    for variant, vcfg in variants.items():
        manifest = gen_manifest.render_manifest(
            variant=variant, vcfg=vcfg, defaults=defaults, file_entries=DUMMY_FILES,
            project="p", commit="deadbee1", pipeline_iid=1, build_type="Release",
        )
        ids = {s["id"] for s in manifest["test"]["failure_signatures"]}
        if manifest["requirements"]["os"] == "android":
            assert "native_crash" in ids, variant
        if "SNPE" in variant:
            assert "cpu_fallback" in ids, variant
