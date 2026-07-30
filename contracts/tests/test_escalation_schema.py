"""escalation 信封(DevOps → PM 升级通道)的正反例校验测试。

设计:docs/superpowers/specs/2026-07-30-devops-pm-escalation-design.md §3。
"""
import pytest
from jsonschema import ValidationError

from contract_helpers import load_example


class TestEscalationSchema:
    contract = "escalation"

    def test_valid_examples_pass(self, validators, valid_case):
        validators["escalation"].validate(load_example(valid_case))

    def test_invalid_examples_rejected(self, validators, invalid_case):
        with pytest.raises(ValidationError):
            validators["escalation"].validate(load_example(invalid_case))
