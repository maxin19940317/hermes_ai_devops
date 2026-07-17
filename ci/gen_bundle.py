#!/usr/bin/env python3
"""gen_bundle.py — 聚合 8 个变体 meta 为 bundle-g{sha}.json(CLAUDE.md §6.3)。

规则:
  - variants.yaml 中声明的每个变体都必须有 meta,缺任何一个拒绝发 bundle
    (挡住被 interruptible 打断的残缺构建);
  - 所有 meta 的 project/commit/pipeline_id/version 必须一致;
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
SHARED_FIELDS = ("project", "commit", "pipeline_id", "version")


def gen_bundle(*, meta_dir, variants_file, schema_file, outdir) -> Path:
    meta_dir = Path(meta_dir)
    variants = sorted(yaml.safe_load(Path(variants_file).read_text(encoding="utf-8"))["variants"])

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
    bundle = {
        "bundle_version": 1,
        **shared,
        "created_at": datetime.now(timezone.utc)
        .isoformat(timespec="milliseconds")
        .replace("+00:00", "Z"),
        "packages": [
            {k: metas[v][k] for k in PACKAGE_FIELDS} for v in variants
        ],
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
    out = outdir / f"bundle-g{shared['commit']}.json"
    out.write_text(json.dumps(bundle, indent=2, ensure_ascii=False), encoding="utf-8")
    return out


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--meta-dir", required=True, type=Path, help="dist/meta")
    parser.add_argument("--variants-file", required=True, type=Path)
    parser.add_argument("--schema", required=True, type=Path,
                        help="contracts/bundle.schema.json")
    parser.add_argument("--outdir", required=True, type=Path)
    args = parser.parse_args(argv)

    out = gen_bundle(
        meta_dir=args.meta_dir, variants_file=args.variants_file,
        schema_file=args.schema, outdir=args.outdir,
    )
    print(out)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
