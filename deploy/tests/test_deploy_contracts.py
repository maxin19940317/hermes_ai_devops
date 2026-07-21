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


def dockerfile_instructions(text):
    """Parse a Dockerfile into logical instructions.

    Skips blank lines and full-line comments, joins backslash continuations,
    and collapses internal whitespace to single spaces so assertions match
    instruction semantics instead of physical layout. Raises ValueError on an
    unterminated continuation at end of file.
    """
    instructions = []
    buffer = ""
    for raw in text.splitlines():
        stripped = raw.strip()
        if not buffer and (not stripped or stripped.startswith("#")):
            continue
        continued = stripped.endswith("\\")
        buffer += stripped[:-1] + " " if continued else stripped
        if continued:
            continue
        instructions.append(" ".join(buffer.split()))
        buffer = ""
    if buffer:
        raise ValueError("unterminated continuation at end of Dockerfile")
    return instructions


class DockerfileParserContracts(unittest.TestCase):
    def test_parser_skips_layout_noise_and_joins_continuations(self):
        text = (
            "# leading comment\n"
            "\n"
            "ARG A=1\n"
            "RUN apk add \\\n"
            "  wget  curl\n"
            "# trailing comment\n"
        )
        self.assertEqual(
            ["ARG A=1", "RUN apk add wget curl"],
            dockerfile_instructions(text),
        )

    def test_parser_rejects_unterminated_continuation(self):
        with self.assertRaises(ValueError):
            dockerfile_instructions("RUN echo \\\n")


class RuntimeImageContracts(unittest.TestCase):
    def test_runtime_image_is_non_root_and_builds_both_commands(self):
        instructions = dockerfile_instructions(
            DOCKERFILE.read_text(encoding="utf-8")
        )

        self.assertEqual("ARG GO_IMAGE=golang:1.26.5-bookworm", instructions[0])
        self.assertEqual(
            "ARG RUNTIME_BASE_IMAGE=alpine:3.22.1", instructions[1]
        )
        self.assertEqual(
            ["FROM ${GO_IMAGE} AS build", "FROM ${RUNTIME_BASE_IMAGE}"],
            [i for i in instructions if i.startswith("FROM ")],
        )

        build_stage = instructions[
            instructions.index("FROM ${GO_IMAGE} AS build")
            + 1 : instructions.index("FROM ${RUNTIME_BASE_IMAGE}")
        ]
        runtime_stage = instructions[
            instructions.index("FROM ${RUNTIME_BASE_IMAGE}") + 1 :
        ]

        for expected in (
            "WORKDIR /src/runtime",
            "COPY runtime/go.mod runtime/go.sum ./",
            "RUN go mod download",
            "COPY runtime/ ./",
        ):
            self.assertIn(expected, build_stage)

        build_prefix = (
            "RUN CGO_ENABLED=0 GOOS=linux go build -trimpath "
            '-ldflags="-s -w"'
        )
        build_runs = [i for i in instructions if "go build" in i]
        self.assertEqual(1, len(build_runs), build_runs)
        dual_build = build_runs[0]
        self.assertTrue(dual_build.startswith(build_prefix), dual_build)
        self.assertIn(dual_build, build_stage)
        for binary, cmd in (
            ("hermes-trigger", "./cmd/trigger"),
            ("hermes-worker", "./cmd/worker"),
        ):
            self.assertIn(f"-o /out/{binary} {cmd}", dual_build)

        self.assertIn(
            "RUN apk add --no-cache ca-certificates wget && addgroup -S hermes"
            " && adduser -S -G hermes -h /nonexistent -s /sbin/nologin hermes",
            runtime_stage,
        )
        for expected in (
            "COPY --from=build /out/hermes-trigger /app/hermes-trigger",
            "COPY --from=build /out/hermes-worker /app/hermes-worker",
            "COPY ci/variants.yaml /etc/hermes/variants.yaml",
        ):
            self.assertIn(expected, runtime_stage)
        self.assertNotIn(
            "go build", " ".join(runtime_stage)
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
