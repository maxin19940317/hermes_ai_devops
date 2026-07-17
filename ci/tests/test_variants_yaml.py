"""variants.yaml:8 个变体齐全,且每个变体渲染出的 Manifest 都能过 Schema。"""
import json

import yaml
from jsonschema import Draft202012Validator

import gen_manifest
from ci_helpers import MANIFEST_SCHEMA, VARIANTS_FILE

EXPECTED_VARIANTS = {
    "aarch64_Linux_SNPE_1.68",
    "aarch64_Android_SNPE_1.68",
    "aarch64_Linux_SNPE_2.21",
    "aarch64_Android_SNPE_2.21",
    "aarch64_Linux_RKNN_2.3.2",
    "aarch64_Android_RKNN_2.3.2",
    "aarch64_Linux_TFLite_2.21.0",
    "aarch64_Android_TFLite_2.21.0",
}

DUMMY_FILES = [
    {
        "src": "run.sh",
        "dst": "run.sh",
        "mode": "0755",
        "sha256": "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
    }
]


def test_all_eight_variants_present():
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
