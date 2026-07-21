import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
GITIGNORE = ROOT / ".gitignore"


class SecretExclusionContracts(unittest.TestCase):
    def test_real_deployment_state_is_ignored(self):
        gitignore = GITIGNORE.read_text(encoding="utf-8")

        self.assertIn("/deploy/.env", gitignore)
        self.assertIn("/deploy/images.lock.env", gitignore)

        for path in ("deploy/.env", "deploy/images.lock.env"):
            with self.subTest(path=path):
                returncode = subprocess.run(
                    ["git", "check-ignore", "--no-index", "-q", path],
                    cwd=ROOT,
                    check=False,
                ).returncode
                self.assertEqual(0, returncode)

        for path in ("deploy/.env.example", "deploy/images.lock.env.example"):
            with self.subTest(path=path):
                returncode = subprocess.run(
                    ["git", "check-ignore", "--no-index", "-q", path],
                    cwd=ROOT,
                    check=False,
                ).returncode
                self.assertNotEqual(0, returncode)


if __name__ == "__main__":
    unittest.main()
