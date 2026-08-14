#!/usr/bin/env python3
"""hermes-upload-kick — 宿主机 HTTP 服务:Hermes 上传 tar → MinIO → kick。

链路:
    Hermes(容器内,可读 /opt/data/workspace/xxx.tar.gz)
        → curl --data-binary @/opt/data/workspace/xxx.tar.gz http://host:18686/upload-kick
        → 服务把文件流写入 MinIO hermes-packages 桶
        → 解析包内 manifest(requirements/failure_signatures/sha256)
        → POST /kick 起单变体 workflow

不再依赖宿主机直接读 rock.lin(权限 700 的问题不存在:文件经 HTTP 流传输)。

端点:
    POST /upload-kick?variant=<variant>&pipeline_iid=<n>&pipeline_global_id=<n>
         &commit=<sha>&project=<name>&version=<ver>
    body = tar.gz 原始字节
    resp 200 {"ok":true,"reply":"202 {...}"}
    resp 4xx/5xx {"ok":false,"error":"..."}

    GET /healthz

鉴权:HERMES_UPLOAD_KICK_TOKEN 非空时要求 Authorization: Bearer <token>。
"""
import argparse
import hashlib
import io
import json
import os
import sys
import tarfile
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

import yaml

from minio import Minio

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "ci"))

import upload_kick  # noqa: E402

MINIO_ENDPOINT = os.environ.get("MINIO_ENDPOINT", "127.0.0.1:9000")
MINIO_ACCESS_KEY = os.environ.get("MINIO_ROOT_USER", "minioadmin")
MINIO_SECRET_KEY = os.environ.get("MINIO_ROOT_PASSWORD", "")
MINIO_BUCKET = os.environ.get("MINIO_PACKAGE_BUCKET", "hermes-packages")
MINIO_PUBLIC_BASE = os.environ.get("MINIO_PUBLIC_ENDPOINT", "http://10.88.118.251:9000")
TOKEN = os.environ.get("HERMES_UPLOAD_KICK_TOKEN", "")
MAX_BODY = int(os.environ.get("HERMES_UPLOAD_KICK_MAX_BYTES", str(500 << 20)))  # 500MB


def _minio() -> Minio:
    return Minio(MINIO_ENDPOINT, access_key=MINIO_ACCESS_KEY,
                 secret_key=MINIO_SECRET_KEY, secure=False)


def extract_manifest_from_bytes(data: bytes):
    """从 tar.gz 字节流提取 manifest.yaml,返回 (dict, raw_bytes)。"""
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as tf:
        for m in tf.getmembers():
            if m.name.endswith("manifest.yaml") and m.isfile():
                raw = tf.extractfile(m)
                if raw is None:
                    continue
                content = raw.read()
                return yaml.safe_load(content.decode("utf-8")), content
    raise upload_kick.UploadKickError("包内无 manifest.yaml")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f"[http] {self.address_string()} {fmt % args}", flush=True)

    def _auth_ok(self) -> bool:
        if not TOKEN:
            return True
        return self.headers.get("Authorization", "") == f"Bearer {TOKEN}"

    def _json(self, status: int, body: dict):
        data = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        path = urlparse(self.path).path
        if path == "/healthz":
            self._json(200, {"ok": True, "service": "hermes-upload-kick",
                             "minio": MINIO_ENDPOINT, "bucket": MINIO_BUCKET,
                             "auth": "on" if TOKEN else "off"})
            return
        self._json(404, {"ok": False, "error": "not found"})

    def do_POST(self):
        path = urlparse(self.path).path
        if path != "/upload-kick":
            self._json(404, {"ok": False, "error": f"unknown path {path}"})
            return
        if not self._auth_ok():
            self._json(401, {"ok": False, "error": "unauthorized"})
            return

        qs = parse_qs(urlparse(self.path).query)
        variant = (qs.get("variant") or [""])[0]
        try:
            pipeline_iid = int((qs.get("pipeline_iid") or ["0"])[0])
            pipeline_global_id = int((qs.get("pipeline_global_id") or ["0"])[0])
        except ValueError:
            self._json(400, {"ok": False, "error": "pipeline_iid/pipeline_global_id 必须是整数"})
            return
        if not variant or pipeline_iid <= 0 or pipeline_global_id <= 0:
            self._json(400, {"ok": False,
                             "error": "缺 variant / pipeline_iid / pipeline_global_id"})
            return

        # 读文件流
        length = int(self.headers.get("Content-Length", 0))
        if length <= 0:
            self._json(400, {"ok": False, "error": "Content-Length 必须 > 0"})
            return
        if length > MAX_BODY:
            self._json(413, {"ok": False, "error": f"文件过大({length} > {MAX_BODY})"})
            return
        try:
            data = self.rfile.read(length)
        except Exception as e:  # noqa: BLE001
            self._json(400, {"ok": False, "error": f"读取失败: {e}"})
            return

        try:
            manifest, manifest_bytes = extract_manifest_from_bytes(data)
            upload_kick.validate_manifest(
                manifest, upload_kick.REPO_ROOT / "contracts/manifest.schema.json")

            # 对象名:variant + commit 派生(唯一,避免重名覆盖)
            commit = (qs.get("commit") or [""])[0] or manifest.get("artifact", {}).get("commit", "")
            name = f"{variant}-{commit or 'manual'}.tar.gz"
            _minio().put_object(MINIO_BUCKET, name, io.BytesIO(data),
                                length=len(data), content_type="application/gzip")
            url = f"{MINIO_PUBLIC_BASE.rstrip('/')}/{MINIO_BUCKET}/{name}"

            project = ((qs.get("project") or [""])[0]
                       or manifest.get("artifact", {}).get("project", ""))
            version = ((qs.get("version") or [""])[0]
                       or manifest.get("artifact", {}).get("version", "0.0.1"))

            payload = {
                "variant": variant,
                "package_file": name,
                "url": url,
                "sha256": hashlib.sha256(data).hexdigest(),
                "size": len(data),
                "manifest_digest": hashlib.sha256(manifest_bytes).hexdigest(),
                "version": version,
                "project": project,
                "commit": commit,
                "pipeline_id": pipeline_iid,
                "pipeline_global_id": pipeline_global_id,
                "requirements": manifest.get("requirements", {}),
                "failure_signatures": manifest.get("test", {}).get("failure_signatures", []),
            }
            token = os.environ.get("TRIGGER_WEBHOOK_SECRET", "")
            if not token:
                raise upload_kick.UploadKickError("缺少 TRIGGER_WEBHOOK_SECRET(Trigger 共享密钥)")
            reply = upload_kick.kick(
                payload,
                os.environ.get("TRIGGER_URL", "http://127.0.0.1:18090/kick"),
                token)
            self._json(200, {"ok": True, "reply": reply, "url": url})
        except upload_kick.UploadKickError as e:
            self._json(400, {"ok": False, "error": str(e)})
        except Exception as e:  # noqa: BLE001
            traceback.print_exc()
            self._json(500, {"ok": False, "error": f"internal: {e}"})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--addr", default=os.environ.get("HERMES_UPLOAD_KICK_ADDR", "0.0.0.0"))
    parser.add_argument("--port", type=int,
                        default=int(os.environ.get("HERMES_UPLOAD_KICK_PORT", "18686")))
    args = parser.parse_args()

    print(f"hermes-upload-kick listening on {args.addr}:{args.port}", flush=True)
    print(f"  minio={MINIO_ENDPOINT} bucket={MINIO_BUCKET} auth={'on' if TOKEN else 'off'}", flush=True)
    ThreadingHTTPServer((args.addr, args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
