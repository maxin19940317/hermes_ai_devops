"""gen_bundle.py:聚合 8 个 meta → bundle-g{sha}-p{global_id}.json;缺任何一个不发 bundle。"""
import json

import pytest
import yaml

import gen_bundle
from ci_helpers import BUNDLE_SCHEMA, VARIANTS_FILE


def all_variants():
    data = yaml.safe_load(VARIANTS_FILE.read_text(encoding="utf-8"))
    return sorted(data["variants"])


def make_metas(meta_dir, variants, commit="deadbee1", pipeline_global_id=42001):
    meta_dir.mkdir(parents=True, exist_ok=True)
    for v in variants:
        meta = {
            "variant": v,
            "package_file": f"algo-super-sdk-{v}-g{commit}-p42.tar.gz",
            "url": f"https://gitlab.example.com/api/v4/projects/7/packages/generic/"
                   f"algo-super-sdk/1.2.3/algo-super-sdk-{v}-g{commit}-p42.tar.gz",
            "sha256": "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0",
            "size": 1024,
            "manifest_digest": "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100",
            "version": "1.2.3",
            "project": "algo-super-sdk",
            "commit": commit,
            "pipeline_id": 42,
            "pipeline_global_id": pipeline_global_id,
        }
        (meta_dir / f"{v}.json").write_text(json.dumps(meta), encoding="utf-8")


def test_full_set_produces_schema_valid_bundle(tmp_path):
    meta_dir = tmp_path / "meta"
    make_metas(meta_dir, all_variants())
    out = gen_bundle.gen_bundle(
        meta_dir=meta_dir, variants_file=VARIANTS_FILE,
        schema_file=BUNDLE_SCHEMA, outdir=tmp_path,
        created_at="2026-07-17T08:00:00Z",
    )
    assert out.name == "bundle-gdeadbee1-p42001.json"
    bundle = json.loads(out.read_text(encoding="utf-8"))
    from jsonschema import Draft202012Validator

    with BUNDLE_SCHEMA.open(encoding="utf-8") as f:
        Draft202012Validator(json.load(f)).validate(bundle)
    assert bundle["bundle_version"] == 1
    assert bundle["commit"] == "deadbee1"
    assert bundle["pipeline_id"] == 42
    assert bundle["pipeline_global_id"] == 42001
    assert bundle["version"] == "1.2.3"
    assert bundle["created_at"] == "2026-07-17T08:00:00.000Z"
    assert [p["variant"] for p in bundle["packages"]] == all_variants()
    # 顶层已有 project/commit 等,packages 内不重复携带
    assert "commit" not in bundle["packages"][0]


def test_missing_one_meta_blocks_bundle(tmp_path):
    """被 interruptible 打断的残缺构建不得发 bundle(§6.3)。"""
    meta_dir = tmp_path / "meta"
    variants = all_variants()
    make_metas(meta_dir, variants[:-1])
    with pytest.raises(SystemExit):
        gen_bundle.gen_bundle(
            meta_dir=meta_dir, variants_file=VARIANTS_FILE,
            schema_file=BUNDLE_SCHEMA, outdir=tmp_path,
            created_at="2026-07-17T08:00:00Z",
        )
    assert not list(tmp_path.glob("bundle-*.json"))


def test_inconsistent_commit_blocks_bundle(tmp_path):
    meta_dir = tmp_path / "meta"
    variants = all_variants()
    make_metas(meta_dir, variants)
    # 篡改其中一个 meta 的 commit
    victim = meta_dir / f"{variants[0]}.json"
    meta = json.loads(victim.read_text(encoding="utf-8"))
    meta["commit"] = "0000000"
    victim.write_text(json.dumps(meta), encoding="utf-8")
    with pytest.raises(SystemExit):
        gen_bundle.gen_bundle(
            meta_dir=meta_dir, variants_file=VARIANTS_FILE,
            schema_file=BUNDLE_SCHEMA, outdir=tmp_path,
            created_at="2026-07-17T08:00:00Z",
        )


def test_retry_is_byte_identical(tmp_path):
    meta_dir = tmp_path / "meta"
    make_metas(meta_dir, all_variants(), pipeline_global_id=42001)
    kwargs = dict(
        meta_dir=meta_dir,
        variants_file=VARIANTS_FILE,
        schema_file=BUNDLE_SCHEMA,
        created_at="2026-07-17T08:00:00Z",
    )
    one = gen_bundle.gen_bundle(outdir=tmp_path / "one", **kwargs)
    two = gen_bundle.gen_bundle(outdir=tmp_path / "two", **kwargs)
    assert one.read_bytes() == two.read_bytes()


def test_same_commit_new_pipeline_gets_new_name(tmp_path):
    first_meta = tmp_path / "first-meta"
    second_meta = tmp_path / "second-meta"
    make_metas(first_meta, all_variants(), pipeline_global_id=42001)
    make_metas(second_meta, all_variants(), pipeline_global_id=42002)
    common = dict(
        variants_file=VARIANTS_FILE,
        schema_file=BUNDLE_SCHEMA,
        created_at="2026-07-17T08:00:00Z",
    )
    first = gen_bundle.gen_bundle(
        meta_dir=first_meta, outdir=tmp_path / "one", **common
    )
    second = gen_bundle.gen_bundle(
        meta_dir=second_meta, outdir=tmp_path / "two", **common
    )
    assert first.name == "bundle-gdeadbee1-p42001.json"
    assert second.name == "bundle-gdeadbee1-p42002.json"


@pytest.mark.parametrize(
    ("created_at", "message"),
    [
        ("not-a-timestamp", "invalid created_at"),
        ("2026-07-17T08:00:00", "created_at must include a timezone"),
    ],
)
def test_invalid_created_at_blocks_bundle(tmp_path, created_at, message):
    meta_dir = tmp_path / "meta"
    make_metas(meta_dir, all_variants())
    with pytest.raises(SystemExit, match=message):
        gen_bundle.gen_bundle(
            meta_dir=meta_dir,
            variants_file=VARIANTS_FILE,
            schema_file=BUNDLE_SCHEMA,
            outdir=tmp_path,
            created_at=created_at,
        )
    assert not list(tmp_path.glob("bundle-*.json"))
