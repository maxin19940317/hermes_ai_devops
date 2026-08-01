import re
import subprocess
import unittest
from dataclasses import dataclass
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
WORKFLOW_RUNS_MIGRATION = (
    ROOT / "deploy" / "postgres" / "migrations"
    / "2026-07-30-workflow-runs.sql"
)
DEPLOY_README = ROOT / "deploy" / "README.md"
WORKFLOW_RUNS_SPEC = (
    ROOT / "docs" / "superpowers" / "specs"
    / "2026-07-30-workflow-runs-design.md"
)
NL_DESIGN = (
    ROOT / "docs" / "superpowers" / "specs"
    / "2026-07-28-feishu-cmd-nl-translate-design.md"
)
CURRENT_OPERATIONAL_DOCS = (
    ROOT / "README.md",
    ROOT / "CLAUDE.md",
    ROOT / "runtime" / "README.md",
    ROOT / "agent" / "README.md",
    ROOT / "ci" / "README.md",
    ROOT / "hermes" / "analyze_bridge" / "README.md",
    ROOT / "docs" / "device-test-sequence.md",
    NL_DESIGN,
    DEPLOY_README,
)
LEGACY_RERUN_SYNTAX = re.compile(
    r"\brerun\s+(?:"
    r"<(?:sha(?:8|[0-9]*)?|commit_sha)>\s+"
    r"<(?:pipeline_iid|pipeline_id)>"
    r"|[0-9a-fA-F]{7,40}\s+[1-9][0-9]*"
    r")(?=[\s`)\]}>.,;:!?，。；：！？*_~]|$)"
)
WORKFLOW_RUNS_MIGRATION_PATTERNS = {
    "workflow_runs table": re.compile(
        r"\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"
        r"workflow_runs\s*\(",
        re.IGNORECASE,
    ),
    "project artifact index": re.compile(
        r"\bCREATE\s+UNIQUE\s+INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?"
        r"artifacts_project_key\s+ON\s+artifacts\s*"
        r"\(\s*project\s*,\s*commit_sha\s*,\s*pipeline_id\s*,\s*variant\s*\)",
        re.IGNORECASE,
    ),
    "artifact constraint from index": re.compile(
        r"\bALTER\s+TABLE\s+artifacts\s+ADD\s+CONSTRAINT\s+"
        r"artifacts_project_key\s+UNIQUE\s+USING\s+INDEX\s+"
        r"artifacts_project_key\s*;",
        re.IGNORECASE,
    ),
    "old artifact constraint removal": re.compile(
        r"\bALTER\s+TABLE\s+artifacts\s+DROP\s+CONSTRAINT\s+"
        r"(?:IF\s+EXISTS\s+)?"
        r"artifacts_commit_sha_pipeline_id_variant_key\s*;",
        re.IGNORECASE,
    ),
}


def strip_sql_comments(text):
    out = []
    i = 0
    block_depth = 0
    in_line_comment = False
    while i < len(text):
        pair = text[i:i + 2]
        if in_line_comment:
            if text[i] in "\r\n":
                in_line_comment = False
                out.append(text[i])
            i += 1
            continue
        if block_depth:
            if pair == "/*":
                block_depth += 1
                i += 2
            elif pair == "*/":
                block_depth -= 1
                i += 2
            else:
                if text[i] in "\r\n":
                    out.append(text[i])
                i += 1
            continue
        if pair == "--":
            in_line_comment = True
            i += 2
        elif pair == "/*":
            block_depth = 1
            i += 2
        else:
            out.append(text[i])
            i += 1
    return "".join(out)


CARD_ACTIONS_ROLLOUT_HEADING = "Card-action two-stage rollout"
CARD_ACTIONS_STAGE_1_MARKER = (
    "**Stage 1 — workflow-runs compatibility.**"
)
CARD_ACTIONS_STAGE_2_MARKER = "**Stage 2 — card actions.**"
CARD_ACTIONS_STAGE_2_GATE = (
    "Only after the workflow_runs rollout is stable:"
)
CARD_ACTIONS_STAGE1_REQUIRED_MARKERS = (
    "config.update_multi: true",
    "CardElement.Actions",
    "The workflow never sets Actions",
    "cards still have no buttons",
    "behavior is unchanged",
    "newly sent messages become updateable",
)
CARD_ACTIONS_STAGE2_REQUIRED_MARKERS = (
    "deploy/postgres/migrations/2026-08-01-card-actions.sql",
    "card_action_inbox",
    "card_actions",
    "card_action_messages",
    "card_action_snapshots",
    "audit_log",
    "Deploy the cardaction Activity/listener implementation.",
    "FEISHU_CARD_ACTIONS_ENABLED=true",
    "strict opt-in and defaults to false",
    "verify the app sender is selected",
    "FEISHU_APP_ID",
    "FEISHU_APP_SECRET",
    "FEISHU_RECEIVE_ID",
    "FEISHU_CMD_WHITELIST",
    "Stage 2 must not be mixed into the workflow_runs stop-write window.",
    "artifact unique key",
    "analyze_bridge v2",
    "failures unattributable and impossible to isolate",
    "card.action.trigger",
    "事件与回调 → 回调",
    "long connection",
    "no public callback URL",
    "Runtime handler registration alone does not prove the platform subscription exists",
)
CARD_ACTIONS_STAGE2_ONLY_MARKERS = CARD_ACTIONS_STAGE2_REQUIRED_MARKERS
CARD_ACTIONS_VALID_PREAMBLE_LINES = (
    "The two stages must not be merged.",
)
CARD_ACTIONS_VALID_STAGE1_LINES = (
    "With the first `workflow_runs` production deployment, ship only `config.update_multi: true` plus the `CardElement.Actions`",
    "declaration and its compatibility assertion. The workflow never sets `Actions`, cards",
    "still have no buttons, behavior is unchanged, and newly sent messages become updateable.",
)
CARD_ACTIONS_VALID_STAGE2_LINES = (
    "1. Apply `deploy/postgres/migrations/2026-08-01-card-actions.sql`, which creates",
    "   `card_action_inbox`, `card_actions`, `card_action_messages`,",
    "   `card_action_snapshots`, and `audit_log`.",
    "2. Deploy the cardaction Activity/listener implementation.",
    "3. After every prerequisite below is ready, explicitly set",
    "   `FEISHU_CARD_ACTIONS_ENABLED=true`.",
    "",
    "Stage 2 must not be mixed into the workflow_runs stop-write window. That window already",
    "changes the artifact unique key and cuts over `analyze_bridge` v2; adding five tables",
    "and a new callback path makes failures unattributable and impossible to isolate.",
    "",
    "`FEISHU_CARD_ACTIONS_ENABLED` is a strict opt-in and defaults to `false`. Before enabling",
    "it, verify the app sender is selected and `FEISHU_APP_ID`, `FEISHU_APP_SECRET`, and",
    "`FEISHU_RECEIVE_ID` are configured, and keep `FEISHU_CMD_WHITELIST` nonempty so the",
    "WebSocket listener is running. In Feishu Open Platform, subscribe to",
    "`card.action.trigger` under “事件与回调 → 回调” using the long connection; no public",
    "callback URL is required. Runtime handler registration alone does not prove the platform",
    "subscription exists, so verify the subscription in the platform before enabling Stage 2.",
)


