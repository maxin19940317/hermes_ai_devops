import re
import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
GITIGNORE = ROOT / ".gitignore"
DOCKERFILE = ROOT / "runtime" / "Dockerfile"
DOCKERIGNORE = ROOT / ".dockerignore"
COMPOSE = ROOT / "deploy" / "docker-compose.yml"
ENV_EXAMPLE = ROOT / "deploy" / ".env.example"
INIT_DB = ROOT / "deploy" / "postgres" / "init" / "10-runtime-db.sh"
LOCK_IMAGES = ROOT / "deploy" / "scripts" / "lock-images.sh"
VALIDATE_ENV = ROOT / "deploy" / "scripts" / "validate-env.sh"
VERIFY_PIPELINE = ROOT / "deploy" / "scripts" / "verify-pipeline.sh"


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


class ComposeContracts(unittest.TestCase):
    def test_required_compose_files_exist(self):
        for path in (COMPOSE, ENV_EXAMPLE, INIT_DB, LOCK_IMAGES, VALIDATE_ENV):
            self.assertTrue(path.is_file(), path)

    def test_compose_isolated_services_and_ports(self):
        text = COMPOSE.read_text(encoding="utf-8")
        for service in ("postgres:", "temporal:", "temporal-ui:", "trigger:", "worker:"):
            self.assertIn(service, text)
        self.assertIn(
            '${TRIGGER_BIND_IP:-0.0.0.0}:${TRIGGER_HOST_PORT:-18090}:8090',
            text,
        )
        self.assertIn(
            '127.0.0.1:${WORKER_CALLBACKS_HOST_PORT:-18091}:8091',
            text,
        )
        self.assertIn(
            '127.0.0.1:${TEMPORAL_UI_HOST_PORT:-18080}:8080',
            text,
        )
        self.assertIn("TEMPORAL_ADDRESS: temporal:7233", text)
        self.assertIn("TEMPORAL_TASK_QUEUE: device-test", text)
        self.assertNotIn("network_mode: host", text)
        self.assertNotIn("container_name:", text)

    def test_compose_requires_locked_third_party_images(self):
        text = COMPOSE.read_text(encoding="utf-8")
        for variable in (
            "POSTGRES_IMAGE",
            "TEMPORAL_IMAGE",
            "TEMPORAL_UI_IMAGE",
            "GO_IMAGE",
            "RUNTIME_BASE_IMAGE",
        ):
            self.assertIn(variable, text)
        self.assertNotRegex(text, re.compile(r"image:\s+[^$\n]*:latest(?:\s|$)"))

    def test_example_contains_no_real_secret(self):
        text = ENV_EXAMPLE.read_text(encoding="utf-8")
        for key in (
            "POSTGRES_ADMIN_PASSWORD",
            "RUNTIME_DB_PASSWORD",
            "GITLAB_TOKEN",
            "TRIGGER_WEBHOOK_SECRET",
        ):
            self.assertRegex(text, rf"(?m)^{key}=\s*$")
        self.assertNotIn("PRIVATE-TOKEN:", text)


class PipelineVerificationContracts(unittest.TestCase):
    def test_verifier_checks_health_registry_database_temporal_and_dedup(self):
        text = VERIFY_PIPELINE.read_text(encoding="utf-8")
        for marker in (
            "/healthz",
            "/api/v4/projects/$project_id/pipelines/$pipeline_id",
            "/webhooks/gitlab",
            "SELECT count(*) FROM artifacts",
            "workflow describe",
            "taskqueue describe",
            ".started == false",
        ):
            self.assertIn(marker, text)


if __name__ == "__main__":
    unittest.main()
