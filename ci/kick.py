#!/usr/bin/env python3
"""kick.py — 变体级触发:产物上传 Registry 后直发 Trigger(§6.3)。

读取 write_meta.py 生成的 meta JSON,POST 到 Trigger 的 /kick 端点
(与 GitLab webhook 复用同一共享密钥,走 X-Gitlab-Token 头)。
一个包编好即触发测试,不再等全部 8 个包与整条 pipeline success。

失败即非零退出:让 build job 失败可见、可重试;重复 kick 由 Runtime
按确定性 workflow ID 去重,重试安全。
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

REQUIRED_META_FIELDS = (
    "variant", "package_file", "url", "sha256", "size", "manifest_digest",
    "version", "project", "commit", "pipeline_id", "pipeline_global_id",
)


def kick(*, meta_file, trigger_url, token, timeout=30) -> str:
    payload = Path(meta_file).read_bytes()
    meta = json.loads(payload)
    missing = [k for k in REQUIRED_META_FIELDS if k not in meta]
    if missing:
        raise SystemExit(f"kick: meta 缺少字段 {missing}(write_meta 输出不完整?)")
    req = urllib.request.Request(
        trigger_url,
        data=payload,
        headers={"Content-Type": "application/json", "X-Gitlab-Token": token},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return f"{resp.status} {resp.read().decode('utf-8', 'replace')}"
    except urllib.error.HTTPError as e:
        raise SystemExit(f"kick: HTTP {e.code}: {e.read().decode('utf-8', 'replace')[:200]}")
    except urllib.error.URLError as e:
        raise SystemExit(f"kick: 连接失败: {e.reason}")


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--meta", required=True, type=Path,
                        help="write_meta.py 生成的 dist/meta/{variant}.json")
    parser.add_argument("--trigger-url", required=True,
                        help="Trigger /kick 完整 URL,如 http://host:18090/kick")
    parser.add_argument("--token-env", default="TRIGGER_KICK_TOKEN",
                        help="承载共享密钥的环境变量名(CI/CD 变量下发,缺省 TRIGGER_KICK_TOKEN)")
    args = parser.parse_args(argv)

    token = os.environ.get(args.token_env, "")
    if not token:
        raise SystemExit(f"kick: 缺少环境变量 {args.token_env}(CI/CD 变量未配置)")
    print("kick:", kick(meta_file=args.meta, trigger_url=args.trigger_url, token=token))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
