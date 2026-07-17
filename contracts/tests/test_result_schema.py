"""result.json v1 (CLAUDE.md §7.3, 测试脚本产出) 的正反例校验测试。"""
import pytest
from jsonschema import ValidationError

from contract_helpers import load_example


class TestResultSchema:
    contract = "result"

    def test_valid_examples_pass(self, validators, valid_case):
        validators["result"].validate(load_example(valid_case))

    def test_invalid_examples_rejected(self, validators, invalid_case):
        with pytest.raises(ValidationError):
            validators["result"].validate(load_example(invalid_case))
