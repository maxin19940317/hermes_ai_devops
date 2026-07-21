import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
GITIGNORE = ROOT / ".gitignore"
DOCKERFILE = ROOT / "runtime" / "Dockerfile"
DOCKERIGNORE = ROOT / ".dockerignore"


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


class RuntimeImageContracts(unittest.TestCase):
    def test_runtime_image_is_non_root_and_builds_both_commands(self):
        text = DOCKERFILE.read_text(encoding="utf-8")
        instructions = [
            line.strip()
            for line in text.splitlines()
            if line.strip() and not line.lstrip().startswith("#")
        ]

        self.assertEqual("ARG GO_IMAGE=golang:1.26.5-bookworm", instructions[0])
        self.assertEqual(
            "ARG RUNTIME_BASE_IMAGE=alpine:3.22.1", instructions[1]
        )
        build_from = instructions.index("FROM ${GO_IMAGE} AS build")
        runtime_from = instructions.index("FROM ${RUNTIME_BASE_IMAGE}")
        self.assertLess(build_from, runtime_from)
        self.assertIn("WORKDIR /src/runtime", instructions)
        self.assertIn("COPY runtime/go.mod runtime/go.sum ./", instructions)
        self.assertIn("RUN go mod download", instructions)
        self.assertIn("COPY runtime/ ./", instructions)

        build_prefix = (
            'CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w"'
        )
        self.assertIn(
            f"{build_prefix} -o /out/hermes-trigger ./cmd/trigger", text
        )
        self.assertIn(
            f"{build_prefix} -o /out/hermes-worker ./cmd/worker", text
        )

        runtime_instructions = instructions[runtime_from + 1 :]
        self.assertIn(
            "RUN apk add --no-cache ca-certificates wget \\",
            runtime_instructions,
        )
        self.assertIn(
            "COPY --from=build /out/hermes-trigger /app/hermes-trigger",
            runtime_instructions,
        )
        self.assertIn(
            "COPY --from=build /out/hermes-worker /app/hermes-worker",
            runtime_instructions,
        )
        self.assertIn(
            "COPY ci/variants.yaml /etc/hermes/variants.yaml",
            runtime_instructions,
        )
        self.assertEqual(
            [
                "USER hermes",
                "WORKDIR /app",
                'CMD ["/app/hermes-trigger"]',
            ],
            instructions[-3:],
        )

    def test_dockerignore_is_exact_and_excludes_secrets(self):
        lines = DOCKERIGNORE.read_text(encoding="utf-8").splitlines()

        self.assertEqual(
            [
                ".git",
                ".worktrees",
                ".venv",
                "__pycache__",
                ".pytest_cache",
                "agent-runs",
                "deploy/.env",
                "deploy/images.lock.env",
                "docs",
            ],
            lines,
        )


if __name__ == "__main__":
    unittest.main()
