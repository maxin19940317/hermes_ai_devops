"""Plan DSL v1 (CLAUDE.md §7.1) 的正反例校验测试。"""
import pytest
from jsonschema import ValidationError

from contract_helpers import load_example


class TestPlanSchema:
    contract = "plan"

    def test_valid_examples_pass(self, validators, valid_case):
        validators["plan"].validate(load_example(valid_case))

    def test_invalid_examples_rejected(self, validators, invalid_case):
        with pytest.raises(ValidationError):
            validators["plan"].validate(load_example(invalid_case))