def normalize_whitespace(text):
    return " ".join(text.split())


def normalize_semantic_text(text):
    return normalize_whitespace(text).replace("`", "")


WORKER_CARD_ACTIONS_OPT_IN_LINE = (
    "      FEISHU_CARD_ACTIONS_ENABLED: ${FEISHU_CARD_ACTIONS_ENABLED:-false}"
)


def assert_worker_card_actions_opt_in(text):
    lines = text.splitlines()

    def is_active(line):
        stripped = line.strip()
        return bool(stripped) and not stripped.startswith("#")

    def indentation(line):
        return len(line) - len(line.lstrip(" "))

    worker_indexes = [
        index
        for index, line in enumerate(lines)
        if is_active(line) and line == "  worker:"
    ]
    if len(worker_indexes) != 1:
        raise AssertionError("expected exactly one active worker service")

    worker_start = worker_indexes[0]
    worker_end = len(lines)
    for index in range(worker_start + 1, len(lines)):
        line = lines[index]
        if is_active(line) and indentation(line) <= 2:
            worker_end = index
            break

    environment_indexes = [
        index
        for index in range(worker_start + 1, worker_end)
        if is_active(lines[index]) and lines[index] == "    environment:"
    ]
    if len(environment_indexes) != 1:
        raise AssertionError(
            "expected exactly one active worker environment mapping"
        )

    environment_start = environment_indexes[0]
    environment_end = worker_end
    for index in range(environment_start + 1, worker_end):
        line = lines[index]
        if is_active(line) and indentation(line) <= 4:
            environment_end = index
            break

    key_pattern = re.compile(
        r"^      FEISHU_CARD_ACTIONS_ENABLED:(?P<value>.*)$"
    )
    matches = []
    for line in lines[environment_start + 1:environment_end]:
        if not is_active(line):
            continue
        match = key_pattern.match(line)
        if match:
            matches.append(match.group("value").strip())

    if not matches:
        raise AssertionError(
            "missing active six-space FEISHU_CARD_ACTIONS_ENABLED "
            "worker environment key"
        )
    if len(matches) > 1:
        raise AssertionError(
            "duplicate active FEISHU_CARD_ACTIONS_ENABLED "
            "worker environment keys"
        )
    if matches[0] != "${FEISHU_CARD_ACTIONS_ENABLED:-false}":
        raise AssertionError(
            "FEISHU_CARD_ACTIONS_ENABLED worker environment value must equal "
            "${FEISHU_CARD_ACTIONS_ENABLED:-false}"
        )


def assert_card_actions_default_off_env_assignment(text):
    assignments = []
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if not stripped.startswith("FEISHU_CARD_ACTIONS_ENABLED="):
            continue
        assignments.append(stripped.split("=", 1)[1])

    if not assignments:
        raise AssertionError(
            "missing active FEISHU_CARD_ACTIONS_ENABLED assignment"
        )
    if len(assignments) > 1:
        raise AssertionError(
            "duplicate active FEISHU_CARD_ACTIONS_ENABLED assignments"
        )
    if assignments[0] != "false":
        raise AssertionError(
            "FEISHU_CARD_ACTIONS_ENABLED value must equal false"
        )


def build_valid_card_actions_rollout_doc(
    *,
    preamble_extra_lines=(),
    stage1_extra_lines=(),
    stage2_intro=CARD_ACTIONS_STAGE_2_GATE,
    stage2_extra_lines=(),
    trailing_lines=(),
):
    lines = [
        f"### {CARD_ACTIONS_ROLLOUT_HEADING}",
        "",
        *CARD_ACTIONS_VALID_PREAMBLE_LINES,
        *preamble_extra_lines,
        "",
        f"{CARD_ACTIONS_STAGE_1_MARKER} {CARD_ACTIONS_VALID_STAGE1_LINES[0]}",
        *CARD_ACTIONS_VALID_STAGE1_LINES[1:],
        *stage1_extra_lines,
        "",
        f"{CARD_ACTIONS_STAGE_2_MARKER} {stage2_intro}",
        *CARD_ACTIONS_VALID_STAGE2_LINES,
        *stage2_extra_lines,
    ]
    if trailing_lines:
        lines.extend(("", *trailing_lines))
    return "\n".join(lines) + "\n"


@dataclass(frozen=True)
class SemanticATXHeading:
    level: int
    title: str
    start: int
    end: int


def scan_semantic_atx_headings(text):
    headings = []
    offset = 0
    for raw_line in text.splitlines(keepends=True):
        line = raw_line.rstrip("\r\n")
        indent = 0
        while indent < len(line) and line[indent] == " ":
            indent += 1
        if indent > 3:
            offset += len(raw_line)
            continue

        marker_end = indent
        while marker_end < len(line) and line[marker_end] == "#":
            marker_end += 1
        level = marker_end - indent
        if not 1 <= level <= 6:
            offset += len(raw_line)
            continue
        if marker_end < len(line) and line[marker_end] not in " \t":
            offset += len(raw_line)
            continue

        remainder = line[marker_end:]
        if remainder and remainder[0] in " \t":
            remainder = remainder.lstrip(" \t")
        title = re.sub(r"[ \t]+#+$", "", remainder.rstrip(" \t"))
        headings.append(
            SemanticATXHeading(
                level=level,
                title=title,
                start=offset,
                end=offset + len(line),
            )
        )
        offset += len(raw_line)
    return headings


