"""bundle-g{sha}.json 的正反例校验测试。

Trigger 服务只认 bundle(CLAUDE.md §6.3),故 bundle 是跨组件契约。
"8 个变体齐全"属于语义校验,由 ci/gen_bundle.py 负责,Schema 只管结构。
"""
import pytest
from jsonschema import ValidationError

from contract_helpers import load_example


class TestBundleSchema:
    contract = "bundle"

    def test_valid_examples_pass(self, validators, valid_case):
        validators["bundle"].validate(load_example(valid_case))

    def test_invalid_examples_rejected(self, validators, invalid_case):
        with pytest.raises(ValidationError):
            validators["bundle"].validate(load_example(invalid_case))
