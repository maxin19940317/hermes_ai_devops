"""kick.py:meta JSON 直发 Trigger /kick(变体级触发,§6.3)。"""
import json

import kick
import pytest

META = {
    "variant": "aarch64_Android_SNPE_2.21",
    "package_file": "pkg.tar.gz",
    "url": "https://gitlab.example/api/v4/projects/651/packages/generic/algo-super-sdk/1.0.2/pkg.tar.gz",
    "sha256": "a" * 64,
    "size": 83188921,
    "manifest_digest": "sha256:deadbeef",
    "version": "1.0.2",
    "project": "aios/algo_super_sdk",
    "commit": "8e981b96",
    "pipeline_id": 48,
    "pipeline_global_id": 712,
}


class FakeResponse:
    def __init__(self, status=202, body=b'{"workflow_id":"wf-1","started":true}'):
        self.status = status
        self._body = body

    def read(self):
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False


def write_meta(tmp_path, meta=None):
    f = tmp_path / "variant.json"
    f.write_text(json.dumps(meta or META), encoding="utf-8")
    return f


def test_kick_posts_meta_with_token(tmp_path, monkeypatch):
    got = {}

    def fake_urlopen(req, timeout):
        got["url"] = req.full_url
        got["token"] = req.headers.get("X-gitlab-token")
        got["body"] = json.loads(req.data)
        got["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setattr(kick.urllib.request, "urlopen", fake_urlopen)
    out = kick.kick(meta_file=write_meta(tmp_path),
                    trigger_url="http://trigger:18090/kick", token="s3cret")
    assert "202" in out
    assert got["url"] == "http://trigger:18090/kick"
    assert got["token"] == "s3cret"
    assert got["body"]["variant"] == META["variant"]
    assert got["body"]["pipeline_global_id"] == 712


def test_kick_missing_meta_field_fails(tmp_path):
    bad = {k: v for k, v in META.items() if k != "manifest_digest"}
    with pytest.raises(SystemExit, match="manifest_digest"):
        kick.kick(meta_file=write_meta(tmp_path, bad),
                  trigger_url="http://t/kick", token="x")


def test_kick_http_error_fails_loud(tmp_path, monkeypatch):
    import urllib.error

    def fake_urlopen(req, timeout):
        raise urllib.error.HTTPError(
            req.full_url, 401, "unauthorized", None,
            type("R", (), {"read": lambda self: b"bad token"})())

    monkeypatch.setattr(kick.urllib.request, "urlopen", fake_urlopen)
    with pytest.raises(SystemExit, match="HTTP 401"):
        kick.kick(meta_file=write_meta(tmp_path),
                  trigger_url="http://t/kick", token="wrong")


def test_main_requires_token(tmp_path, monkeypatch, capsys):
    monkeypatch.delenv("TRIGGER_KICK_TOKEN", raising=False)
    with pytest.raises(SystemExit, match="TRIGGER_KICK_TOKEN"):
        kick.main(["--meta", str(write_meta(tmp_path)),
                   "--trigger-url", "http://t/kick"])
