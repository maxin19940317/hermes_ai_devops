#!/usr/bin/env python3
"""write_meta.py — 生成 dist/meta/{variant}.json(CLAUDE.md §6.3)。

输入是 gen_manifest.py 的 --info-out JSON 与 CI 变量;输出的 meta 作为
job artifact 传给 publish:bundle job,由 gen_bundle.py 聚合。
URL 按 Generic Registry 确定性寻址规则拼接:
    {registry_project_url}/packages/generic/{package_name}/{version}/{package_file}
"""
import argparse
import json
import sys
from pathlib import Path


def write_meta(*, info_file, variant, project, version, commit, pipeline_iid,
               pipeline_global_id, package_name, registry_project_url,
               outdir) -> Path:
    info = json.loads(Path(info_file).read_text(encoding="utf-8"))
    url = (f"{registry_project_url.rstrip('/')}/packages/generic/"
           f"{package_name}/{version}/{info['package_file']}")
    meta = {
        "variant": variant,
        "package_file": info["package_file"],
        "url": url,
        "sha256": info["package_sha256"],
        "size": info["size"],
        "manifest_digest": info["manifest_digest"],
        "version": version,
        "project": project,
        "commit": commit,
        "pipeline_id": pipeline_iid,
        "pipeline_global_id": pipeline_global_id,
    }
    outdir = Path(outdir)
    outdir.mkdir(parents=True, exist_ok=True)
    out = outdir / f"{variant}.json"
    out.write_text(json.dumps(meta, indent=2, ensure_ascii=False), encoding="utf-8")
    return out


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--info", required=True, type=Path,
                        help="gen_manifest.py 的 --info-out 文件")
    parser.add_argument("--variant", required=True)
    parser.add_argument("--project", required=True)
    parser.add_argument("--version", required=True, help="X.Y.Z(GitLab 13.8 strict)")
    parser.add_argument("--commit", required=True, help="CI_COMMIT_SHORT_SHA")
    parser.add_argument("--pipeline-iid", required=True, type=int, help="CI_PIPELINE_IID")
    parser.add_argument("--pipeline-global-id", required=True, type=int,
                        help="CI_PIPELINE_ID")
    parser.add_argument("--package-name", required=True, help="RELEASE_PACKAGE_NAME")
    parser.add_argument("--registry-project-url", required=True,
                        help="{CI_API_V4_URL}/projects/{CI_PROJECT_ID}")
    parser.add_argument("--outdir", required=True, type=Path, help="通常为 dist/meta")
    args = parser.parse_args(argv)

    out = write_meta(
        info_file=args.info, variant=args.variant, project=args.project,
        version=args.version, commit=args.commit, pipeline_iid=args.pipeline_iid,
        pipeline_global_id=args.pipeline_global_id, package_name=args.package_name,
        registry_project_url=args.registry_project_url, outdir=args.outdir,
    )
    print(out)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
