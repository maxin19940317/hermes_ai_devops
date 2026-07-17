#!/usr/bin/env python3
"""gen_manifest.py — 打包后注入契约文件并重命名(CLAUDE.md §6.2 / §6.1)。

流程:解包 release_pack.sh 产出的 tar.gz → 扫描包内文件(sha256/mode)
    → 按 ci/variants.yaml 渲染 manifest.yaml + files.sha256 → 用
    contracts/manifest.schema.json 校验(不合法 fail pipeline)
    → 重打包为唯一文件名: {package_name}-{variant}-g{commit}-p{iid}.tar.gz

在 GitLab Runner 上运行,依赖: python3 >= 3.9, pyyaml, jsonschema。
"""
import argparse
import hashlib
import io
import json
import sys
import tarfile
from pathlib import Path

import yaml

try:
    from jsonschema import Draft202012Validator
except ImportError:  # pragma: no cover
    sys.exit("gen_manifest.py requires jsonschema: pip install jsonschema")

# 注入的契约文件,不属于部署内容,重跑时也要跳过
INJECTED_FILES = {"manifest.yaml", "files.sha256"}


def load_variants(path: Path):
    """加载 variants.yaml,返回 (defaults, variants)。"""
    data = yaml.safe_load(Path(path).read_text(encoding="utf-8"))
    return data["defaults"], data["variants"]


def render_manifest(*, variant, vcfg, defaults, file_entries,
                    project, commit, pipeline_iid, build_type):
    """按变体配置渲染 Manifest 字典(未校验)。"""
    requirements = vcfg["requirements"]
    target_os = requirements.get("os", "")
    workdir_key = "workdir_android" if target_os == "android" else "workdir_linux"
    default_test = defaults["test"]
    variant_test = vcfg.get("test", {})
    signatures = (
        list(defaults.get(f"signatures_common_{target_os}", []))
        + list(vcfg.get("signatures", []))
    )
    deploy = {
        "workdir": defaults["deploy"][workdir_key],
        "files": file_entries,
    }
    if vcfg.get("env"):
        deploy["env"] = vcfg["env"]
    return {
        "manifest_version": 1,
        "artifact": {
            "project": project,
            "commit": commit,
            "pipeline_id": pipeline_iid,
            "platform": variant,
            "build_type": build_type,
        },
        "requirements": requirements,
        "deploy": deploy,
        "test": {
            "entry": variant_test.get("entry", default_test["entry"]),
            "args": variant_test.get("args", []),
            "timeout_sec": variant_test.get("timeout_sec", default_test["timeout_sec"]),
            "success": variant_test.get("success", default_test["success"]),
            "failure_signatures": signatures,
        },
        "collect": vcfg.get("collect", defaults["collect"]),
        "cleanup": defaults["cleanup"],
    }


def _scan_package(tar: tarfile.TarFile):
    """扫描包内常规文件,返回 (members, file_entries)。

    单一顶层目录布局时:src 保留实际路径,dst 剥掉顶层目录(部署到 workdir 根)。
    """
    members = [
        m for m in tar.getmembers()
        if m.isfile() and m.name not in INJECTED_FILES
    ]
    if not members:
        raise SystemExit("package contains no regular files")
    tops = {m.name.split("/", 1)[0] for m in members}
    has_topdir = len(tops) == 1 and all("/" in m.name for m in members)
    topdir = tops.pop() if has_topdir else None

    entries = []
    for m in sorted(members, key=lambda m: m.name):
        digest = hashlib.sha256(tar.extractfile(m).read()).hexdigest()
        dst = m.name[len(topdir) + 1:] if topdir else m.name
        entries.append({
            "src": m.name,
            "dst": dst,
            "mode": "0755" if m.mode & 0o100 else "0644",
            "sha256": digest,
        })
    return members, entries


def _validate_or_die(manifest: dict, schema_file: Path):
    with Path(schema_file).open(encoding="utf-8") as f:
        validator = Draft202012Validator(json.load(f))
    errors = sorted(validator.iter_errors(manifest), key=lambda e: list(e.absolute_path))
    if errors:
        for e in errors:
            loc = "/".join(str(p) for p in e.absolute_path) or "<root>"
            print(f"manifest invalid at {loc}: {e.message}", file=sys.stderr)
        raise SystemExit(2)


def inject_manifest(*, package, variant, variants_file, schema_file,
                    project, commit, pipeline_iid, build_type,
                    package_name, outdir):
    """执行注入与重打包,返回 info 字典供 write_meta.py 消费。"""
    defaults, variants = load_variants(variants_file)
    if variant not in variants:
        raise SystemExit(f"unknown variant {variant!r}, not in {variants_file}")

    outdir = Path(outdir)
    outdir.mkdir(parents=True, exist_ok=True)
    out_name = f"{package_name}-{variant}-g{commit}-p{pipeline_iid}.tar.gz"
    out_path = outdir / out_name

    with tarfile.open(package, "r:gz") as tar:
        members, file_entries = _scan_package(tar)
        manifest = render_manifest(
            variant=variant, vcfg=variants[variant], defaults=defaults,
            file_entries=file_entries, project=project, commit=commit,
            pipeline_iid=pipeline_iid, build_type=build_type,
        )
        _validate_or_die(manifest, schema_file)

        manifest_bytes = yaml.safe_dump(
            manifest, sort_keys=False, allow_unicode=True
        ).encode("utf-8")
        checksum_bytes = "".join(
            f"{e['sha256']}  {e['src']}\n" for e in file_entries
        ).encode("utf-8")

        with tarfile.open(out_path, "w:gz") as out_tar:
            for m in members:
                out_tar.addfile(m, tar.extractfile(m))
            for name, data in (("manifest.yaml", manifest_bytes),
                               ("files.sha256", checksum_bytes)):
                info = tarfile.TarInfo(name)
                info.size = len(data)
                info.mode = 0o644
                out_tar.addfile(info, io.BytesIO(data))

    return {
        "package_file": out_name,
        "package_path": str(out_path),
        "package_sha256": hashlib.sha256(out_path.read_bytes()).hexdigest(),
        "size": out_path.stat().st_size,
        "manifest_digest": hashlib.sha256(manifest_bytes).hexdigest(),
    }


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", required=True, type=Path)
    parser.add_argument("--variant", required=True)
    parser.add_argument("--variants-file", required=True, type=Path)
    parser.add_argument("--schema", required=True, type=Path)
    parser.add_argument("--project", required=True)
    parser.add_argument("--commit", required=True, help="CI_COMMIT_SHORT_SHA")
    parser.add_argument("--pipeline-iid", required=True, type=int, help="CI_PIPELINE_IID")
    parser.add_argument("--build-type", default="Release", choices=["Release", "Debug"])
    parser.add_argument("--package-name", required=True, help="RELEASE_PACKAGE_NAME")
    parser.add_argument("--outdir", required=True, type=Path)
    parser.add_argument("--info-out", type=Path,
                        help="将 info JSON 写入此文件,供 write_meta.py 消费")
    args = parser.parse_args(argv)

    info = inject_manifest(
        package=args.package, variant=args.variant,
        variants_file=args.variants_file, schema_file=args.schema,
        project=args.project, commit=args.commit,
        pipeline_iid=args.pipeline_iid, build_type=args.build_type,
        package_name=args.package_name, outdir=args.outdir,
    )
    if args.info_out:
        args.info_out.write_text(json.dumps(info, indent=2), encoding="utf-8")
    print(info["package_path"])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
