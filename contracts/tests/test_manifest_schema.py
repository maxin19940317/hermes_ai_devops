"""Manifest v1 (CLAUDE.md §7.2, 包内 manifest.yaml) 的正反例校验测试。"""
import pytest
from jsonschema import ValidationError

from contract_helpers import example_files, load_example


class TestManifestSchema:
    contract = "manifest"

    def test_valid_examples_pass(self, validators, valid_case):
        validators["manifest"].validate(load_example(valid_case))

    def test_invalid_examples_rejected(self, validators, invalid_case):
        with pytest.raises(ValidationError):
            validators["manifest"].validate(load_example(invalid_case))


@pytest.mark.parametrize(
    "valid_case",
    example_files("manifest", "valid"),
    ids=lambda path: path.stem,
)
def test_relative_paths_allow_android_cpp_runtime(validators, valid_case):
    manifest = load_example(valid_case)
    manifest["deploy"]["files"][0]["src"] = "payload/lib/libc++_shared.so"
    manifest["deploy"]["files"][0]["dst"] = "lib/libc++_shared.so"
    validators["manifest"].validate(manifest)