def parse_card_actions_rollout_sections(text):
    headings = scan_semantic_atx_headings(text)
    candidate_sections = [
        heading
        for heading in headings
        if (
            heading.level == 3
            and CARD_ACTIONS_ROLLOUT_HEADING in heading.title
        )
    ]
    if not candidate_sections:
        raise ValueError(
            f"missing ### {CARD_ACTIONS_ROLLOUT_HEADING} section"
        )
    if len(candidate_sections) > 1:
        raise ValueError(
            f"duplicated ### {CARD_ACTIONS_ROLLOUT_HEADING} section"
        )

    sections = [
        heading
        for heading in candidate_sections
        if heading.title == CARD_ACTIONS_ROLLOUT_HEADING
    ]
    if not sections:
        raise ValueError(
            f"missing ### {CARD_ACTIONS_ROLLOUT_HEADING} section"
        )

    body_start = sections[0].end
    if text.startswith("\r\n", body_start):
        body_start += 2
    elif body_start < len(text) and text[body_start] in "\r\n":
        body_start += 1

    next_heading = next(
        (
            heading
            for heading in headings
            if heading.start >= body_start and heading.level <= 3
        ),
        None,
    )
    body_end = next_heading.start if next_heading else len(text)
    body = text[body_start:body_end]
    if not body.strip():
        raise ValueError(
            f"### {CARD_ACTIONS_ROLLOUT_HEADING} section is empty"
        )

    lines = body.splitlines()

    def find_marker(marker, label):
        matcher = re.compile(
            rf"^{re.escape(marker)}(?P<rest>.*)$"
        )
        hits = []
        for index, line in enumerate(lines):
            match = matcher.match(line.strip())
            if match:
                hits.append((index, match.group("rest").strip()))
        if not hits:
            raise ValueError(f"missing {label}")
        if len(hits) > 1:
            raise ValueError(f"duplicated {label}")
        return hits[0]

    stage_1_index, stage_1_rest = find_marker(
        CARD_ACTIONS_STAGE_1_MARKER,
        "Stage 1 marker",
    )
    stage_2_index, stage_2_rest = find_marker(
        CARD_ACTIONS_STAGE_2_MARKER,
        "Stage 2 marker",
    )
    if stage_2_index < stage_1_index:
        raise ValueError("Stage 2 appears before Stage 1")

    stage_1_lines = [stage_1_rest, *lines[stage_1_index + 1 : stage_2_index]]
    if not any(line.strip() for line in stage_1_lines):
        raise ValueError("Stage 1 section is empty")

    stage_2_lines = [stage_2_rest, *lines[stage_2_index + 1 :]]
    if not any(line.strip() for line in stage_2_lines):
        raise ValueError("Stage 2 section is empty")

    return {
        "preamble": "\n".join(lines[:stage_1_index]),
        "stage1": "\n".join(stage_1_lines),
        "stage2": "\n".join(stage_2_lines),
        "stage2_intro": stage_2_rest,
    }


