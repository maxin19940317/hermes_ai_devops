"""express.schema.json(表述层输出契约)的正反例校验测试。

事实永远由规则计算,LLM 只负责表述;输出封闭结构 summary/sections/footer。
"""
import pytest
from jsonschema import ValidationError

from contract_helpers import load_example


class TestExpressSchema:
    contract = "express"

    def test_valid_examples_pass(self, validators, valid_case):
        validators["express"].validate(load_example(valid_case))

    def test_invalid_examples_rejected(self, validators, invalid_case):
        with pytest.raises(ValidationError):
            validators["express"].validate(load_example(invalid_case))

    def test_missing_summary_rejected(self, validators):
        with pytest.raises(ValidationError):
            validators["express"].validate({"sections": ["x"], "footer": ""})

    def test_sections_too_many_rejected(self, validators):
        with pytest.raises(ValidationError):
            validators["express"].validate({
                "summary": "s",
                "sections": [f"s{i}" for i in range(7)],
                "footer": "",
            })

    def test_extra_field_rejected(self, validators):
        with pytest.raises(ValidationError):
            validators["express"].validate({
                "summary": "s", "sections": ["x"], "footer": "",
                "facts": "should not leak",
            })
