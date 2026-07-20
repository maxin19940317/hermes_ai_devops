"""write_meta.py:由 gen_manifest 的 info + CI 变量生成 dist/meta/{variant}.json。"""
import json

import write_meta

INFO = {
    "package_file": "algo-super-sdk-aarch64_Android_SNPE_2.21-gdeadbee1-p42.tar.gz",
    "package_sha256": "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0",
    "size": 10485760,
    "manifest_digest": "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100",
}


def test_meta_file_content(tmp_path):
    info_file = tmp_path / "manifest-info.json"
    info_file.write_text(json.dumps(INFO), encoding="utf-8")
    outdir = tmp_path / "meta"
    out = write_meta.write_meta(
        info_file=info_file,
        variant="aarch64_Android_SNPE_2.21",
        project="algo-super-sdk",
        version="1.2.3",
        commit="deadbee1",
        pipeline_iid=42,
        pipeline_global_id=42001,
        package_name="algo-super-sdk",
        registry_project_url="https://gitlab.example.com/api/v4/projects/7",
        outdir=outdir,
    )
    assert out == outdir / "aarch64_Android_SNPE_2.21.json"
    meta = json.loads(out.read_text(encoding="utf-8"))
    assert meta["pipeline_id"] == 42
    assert meta["pipeline_global_id"] == 42001
    assert meta == {
        "variant": "aarch64_Android_SNPE_2.21",
        "package_file": INFO["package_file"],
        "url": "https://gitlab.example.com/api/v4/projects/7/packages/generic/"
               "algo-super-sdk/1.2.3/" + INFO["package_file"],
        "sha256": INFO["package_sha256"],
        "size": INFO["size"],
        "manifest_digest": INFO["manifest_digest"],
        "version": "1.2.3",
        "project": "algo-super-sdk",
        "commit": "deadbee1",
        "pipeline_id": 42,
        "pipeline_global_id": 42001,
    }
