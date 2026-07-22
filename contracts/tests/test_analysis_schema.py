"""analysis.json v1 (CLAUDE.md §12 Phase 2, Analyzer LLM 结构化输出) 的正反例校验测试。"""
import pytest
from jsonschema import ValidationError

from contract_helpers import load_example


class TestAnalysisSchema:
    contract = "analysis"

    def test_valid_examples_pass(self, validators, valid_case):
        validators["analysis"].validate(load_example(valid_case))

    def test_invalid_examples_rejected(self, validators, invalid_case):
        with pytest.raises(ValidationError):
            validators["analysis"].validate(load_example(invalid_case))
