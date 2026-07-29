"""evidence.json v3 (CLAUDE.md §12 Phase 2, Runtime Evidence Extractor 产出) 的正反例校验测试。"""
import pytest
from jsonschema import ValidationError

from contract_helpers import EXAMPLES_DIR, load_example


class TestEvidenceSchema:
    contract = "evidence"

    def test_valid_examples_pass(self, validators, valid_case):
        validators["evidence"].validate(load_example(valid_case))

    def test_invalid_examples_rejected(self, validators, invalid_case):
        with pytest.raises(ValidationError):
            validators["evidence"].validate(load_example(invalid_case))

    @pytest.mark.parametrize(
        ("fixture_name", "expected_path"),
        [
            ("bad_where_enum.json", ("signatures", 0, "where")),
            ("line_no_below_one.json", ("signatures", 0, "matches", 0, "line_no")),
        ],
    )
    def test_field_invalid_examples_report_intended_path_without_version_error(
        self, validators, fixture_name, expected_path
    ):
        example = load_example(EXAMPLES_DIR / "evidence" / "invalid" / fixture_name)
        error_paths = {
            tuple(error.absolute_path)
            for error in validators["evidence"].iter_errors(example)
        }

        assert expected_path in error_paths
        assert ("evidence_version",) not in error_paths
