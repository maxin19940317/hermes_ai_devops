"""ci/ 脚本测试的共享 fixtures:构造假的 release 包(tar.gz)。"""
import io
import sys
import tarfile
from pathlib import Path

import pytest

from ci_helpers import CI_DIR, FAKE_FILES

sys.path.insert(0, str(CI_DIR))


@pytest.fixture
def fake_package(tmp_path):
    """构造 release_pack.sh 风格的 tar.gz。topdir=None 表示文件在包根。"""

    def build(topdir: str | None = None, name: str = "algo-super-sdk-orig.tar.gz") -> Path:
        pkg = tmp_path / name
        with tarfile.open(pkg, "w:gz") as tar:
            for rel, (data, mode) in FAKE_FILES.items():
                arcname = f"{topdir}/{rel}" if topdir else rel
                info = tarfile.TarInfo(arcname)
                info.size = len(data)
                info.mode = mode
                tar.addfile(info, io.BytesIO(data))
        return pkg

    return build
