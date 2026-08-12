#!/usr/bin/env python3
"""gen_bundle.py — 聚合 8 个变体 meta 为 bundle-g{sha}-p{global_id}.json(CLAUDE.md §6.3)。

规则:
  - variants.yaml 中声明的每个变体都必须有 meta,缺任何一个拒绝发 bundle
    (挡住被 interruptible 打断的残缺构建);
  - 所有 meta 的 project/commit/pipeline_id/pipeline_global_id/version 必须一致;
  - 输出前用 contracts/bundle.schema.json 校验。
Trigger 服务只认 bundle。
"""
import argparse
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

import yaml

try:
    from jsonschema import Draft202012Validator
except ImportError:  # pragma: no cover
    sys.exit("gen_bundle.py requires jsonschema: pip install jsonschema")

PACKAGE_FIELDS = ("variant", "package_file", "url", "sha256", "size", "manifest_digest")
SHARED_FIELDS = (
    "project", "commit", "pipeline_id", "pipeline_global_id", "version",
)

# 规则引擎版本(目标设计基线 v1.0 原则 2):bundle 未显式声明时 Trigger 按
# 此缺省路由 rules 实现;引入新规则版本时由发版流程显式提升。
DEFAULT_RULE_VERSION = "verdict-rules-v1"


def _normalize_created_at(value):
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise SystemExit(f"invalid created_at: {value!r}") from exc
    if parsed.tzinfo is None:
        raise SystemExit("created_at must include a timezone")
    return parsed.astimezone(timezone.utc).isoformat(
        timespec="milliseconds"
    ).replace("+00:00", "Z")


def gen_bundle(*, meta_dir, variants_file, schema_file, outdir, created_at) -> Path:
    meta_dir = Path(meta_dir)
    variants_data = yaml.safe_load(Path(variants_file).read_text(encoding="utf-8"))
    variants = sorted(variants_data["variants"])

    missing = [v for v in variants if not (meta_dir / f"{v}.json").exists()]
    if missing:
        raise SystemExit(
            "refusing to publish bundle, missing meta for: " + ", ".join(missing)
        )

    metas = {
        v: json.loads((meta_dir / f"{v}.json").read_text(encoding="utf-8"))
        for v in variants
    }
    for key in SHARED_FIELDS:
        values = {m[key] for m in metas.values()}
        if len(values) != 1:
            raise SystemExit(f"inconsistent {key!r} across metas: {sorted(values)}")

    shared = {key: metas[variants[0]][key] for key in SHARED_FIELDS}

    def package_entry(v):
        entry = {k: metas[v][k] for k in PACKAGE_FIELDS}
        # 2026-08-12 解耦:业务仓库 variants.yaml 是唯一权威。每个 package
        # 携带调度约束(requirements)与失败签名(failure_signatures),
        # Runtime 据此做设备匹配与证据提取,不再维护自己的变体配置。
        decl = variants_data["variants"][v]
        entry["requirements"] = decl.get("requirements") or {}
        entry["failure_signatures"] = _merge_signatures(variants_data, decl)
        return entry

    bundle = {
        "bundle_version": 1,
        **shared,
        "rule_version": DEFAULT_RULE_VERSION,
        "created_at": _normalize_created_at(created_at),
        "packages": [package_entry(v) for v in variants],
    }

    with Path(schema_file).open(encoding="utf-8") as f:
        validator = Draft202012Validator(json.load(f))
    errors = list(validator.iter_errors(bundle))
    if errors:
        for e in errors:
            print(f"bundle invalid: {e.message}", file=sys.stderr)
        raise SystemExit(2)

    outdir = Path(outdir)
    outdir.mkdir(parents=True, exist_ok=True)
    out = outdir / (
        f"bundle-g{shared['commit']}-p{shared['pipeline_global_id']}.json"
    )
    out.write_text(json.dumps(bundle, indent=2, ensure_ascii=False), encoding="utf-8")
    return out


def _merge_signatures(variants_data, decl):
    """合并 defaults.signatures_common_* 与变体自身 signatures(变体覆盖)。

    与 Runtime SignaturesForVariant 的合并语义一致:先公共后变体,
    同 id 变体覆盖公共。返回 [{id,where,pattern,classify}] 列表。
    """
    os_ = (decl.get("requirements") or {}).get("os", "android")
    common_key = (
        "signatures_common_linux" if os_ == "linux" else "signatures_common_android"
    )
    merged = {}
    order = []
    for d in (variants_data.get("defaults") or {}).get(common_key) or []:
        if d["id"] not in merged:
            order.append(d["id"])
        merged[d["id"]] = {
            "id": d["id"],
            "where": d.get("where", "logs"),
            "pattern": d["pattern"],
            "classify": d["classify"],
        }
    for d in decl.get("signatures") or []:
        if d["id"] not in merged:
            order.append(d["id"])
        merged[d["id"]] = {
            "id": d["id"],
            "where": d.get("where", "logs"),
            "pattern": d["pattern"],
            "classify": d["classify"],
        }
    return [merged[k] for k in order]


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--meta-dir", required=True, type=Path, help="dist/meta")
    parser.add_argument("--variants-file", required=True, type=Path)
    parser.add_argument("--schema", required=True, type=Path,
                        help="contracts/bundle.schema.json")
    parser.add_argument("--outdir", required=True, type=Path)
    parser.add_argument(
        "--created-at", required=True,
        help="stable RFC3339 timestamp; CI uses CI_COMMIT_TIMESTAMP",
    )
    args = parser.parse_args(argv)

    out = gen_bundle(
        meta_dir=args.meta_dir, variants_file=args.variants_file,
        schema_file=args.schema, outdir=args.outdir,
        created_at=args.created_at,
    )
    print(out)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