def assert_card_actions_rollout_contract(text):
    rollout = parse_card_actions_rollout_sections(text)
    preamble = normalize_semantic_text(rollout["preamble"])
    stage1 = normalize_semantic_text(rollout["stage1"])
    stage2 = normalize_semantic_text(rollout["stage2"])
    stage2_intro = normalize_semantic_text(rollout["stage2_intro"])

    if "The two stages must not be merged." not in preamble:
        raise AssertionError("missing cannot-merge warning before Stage 1")

    for marker in CARD_ACTIONS_STAGE1_REQUIRED_MARKERS:
        if marker not in stage1:
            raise AssertionError(f"missing Stage 1 marker: {marker}")

    for forbidden in CARD_ACTIONS_STAGE2_ONLY_MARKERS:
        if forbidden in preamble:
            raise AssertionError(f"Preamble must not contain {forbidden}")
    for forbidden in CARD_ACTIONS_STAGE2_ONLY_MARKERS:
        if forbidden in stage1:
            raise AssertionError(f"Stage 1 must not contain {forbidden}")

    expected_preamble = normalize_semantic_text(
        "\n".join(CARD_ACTIONS_VALID_PREAMBLE_LINES)
    )
    if preamble != expected_preamble:
        raise AssertionError("Preamble must match canonical content exactly")

    expected_stage1 = normalize_semantic_text(
        "\n".join(CARD_ACTIONS_VALID_STAGE1_LINES)
    )
    if stage1 != expected_stage1:
        raise AssertionError("Stage 1 must match canonical content exactly")

    if stage2_intro != CARD_ACTIONS_STAGE_2_GATE:
        raise AssertionError("Stage 2 must start with the exact gate phrase")

    for marker in CARD_ACTIONS_STAGE2_REQUIRED_MARKERS:
        if marker not in stage2:
            raise AssertionError(f"missing Stage 2 marker: {marker}")

    expected_stage2 = normalize_semantic_text(
        "\n".join((CARD_ACTIONS_STAGE_2_GATE, *CARD_ACTIONS_VALID_STAGE2_LINES))
    )
    if stage2 != expected_stage2:
        raise AssertionError("Stage 2 must match canonical content exactly")

    return rollout


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
        for service in (
            "postgres:", "temporal:", "temporal-ui:", "trigger:", "worker:",
            "minio:", "minio-init:",
        ):
            self.assertIn(service, text)
        self.assertIn(
            '${TRIGGER_BIND_IP:-0.0.0.0}:${TRIGGER_HOST_PORT:-18090}:8090',
            text,
        )
        # 决策 2(UAT LAN 暴露):worker callbacks 绑 IP 变量化,缺省 0.0.0.0。
        self.assertIn(
            '${WORKER_CALLBACKS_BIND_IP:-0.0.0.0}:${WORKER_CALLBACKS_HOST_PORT:-18091}:8091',
            text,
        )
        self.assertIn(
            '127.0.0.1:${TEMPORAL_UI_HOST_PORT:-18080}:8080',
            text,
        )
        self.assertIn(
            '${MINIO_BIND_IP:-0.0.0.0}:${MINIO_HOST_PORT:-9000}:9000',
            text,
        )
        self.assertIn(
            '127.0.0.1:${MINIO_CONSOLE_PORT:-9001}:9001',
            text,
        )
        self.assertIn("TEMPORAL_ADDRESS: temporal:7233", text)
        self.assertIn("TEMPORAL_TASK_QUEUE: device-test", text)
        self.assertNotIn("network_mode: host", text)
        self.assertNotIn("container_name:", text)

    def test_worker_propagates_card_actions_opt_in_with_strict_default_off(self):
        assert_worker_card_actions_opt_in(COMPOSE.read_text(encoding="utf-8"))

    def test_worker_rejects_commented_out_card_actions_opt_in_line(self):
        text = COMPOSE.read_text(encoding="utf-8").replace(
            WORKER_CARD_ACTIONS_OPT_IN_LINE,
            f"      # {WORKER_CARD_ACTIONS_OPT_IN_LINE.strip()}",
            1,
        )

        with self.assertRaisesRegex(
            AssertionError,
            "missing active six-space FEISHU_CARD_ACTIONS_ENABLED worker environment key",
        ):
            assert_worker_card_actions_opt_in(text)

    def test_worker_rejects_wrongly_indented_card_actions_opt_in_line(self):
        text = COMPOSE.read_text(encoding="utf-8").replace(
            WORKER_CARD_ACTIONS_OPT_IN_LINE,
            f"  {WORKER_CARD_ACTIONS_OPT_IN_LINE}",
            1,
        )

        with self.assertRaisesRegex(
            AssertionError,
            "missing active six-space FEISHU_CARD_ACTIONS_ENABLED worker environment key",
        ):
            assert_worker_card_actions_opt_in(text)

    def test_worker_rejects_wrong_card_actions_opt_in_default(self):
        text = COMPOSE.read_text(encoding="utf-8").replace(
            WORKER_CARD_ACTIONS_OPT_IN_LINE,
            "      FEISHU_CARD_ACTIONS_ENABLED: ${FEISHU_CARD_ACTIONS_ENABLED:-true}",
            1,
        )

        with self.assertRaisesRegex(
            AssertionError,
            (
                "FEISHU_CARD_ACTIONS_ENABLED worker environment value must equal "
                r"\$\{FEISHU_CARD_ACTIONS_ENABLED:-false\}"
            ),
        ):
            assert_worker_card_actions_opt_in(text)

    def test_worker_rejects_duplicate_card_actions_opt_in_keys(self):
        text = COMPOSE.read_text(encoding="utf-8").replace(
            WORKER_CARD_ACTIONS_OPT_IN_LINE,
            (
                f"{WORKER_CARD_ACTIONS_OPT_IN_LINE}\n"
                f"{WORKER_CARD_ACTIONS_OPT_IN_LINE}"
            ),
            1,
        )

        with self.assertRaisesRegex(
            AssertionError,
            "duplicate active FEISHU_CARD_ACTIONS_ENABLED worker environment keys",
        ):
            assert_worker_card_actions_opt_in(text)

    def test_worker_rejects_card_actions_opt_in_moved_under_labels(self):
        text = COMPOSE.read_text(encoding="utf-8").replace(
            f"{WORKER_CARD_ACTIONS_OPT_IN_LINE}\n",
            "",
            1,
        ).replace(
            "    ports:\n"
            '      - "${WORKER_CALLBACKS_BIND_IP:-0.0.0.0}:'
            '${WORKER_CALLBACKS_HOST_PORT:-18091}:8091"',
            "    labels:\n"
            f"{WORKER_CARD_ACTIONS_OPT_IN_LINE}\n"
            "    ports:\n"
            '      - "${WORKER_CALLBACKS_BIND_IP:-0.0.0.0}:'
            '${WORKER_CALLBACKS_HOST_PORT:-18091}:8091"',
            1,
        )

        with self.assertRaisesRegex(
            AssertionError,
            "missing active six-space FEISHU_CARD_ACTIONS_ENABLED worker environment key",
        ):
            assert_worker_card_actions_opt_in(text)

    def test_compose_never_publishes_internal_ports(self):
        text = COMPOSE.read_text(encoding="utf-8")
        # PostgreSQL and Temporal gRPC must stay inside the Compose network.
        self.assertNotIn("5432:5432", text)
        self.assertNotIn("7233:7233", text)
        # 决策 2:8091(worker callbacks)改为参数化 bind IP,不再强制 localhost;
        # 但 bind IP 必须来自变量,不得硬编码。
        self.assertIn(
            '${WORKER_CALLBACKS_BIND_IP:-0.0.0.0}:${WORKER_CALLBACKS_HOST_PORT:-18091}:8091',
            text,
        )
        # Temporal UI(8080)与 MinIO 控制台(9001)仍必须绑 localhost;
        # 为它们添加 0.0.0.0 映射必须使本契约失败。
        for line in text.splitlines():
            stripped = line.strip()
            if stripped.startswith("- ") and (
                ":8080" in stripped or ":9001" in stripped
            ):
                self.assertIn("127.0.0.1:", stripped, stripped)

    def test_compose_minio_services(self):
        text = COMPOSE.read_text(encoding="utf-8")
        for marker in (
            "minio:",
            "minio-init:",
            "MINIO_IMAGE",
            "MINIO_MC_IMAGE",
            "server /data",
            "mc mb --ignore-existing",
            'restart: "no"',
            "hermes-runtime-minio",
            "/minio/health/live",
            "condition: service_healthy",
        ):
            self.assertIn(marker, text)
        # worker 不 depends_on minio:预签名缺失时优雅降级(§3.7)。
        worker_block = text.split("  worker:", 1)[1].split("  minio:", 1)[0]
        self.assertNotRegex(
            worker_block,
            re.compile(r"depends_on:\n(\s+\w+:\n\s+condition: service_healthy\n)*\s+minio:"),
        )

    def test_compose_minio_lifecycle_rules(self):
        """生命周期按前缀分:runs/ 到期清理,evidence/ 不设过期(随 decisions 长期)。"""
        text = COMPOSE.read_text(encoding="utf-8")
        init_block = text.split("  minio-init:", 1)[1]
        # 幂等:先清空再重建,重复执行不叠加规则。
        self.assertIn("mc ilm rule rm --all --force", init_block)
        self.assertIn('--prefix "runs/"', init_block)
        self.assertIn("MINIO_RUNS_RETAIN_DAYS", init_block)
        # evidence/ 绝不能有过期规则——快照是 decisions 的回放依据(差距 #6)。
        self.assertNotIn('--prefix "evidence/"', init_block)
        self.assertIn("MINIO_RUNS_RETAIN_DAYS=", ENV_EXAMPLE.read_text(encoding="utf-8"))

    def test_compose_health_chain_and_in_container_probes(self):
        text = COMPOSE.read_text(encoding="utf-8")
        # postgres → temporal → trigger → worker, plus temporal-ui → temporal.
        self.assertGreaterEqual(text.count("condition: service_healthy"), 4)
        for probe in (
            "pg_isready",
            # auto-setup binds the frontend to the container IP, not 127.0.0.1.
            'tctl --address \\"$$(hostname -i):7233\\" cluster health',
            "wget, -qO-, http://127.0.0.1:8090/healthz",
            "wget, -qO-, http://127.0.0.1:8091/healthz",
        ):
            self.assertIn(probe, text)
        self.assertNotIn("sleep ", text)

    def test_compose_requires_locked_third_party_images(self):
        text = COMPOSE.read_text(encoding="utf-8")
        for variable in (
            "POSTGRES_IMAGE",
            "TEMPORAL_IMAGE",
            "TEMPORAL_UI_IMAGE",
            "GO_IMAGE",
            "RUNTIME_BASE_IMAGE",
            "MINIO_IMAGE",
            "MINIO_MC_IMAGE",
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
            "MINIO_ROOT_PASSWORD",
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


class WorkflowRunsDeploymentContracts(unittest.TestCase):
    def test_migration_rekeys_artifacts_and_creates_workflow_runs(self):
        migration = strip_sql_comments(
            WORKFLOW_RUNS_MIGRATION.read_text(encoding="utf-8")
        )

        for label, pattern in WORKFLOW_RUNS_MIGRATION_PATTERNS.items():
            with self.subTest(statement=label):
                self.assertRegex(migration, pattern)

    def test_commented_migration_statements_do_not_satisfy_contract(self):
        migration = WORKFLOW_RUNS_MIGRATION.read_text(encoding="utf-8")
        mutations = {
            "line comments": "\n".join(
                f"-- {line}" for line in migration.splitlines()
            ),
            "block comment": f"/*\n{migration}\n*/",
            "nested block comment": (
                f"/* outer\n/* nested marker */\n{migration}\n*/"
            ),
        }

        for name, mutation in mutations.items():
            stripped = strip_sql_comments(mutation)
            for label, pattern in WORKFLOW_RUNS_MIGRATION_PATTERNS.items():
                with self.subTest(mutation=name, statement=label):
                    self.assertNotRegex(stripped, pattern)

    def test_documented_rollout_order_and_prerequisites_are_explicit(self):
        deploy_readme = " ".join(
            DEPLOY_README.read_text(encoding="utf-8").split()
        )

        rollout_order = (
            "prior batch stable -> stop all old artifact writers and Feishu command "
            "listeners -> migrate -> update and restart "
            "analyze_bridge on every hermes-agent host -> deploy all new binaries -> resume"
        )
        mismatch_warning = (
            "A forward or reverse v1/v2 mismatch breaks all natural-language commands."
        )
        for path in (DEPLOY_README, WORKFLOW_RUNS_SPEC):
            documented = " ".join(path.read_text(encoding="utf-8").split())
            with self.subTest(path=path):
                self.assertIn(rollout_order, documented)
                self.assertIn("command.schema.json v2", documented)
                self.assertIn(mismatch_warning, documented)
                self.assertIn(
                    "Only resume command and artifact ingress after analyze_bridge "
                    "and all new binaries are on v2.",
                    documented,
                )
        self.assertIn(
            "The already-merged presign/evidence-v3/attribution batch must "
            "be deployed and observed stable first.",
            deploy_readme,
        )
        self.assertIn(
            "Merging the workflow_runs branch does not authorize the "
            "production migration.",
            deploy_readme,
        )
        self.assertIn(
            "Stop all old artifact writers and Feishu command listeners before "
            "removing the old unique constraint",
            deploy_readme,
        )

    def test_current_docs_do_not_advertise_legacy_rerun_syntax(self):
        for path in CURRENT_OPERATIONAL_DOCS:
            with self.subTest(path=path.relative_to(ROOT)):
                self.assertNotRegex(
                    path.read_text(encoding="utf-8"),
                    LEGACY_RERUN_SYNTAX,
                )

    def test_legacy_rerun_detector_covers_placeholders_and_concrete_ids(self):
        mutations = (
            "rerun <sha> <pipeline_iid>",
            "rerun <sha8> <pipeline_id> variant",
            "rerun <sha40> <pipeline_iid>",
            "rerun <commit_sha> <pipeline_id>",
            "rerun 9da3b9d9 56",
            "rerun abcdef1234567890 1 variant",
            "`rerun <commit_sha> <pipeline_id>`",
            "`rerun 9da3b9d9 56`",
        )

        for mutation in mutations:
            with self.subTest(mutation=mutation):
                self.assertRegex(mutation, LEGACY_RERUN_SYNTAX)

        for current in (
            "`rerun device-test-grp/project-g9da3b9d9-p56`",
            "rerun device-test-grp/project-g9da3b9d9-p56 variant",
        ):
            with self.subTest(current=current):
                self.assertNotRegex(current, LEGACY_RERUN_SYNTAX)

    def test_rerun_variant_selection_predicate_is_explicit(self):
        deploy_readme = " ".join(
            DEPLOY_README.read_text(encoding="utf-8").split()
        )
        sequence = " ".join((
            ROOT / "docs" / "device-test-sequence.md"
        ).read_text(encoding="utf-8").split()).replace("`", "")

        self.assertIn(
            "verdict != PASSED && verdict != SKIPPED",
            deploy_readme,
        )
        self.assertIn(
            "explicit variant remains allowed when it belongs to the source run, "
            "including PASSED or SKIPPED",
            deploy_readme,
        )
        self.assertIn(
            "verdict != PASSED && verdict != SKIPPED",
            sequence,
        )
        self.assertIn(
            "显式 variant 只要属于源 run 就仍可重跑，包括 PASSED 或 SKIPPED",
            sequence,
        )

    def test_attempt_allocation_and_workflow_id_derivation_are_precise(self):
        sequence = " ".join((
            ROOT / "docs" / "device-test-sequence.md"
        ).read_text(encoding="utf-8").split()).replace("`", "")

        self.assertNotIn("原子分配新的 attempt 和 workflow ID", sequence)
        self.assertIn("原子递增分配新的 attempt", sequence)
        self.assertIn("workflow ID 随后由输入确定性派生", sequence)
        self.assertIn("StartDeviceTest 失败会留下 attempt 空洞", sequence)

    def test_nl_design_scopes_compatibility_promise(self):
        design = " ".join(
            NL_DESIGN.read_text(encoding="utf-8").split()
        ).replace("`", "")

        for stale in (
            "执行路径一个字节不变",
            "对既有指令零回归风险",
            "行为与本轮改动前逐字节一致",
        ):
            self.assertNotIn(stale, design)
        self.assertIn("翻译层不会分叉当前 Parse/execute", design)
        self.assertIn("非 rerun 手打指令保留既有行为", design)
        self.assertIn("legacy rerun 有意 fail closed 并返回迁移提示", design)


class CardActionsDeploymentContracts(unittest.TestCase):
    def test_valid_rollout_fixture_satisfies_contract(self):
        rollout = assert_card_actions_rollout_contract(
            build_valid_card_actions_rollout_doc()
        )

        self.assertIn(
            "Only after the workflow_runs rollout is stable",
            normalize_semantic_text(rollout["stage2"]),
        )

    def test_rollout_section_parser_reads_only_the_dedicated_section(self):
        rollout = parse_card_actions_rollout_sections(
            DEPLOY_README.read_text(encoding="utf-8")
        )

        self.assertIn(
            "The two stages must not be merged.",
            normalize_whitespace(rollout["preamble"]),
        )
        self.assertIn(
            "config.update_multi: true",
            normalize_semantic_text(rollout["stage1"]),
        )
        self.assertIn(
            "CardElement.Actions",
            normalize_semantic_text(rollout["stage1"]),
        )
        self.assertIn(
            "Only after the workflow_runs rollout is stable",
            normalize_semantic_text(rollout["stage2"]),
        )
        self.assertIn(
            "Apply deploy/postgres/migrations/2026-08-01-card-actions.sql",
            normalize_semantic_text(rollout["stage2"]),
        )
        self.assertNotIn(
            "deploy/postgres/migrations/2026-08-01-card-actions.sql",
            normalize_semantic_text(rollout["stage1"]),
        )
        self.assertNotIn(
            "FEISHU_CARD_ACTIONS_ENABLED=true",
            normalize_semantic_text(rollout["stage1"]),
        )

    def test_semantic_heading_scanner_normalizes_closing_hashes_and_offsets(self):
        text = (
            "Intro\n"
            "### Card-action two-stage rollout ###   \n"
            "Body\n"
        )

        headings = scan_semantic_atx_headings(text)

        self.assertEqual(1, len(headings))
        self.assertEqual(3, headings[0].level)
        self.assertEqual(CARD_ACTIONS_ROLLOUT_HEADING, headings[0].title)
        self.assertEqual(
            text.index("### Card-action two-stage rollout ###"),
            headings[0].start,
        )
        self.assertEqual(
            "### Card-action two-stage rollout ###   ",
            text[headings[0].start : headings[0].end],
        )

    def test_semantic_heading_scanner_accepts_up_to_three_leading_spaces(self):
        for indent in range(4):
            with self.subTest(indent=indent):
                headings = scan_semantic_atx_headings(
                    f"{' ' * indent}### {CARD_ACTIONS_ROLLOUT_HEADING} ###\n"
                )

                self.assertEqual(1, len(headings))
                self.assertEqual(3, headings[0].level)
                self.assertEqual(CARD_ACTIONS_ROLLOUT_HEADING, headings[0].title)

    def test_semantic_heading_scanner_ignores_four_space_indented_lines(self):
        headings = scan_semantic_atx_headings(
            f"    ### {CARD_ACTIONS_ROLLOUT_HEADING} ###\n"
        )

        self.assertEqual([], headings)

    def test_semantic_heading_scanner_ignores_missing_separator(self):
        headings = scan_semantic_atx_headings(
            f"###{CARD_ACTIONS_ROLLOUT_HEADING} ###\n"
        )

        self.assertEqual([], headings)

    def test_rollout_section_parser_rejects_duplicate_rollout_headings(self):
        doc = build_valid_card_actions_rollout_doc() + (
            "\n### Card-action two-stage rollout\n\n"
            "Stage 2 may begin during the workflow_runs stop-write window.\n"
        )

        with self.assertRaisesRegex(
            ValueError,
            re.escape(
                "duplicated ### Card-action two-stage rollout section"
            ),
        ):
            parse_card_actions_rollout_sections(doc)

    def test_rollout_section_parser_rejects_render_equivalent_duplicates(self):
        for duplicate_heading in (
            "### **Card-action two-stage rollout**",
            "### [Card-action two-stage rollout](#card-actions)",
        ):
            with self.subTest(duplicate_heading=duplicate_heading):
                doc = build_valid_card_actions_rollout_doc() + (
                    f"\n{duplicate_heading}\n\n"
                    "Stage 2 may begin during the workflow_runs "
                    "stop-write window.\n"
                )

                with self.assertRaisesRegex(
                    ValueError,
                    re.escape(
                        "duplicated ### Card-action two-stage rollout section"
                    ),
                ):
                    parse_card_actions_rollout_sections(doc)

    def test_rollout_section_parser_rejects_formatted_heading_without_exact_match_as_missing_section(self):
        doc = (
            "\n### **Card-action two-stage rollout**\n\n"
            "Stage 2 may begin during the workflow_runs "
            "stop-write window.\n"
        )

        with self.assertRaisesRegex(
            ValueError,
            re.escape(
                "missing ### Card-action two-stage rollout section"
            ),
        ):
            parse_card_actions_rollout_sections(doc)

    def test_rollout_section_parser_ignores_unrelated_h3_headings(self):
        doc = build_valid_card_actions_rollout_doc() + (
            "\n### Stage 2 follow-up\n\n"
            "This unrelated section must not count as a duplicate.\n"
        )

        assert_card_actions_rollout_contract(doc)

    def test_rollout_section_parser_stops_at_next_h3_and_keeps_h4_content(self):
        doc = "\n".join(
            (
                f"### {CARD_ACTIONS_ROLLOUT_HEADING}",
                "",
                *CARD_ACTIONS_VALID_PREAMBLE_LINES,
                "",
                f"{CARD_ACTIONS_STAGE_1_MARKER} "
                f"{CARD_ACTIONS_VALID_STAGE1_LINES[0]}",
                *CARD_ACTIONS_VALID_STAGE1_LINES[1:],
                "",
                f"{CARD_ACTIONS_STAGE_2_MARKER} {CARD_ACTIONS_STAGE_2_GATE}",
                *CARD_ACTIONS_VALID_STAGE2_LINES,
                "",
                "#### Nested follow-up",
                "Keep this guidance inside Stage 2.",
                "",
                "### Sibling section",
                "Do not include this sibling content.",
            )
        ) + "\n"

        rollout = parse_card_actions_rollout_sections(doc)

        self.assertIn("#### Nested follow-up", rollout["stage2"])
        self.assertIn("Keep this guidance inside Stage 2.", rollout["stage2"])
        self.assertNotIn("### Sibling section", rollout["stage2"])
        self.assertNotIn(
            "Do not include this sibling content.",
            rollout["stage2"],
        )

    def test_rollout_section_parser_rejects_reversed_stages(self):
        doc = build_valid_card_actions_rollout_doc().replace(
            (
                f"{CARD_ACTIONS_STAGE_1_MARKER} "
                f"{CARD_ACTIONS_VALID_STAGE1_LINES[0]}\n"
                f"{CARD_ACTIONS_VALID_STAGE1_LINES[1]}\n"
                f"{CARD_ACTIONS_VALID_STAGE1_LINES[2]}\n\n"
                f"{CARD_ACTIONS_STAGE_2_MARKER} {CARD_ACTIONS_STAGE_2_GATE}\n"
                f"{CARD_ACTIONS_VALID_STAGE2_LINES[0]}\n"
            ),
            (
                f"{CARD_ACTIONS_STAGE_2_MARKER} {CARD_ACTIONS_STAGE_2_GATE}\n"
                f"{CARD_ACTIONS_VALID_STAGE2_LINES[0]}\n\n"
                f"{CARD_ACTIONS_STAGE_1_MARKER} "
                f"{CARD_ACTIONS_VALID_STAGE1_LINES[0]}\n"
                f"{CARD_ACTIONS_VALID_STAGE1_LINES[1]}\n"
                f"{CARD_ACTIONS_VALID_STAGE1_LINES[2]}\n"
            ),
            1,
        )

        with self.assertRaisesRegex(ValueError, "Stage 2 appears before Stage 1"):
            parse_card_actions_rollout_sections(doc)

    def test_rollout_section_parser_rejects_stage2_wording_changes(self):
        doc = build_valid_card_actions_rollout_doc(
            stage2_intro="Only after workflow_runs is stable:"
        )

        with self.assertRaisesRegex(
            AssertionError,
            "Stage 2 must start with the exact gate phrase",
        ):
            assert_card_actions_rollout_contract(doc)

    def test_rollout_section_parser_rejects_stage2_gate_suffix(self):
        doc = build_valid_card_actions_rollout_doc(
            stage2_intro=(
                f"{CARD_ACTIONS_STAGE_2_GATE} after the prerequisite batch finishes"
            )
        )

        with self.assertRaisesRegex(
            AssertionError,
            "Stage 2 must start with the exact gate phrase",
        ):
            assert_card_actions_rollout_contract(doc)

    def test_rollout_section_parser_rejects_migration_inside_stage1(self):
        doc = build_valid_card_actions_rollout_doc(
            stage1_extra_lines=(
                "Apply `deploy/postgres/migrations/2026-08-01-card-actions.sql`.",
            )
        )

        with self.assertRaisesRegex(
            AssertionError,
            re.escape(
                "Stage 1 must not contain "
                "deploy/postgres/migrations/2026-08-01-card-actions.sql"
            ),
        ):
            assert_card_actions_rollout_contract(doc)

    def test_rollout_section_parser_rejects_stage2_markers_in_preamble(self):
        doc = build_valid_card_actions_rollout_doc(
            preamble_extra_lines=(
                "Apply `deploy/postgres/migrations/2026-08-01-card-actions.sql`.",
                "Set `FEISHU_CARD_ACTIONS_ENABLED=true` only after the rollout.",
            )
        )

        with self.assertRaisesRegex(
            AssertionError,
            re.escape(
                "Preamble must not contain "
                "deploy/postgres/migrations/2026-08-01-card-actions.sql"
            ),
        ):
            assert_card_actions_rollout_contract(doc)

    def test_rollout_section_parser_rejects_paraphrased_operations_in_preamble(self):
        doc = build_valid_card_actions_rollout_doc(
            preamble_extra_lines=(
                "Before Stage 1, apply the five-table card-action database migration and enable the feature flag.",
            )
        )

        with self.assertRaisesRegex(
            AssertionError,
            "Preamble must match canonical content exactly",
        ):
            assert_card_actions_rollout_contract(doc)

    def test_rollout_section_parser_rejects_paraphrased_operations_in_stage1(self):
        doc = build_valid_card_actions_rollout_doc(
            stage1_extra_lines=(
                "Configure Feishu to deliver interactive-card clicks over its persistent socket, then run the five-table database upgrade and switch on card actions.",
            )
        )

        with self.assertRaisesRegex(
            AssertionError,
            "Stage 1 must match canonical content exactly",
        ):
            assert_card_actions_rollout_contract(doc)

    def test_rollout_section_parser_rejects_stage2_subscription_markers_in_stage1(self):
        doc = build_valid_card_actions_rollout_doc(
            stage1_extra_lines=(
                "Subscribe to `card.action.trigger` under “事件与回调 → 回调” using the long connection; no public callback URL is required.",
                "Runtime handler registration alone does not prove the platform subscription exists.",
            )
        )

        with self.assertRaisesRegex(
            AssertionError,
            re.escape("Stage 1 must not contain card.action.trigger"),
        ):
            assert_card_actions_rollout_contract(doc)

    def test_rollout_section_parser_rejects_subscription_markers_in_sibling_h3(self):
        doc = build_valid_card_actions_rollout_doc().replace(
            "WebSocket listener is running. In Feishu Open Platform, subscribe to",
            (
                "WebSocket listener is running.\n\n"
                "### Unrelated follow-up\n\n"
                "In Feishu Open Platform, subscribe to"
            ),
            1,
        )

        with self.assertRaisesRegex(
            AssertionError,
            re.escape("missing Stage 2 marker: card.action.trigger"),
        ):
            assert_card_actions_rollout_contract(doc)

    def test_rollout_section_parser_rejects_relocated_stability_gate(self):
        doc = build_valid_card_actions_rollout_doc().replace(
            (
                "**Stage 2 — card actions.** Only after the workflow_runs "
                "rollout is stable:"
            ),
            (
                "**Stage 2 — card actions.** Prepare the rollout:\n\n"
                "Only after the workflow_runs rollout is stable:"
            ),
            1,
        )

        with self.assertRaisesRegex(
            AssertionError,
            "Stage 2 must start with the exact gate phrase",
        ):
            assert_card_actions_rollout_contract(doc)

    def test_rollout_section_parser_rejects_extra_stage2_guidance(self):
        doc = build_valid_card_actions_rollout_doc(
            stage2_extra_lines=(
                "Enable the feature during the workflow_runs stop-write window.",
            )
        )

        with self.assertRaisesRegex(
            AssertionError,
            "Stage 2 must match canonical content exactly",
        ):
            assert_card_actions_rollout_contract(doc)

    def test_rollout_is_explicitly_ordered_and_not_merged(self):
        rollout = assert_card_actions_rollout_contract(
            DEPLOY_README.read_text(encoding="utf-8")
        )

        self.assertIn(
            "The two stages must not be merged.",
            normalize_whitespace(rollout["preamble"]),
        )
        for marker in (
            "config.update_multi: true",
            "CardElement.Actions",
            "The workflow never sets Actions",
            "cards still have no buttons",
            "behavior is unchanged",
            "newly sent messages become updateable",
        ):
            self.assertIn(marker, normalize_semantic_text(rollout["stage1"]))
        for marker in (
            "deploy/postgres/migrations/2026-08-01-card-actions.sql",
            "card_action_inbox",
            "card_actions",
            "card_action_messages",
            "card_action_snapshots",
            "audit_log",
            "FEISHU_CARD_ACTIONS_ENABLED=true",
            "strict opt-in and defaults to false",
            "FEISHU_APP_ID",
            "FEISHU_APP_SECRET",
            "FEISHU_RECEIVE_ID",
            "FEISHU_CMD_WHITELIST",
            "Stage 2 must not be mixed into the workflow_runs stop-write window.",
            "artifact unique key",
            "analyze_bridge v2",
            "failures unattributable and impossible to isolate",
            "card.action.trigger",
            "事件与回调 → 回调",
            "long connection",
            "no public callback URL",
            "Runtime handler registration alone does not prove the platform subscription exists",
        ):
            self.assertIn(marker, normalize_semantic_text(rollout["stage2"]))

    def test_feishu_platform_subscription_is_an_explicit_prerequisite(self):
        rollout = assert_card_actions_rollout_contract(
            DEPLOY_README.read_text(encoding="utf-8")
        )
        stage2 = normalize_semantic_text(rollout["stage2"])

        for marker in (
            "card.action.trigger",
            "事件与回调 → 回调",
            "long connection",
            "no public callback URL",
            "Runtime handler registration alone does not prove the platform subscription exists",
        ):
            self.assertIn(marker, stage2)

    def test_card_actions_default_off_rejects_duplicate_true_assignment(self):
        env_example = ENV_EXAMPLE.read_text(encoding="utf-8").replace(
            "FEISHU_CARD_ACTIONS_ENABLED=false",
            (
                "FEISHU_CARD_ACTIONS_ENABLED=false\n"
                "FEISHU_CARD_ACTIONS_ENABLED=true"
            ),
            1,
        )

        with self.assertRaisesRegex(
            AssertionError,
            "duplicate active FEISHU_CARD_ACTIONS_ENABLED assignments",
        ):
            assert_card_actions_default_off_env_assignment(env_example)

    def test_card_actions_default_off_rejects_missing_commented_and_wrong_value(self):
        cases = {
            "missing": (
                ENV_EXAMPLE.read_text(encoding="utf-8").replace(
                    "FEISHU_CARD_ACTIONS_ENABLED=false\n",
                    "",
                    1,
                ),
                "missing active FEISHU_CARD_ACTIONS_ENABLED assignment",
            ),
            "commented only": (
                ENV_EXAMPLE.read_text(encoding="utf-8").replace(
                    "FEISHU_CARD_ACTIONS_ENABLED=false",
                    "# FEISHU_CARD_ACTIONS_ENABLED=false",
                    1,
                ),
                "missing active FEISHU_CARD_ACTIONS_ENABLED assignment",
            ),
            "wrong value": (
                ENV_EXAMPLE.read_text(encoding="utf-8").replace(
                    "FEISHU_CARD_ACTIONS_ENABLED=false",
                    "FEISHU_CARD_ACTIONS_ENABLED=true",
                    1,
                ),
                "FEISHU_CARD_ACTIONS_ENABLED value must equal false",
            ),
        }

        for name, (env_example, error) in cases.items():
            with self.subTest(case=name):
                with self.assertRaisesRegex(AssertionError, error):
                    assert_card_actions_default_off_env_assignment(env_example)

    def test_card_actions_are_strictly_default_off(self):
        env_example = ENV_EXAMPLE.read_text(encoding="utf-8")

        assert_card_actions_default_off_env_assignment(env_example)


if __name__ == "__main__":
    unittest.main()
