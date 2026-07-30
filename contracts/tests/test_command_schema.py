"""command.json v2 (CLAUDE.md §12 Phase 2, 飞书指令层 LLM 翻译输出) 的正反例校验测试。"""
from pathlib import Path

import pytest
from jsonschema import ValidationError

from contract_helpers import load_example


class TestCommandSchema:
    contract = "command"

    def test_valid_examples_pass(self, validators, valid_case):
        validators["command"].validate(load_example(valid_case))

    def test_invalid_examples_rejected(self, validators, invalid_case):
        with pytest.raises(ValidationError):
            validators["command"].validate(load_example(invalid_case))

    def test_rerun_accepts_workflow_id_and_optional_variant(self, validators):
        workflow_id = "device-test-grp/algo-super-sdk-" + "a" * 65
        for args in ([workflow_id], [workflow_id, "aarch64_Android_SNPE_1.68"]):
            validators["command"].validate(
                {
                    "translation_version": 2,
                    "command": "rerun",
                    "args": args,
                    "confidence": 0.9,
                }
            )

    @pytest.mark.parametrize(
        ("command", "args", "valid"),
        [
            ("rerun", [], False),
            ("rerun", ["wf"], True),
            ("rerun", ["wf", "variant"], True),
            ("rerun", ["wf", "variant", "extra"], False),
            ("status", [], True),
            ("status", ["extra"], False),
            ("devices", [], True),
            ("devices", ["extra"], False),
            ("none", [], True),
            ("none", ["extra"], False),
            ("unquarantine", [], True),
            ("unquarantine", ["dev-1"], True),
            ("unquarantine", ["dev-1", "dev-2"], False),
        ],
    )
    def test_command_arities_are_exact(self, validators, command, args, valid):
        doc = {
            "translation_version": 2,
            "command": command,
            "args": args,
            "confidence": 0.9,
        }
        if valid:
            validators["command"].validate(doc)
        else:
            with pytest.raises(ValidationError):
                validators["command"].validate(doc)

    @pytest.mark.parametrize(
        ("command", "valid"),
        [
            ("rerun", False),
            ("status", True),
            ("devices", True),
            ("none", True),
            ("unquarantine", True),
        ],
    )
    def test_args_omission_matches_command_contract(self, validators, command, valid):
        doc = {
            "translation_version": 2,
            "command": command,
            "confidence": 0.9,
        }
        if valid:
            validators["command"].validate(doc)
        else:
            with pytest.raises(ValidationError):
                validators["command"].validate(doc)

    @pytest.mark.parametrize("arg", ["wf id", "wf\tid", "wf\nid", "wf\rid"])
    def test_args_reject_whitespace_anywhere(self, validators, arg):
        with pytest.raises(ValidationError):
            validators["command"].validate(
                {
                    "translation_version": 2,
                    "command": "rerun",
                    "args": [arg],
                    "confidence": 0.9,
                }
            )

    def test_current_schema_rejects_translation_v1(self, validators):
        with pytest.raises(ValidationError):
            validators["command"].validate(
                {
                    "translation_version": 1,
                    "command": "devices",
                    "args": [],
                    "confidence": 0.9,
                }
            )

    def test_all_schema_copies_are_byte_identical(self):
        contracts = Path(__file__).resolve().parents[1] / "command.schema.json"
        root = contracts.parents[1]
        copies = [
            root / "runtime/internal/hermesclient/command.schema.json",
            root / "hermes/analyze_bridge/command.schema.json",
        ]
        want = contracts.read_bytes()
        for copy in copies:
            assert copy.read_bytes() == want, f"{copy} 与 contracts/command.schema.json 不一致"
