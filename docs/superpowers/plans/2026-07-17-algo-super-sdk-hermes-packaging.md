# Algo_Super_SDK Hermes Test Packaging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce minimal, contract-valid Hermes smoke-test packages for all eight Algo_Super_SDK variants while preserving the existing SDK release packages.

**Architecture:** First update and commit the authoritative Hermes path contract and variant configuration. Then copy that exact Hermes commit into the independent Algo repository, where a separate packager builds a minimal runtime payload, the vendored Hermes tools inject contracts and metadata, and GitLab CI publishes complete bundles only after all eight variants succeed.

**Tech Stack:** Bash, Python 3.9+, pytest, PyYAML, jsonschema, Go, GitLab CI YAML

---

## Repository boundaries

- Hermes repository: `/home/maxin/Code/hermes_ai_devops`
- Algo repository: `/home/maxin/Code/560D/Algo_Super_SDK`
- Use a separate isolated worktree and feature branch for each repository.
- Complete and commit Hermes Tasks 1–2 before Task 3; Task 3 records the resulting Hermes commit in Algo `ci/hermes/REVISION`.
- Never add Algo's pre-existing untracked files. Stage only paths named by this plan.

### Task 1: Allow Android C++ runtime names in the Hermes contract

**Files:**
- Modify: `contracts/tests/test_manifest_schema.py`
- Modify: `contracts/manifest.schema.json:159-164`
- Modify: `agent/internal/manifest/manifest.schema.json:159-164`
- Test: `contracts/tests/test_manifest_schema.py`
- Test: `agent/internal/manifest/manifest_test.go`

- [ ] **Step 1: Add a failing positive contract test**

Append this test to `contracts/tests/test_manifest_schema.py`:

```python
def test_relative_paths_allow_android_cpp_runtime(validators, valid_case):
    manifest = load_example(valid_case)
    manifest["deploy"]["files"][0]["src"] = "payload/lib/libc++_shared.so"
    manifest["deploy"]["files"][0]["dst"] = "lib/libc++_shared.so"
    validators["manifest"].validate(manifest)
```

- [ ] **Step 2: Run the focused test and observe the contract failure**

Run:

```bash
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  contracts/tests/test_manifest_schema.py::test_relative_paths_allow_android_cpp_runtime -q
```

Expected: FAIL because `libc++_shared.so` does not match `relativePath`.

- [ ] **Step 3: Permit `+` in both authoritative and embedded schemas**

Change `$defs.relativePath.pattern` in both schema files from:

```json
"^[A-Za-z0-9._-][A-Za-z0-9._/-]*$"
```

to:

```json
"^[A-Za-z0-9._+-][A-Za-z0-9._+/-]*$"
```

Do not change the existing `not` rule for `..`.

- [ ] **Step 4: Run positive, negative and embedded-schema tests**

Run:

```bash
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest contracts/tests/test_manifest_schema.py -q
cd agent && "$HOME/.local/go/bin/go" test ./internal/manifest
```

Expected: contract tests pass, existing traversal cases remain rejected, and Agent schema drift test passes.

- [ ] **Step 5: Commit the path contract change**

```bash
git add contracts/manifest.schema.json contracts/tests/test_manifest_schema.py \
  agent/internal/manifest/manifest.schema.json
git commit -m "fix(contracts): allow Android C++ runtime paths"
```

### Task 2: Align Hermes variant manifests with generated runners

**Files:**
- Modify: `ci/tests/test_variants_yaml.py`
- Modify: `ci/variants.yaml`
- Test: `ci/tests/test_variants_yaml.py`
- Test: `ci/tests/test_gen_manifest.py`

- [ ] **Step 1: Add failing assertions for runner arguments and environment**

Append to `ci/tests/test_variants_yaml.py`:

```python
def test_generated_runner_needs_no_manifest_arguments():
    defaults, variants = gen_manifest.load_variants(VARIANTS_FILE)
    for variant, vcfg in variants.items():
        manifest = gen_manifest.render_manifest(
            variant=variant, vcfg=vcfg, defaults=defaults,
            file_entries=DUMMY_FILES, project="p", commit="deadbee1",
            pipeline_iid=1, build_type="Release",
        )
        assert manifest["test"]["entry"] == "./run.sh"
        assert manifest["test"]["args"] == [], variant


def test_android_runtime_paths_match_normalized_package_layout():
    defaults, variants = gen_manifest.load_variants(VARIANTS_FILE)
    for variant, vcfg in variants.items():
        if vcfg["requirements"]["os"] != "android":
            continue
        assert vcfg["env"]["LD_LIBRARY_PATH"] == "{workdir}/lib"
        if "SNPE" in variant:
            assert "{workdir}/lib/dsp" in vcfg["env"]["ADSP_LIBRARY_PATH"]
```

- [ ] **Step 2: Run focused tests and observe non-empty args failures**

Run:

```bash
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest ci/tests/test_variants_yaml.py -q
```

Expected: FAIL because current variants render `--suite/--output` args.

- [ ] **Step 3: Set all variant test args to empty lists**

In each of the eight `ci/variants.yaml` entries, replace the suite argument list with:

```yaml
test:
  args: []
```

Keep `defaults.test.entry: ./run.sh`, existing timeout/success rules, Android
`LD_LIBRARY_PATH`, SNPE `ADSP_LIBRARY_PATH`, SoC lists and capabilities unchanged.

- [ ] **Step 4: Run complete Hermes verification**

Run:

```bash
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest contracts/tests ci/tests -q -p no:cacheprovider
cd agent && "$HOME/.local/go/bin/go" test -count=1 ./...
cd ../runtime && "$HOME/.local/go/bin/go" test -count=1 ./...
```

Expected: 48 or more Python tests pass; Agent and Runtime Go tests pass.

- [ ] **Step 5: Commit and record the authoritative snapshot commit**

```bash
git add ci/variants.yaml ci/tests/test_variants_yaml.py
git commit -m "fix(ci): align variants with packaged smoke runners"
git rev-parse HEAD
```

Save the printed full commit as `HERMES_SNAPSHOT_REV`; Task 3 writes it verbatim to Algo.

### Task 3: Vendor the fixed Hermes toolchain into Algo

**Files:**
- Create: `ci/hermes/REVISION`
- Create: `ci/hermes/README.md`
- Create: `ci/hermes/gen_manifest.py`
- Create: `ci/hermes/write_meta.py`
- Create: `ci/hermes/gen_bundle.py`
- Create: `ci/hermes/variants.yaml`
- Create: `ci/hermes/contracts/manifest.schema.json`
- Create: `ci/hermes/contracts/bundle.schema.json`
- Create: `ci/hermes/contracts/result.schema.json`

- [ ] **Step 1: Copy only the approved snapshot files**

From the Algo worktree, copy exact bytes from the completed Hermes worktree:

```bash
mkdir -p ci/hermes/contracts
cp /home/maxin/Code/hermes_ai_devops/ci/{gen_manifest.py,write_meta.py,gen_bundle.py,variants.yaml} ci/hermes/
cp /home/maxin/Code/hermes_ai_devops/contracts/{manifest.schema.json,bundle.schema.json,result.schema.json} ci/hermes/contracts/
git -C /home/maxin/Code/hermes_ai_devops rev-parse HEAD > ci/hermes/REVISION
```

`REVISION` must equal the Task 2 `HERMES_SNAPSHOT_REV`, not a later unrelated commit.

- [ ] **Step 2: Document snapshot ownership and update procedure**

Create `ci/hermes/README.md` with this content:

```markdown
# Hermes CI snapshot

This directory is a pinned copy from `hermes_ai_devops`; Algo CI never downloads
these files at runtime. `REVISION` is the full source commit.

Copied files: `gen_manifest.py`, `write_meta.py`, `gen_bundle.py`, `variants.yaml`,
and the manifest, bundle and result schemas under `contracts/`.

To update, first make and verify the authoritative Hermes change, copy the listed
files byte-for-byte, replace `REVISION`, then run `python3 -m unittest discover
-s scripts/hermes_test_pack/tests -v` and the package verification commands.
```

- [ ] **Step 3: Verify the snapshot byte-for-byte**

Run:

```bash
test "$(cat ci/hermes/REVISION)" = "$(git -C /home/maxin/Code/hermes_ai_devops rev-parse HEAD)"
cmp ci/hermes/gen_manifest.py /home/maxin/Code/hermes_ai_devops/ci/gen_manifest.py
cmp ci/hermes/write_meta.py /home/maxin/Code/hermes_ai_devops/ci/write_meta.py
cmp ci/hermes/gen_bundle.py /home/maxin/Code/hermes_ai_devops/ci/gen_bundle.py
cmp ci/hermes/variants.yaml /home/maxin/Code/hermes_ai_devops/ci/variants.yaml
for name in manifest bundle result; do
  cmp "ci/hermes/contracts/$name.schema.json" \
      "/home/maxin/Code/hermes_ai_devops/contracts/$name.schema.json"
done
```

Expected: every command exits 0.

- [ ] **Step 4: Commit the vendored snapshot**

```bash
git add ci/hermes
git commit -m "build: vendor Hermes CI contracts"
```

### Task 4: Define and test variant smoke mappings

**Files:**
- Create: `scripts/hermes_test_pack/lib/variants.sh`
- Create: `scripts/hermes_test_pack/tests/test_variants.sh`

- [ ] **Step 1: Write a failing shell mapping test**

Create `scripts/hermes_test_pack/tests/test_variants.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
source "$root/scripts/hermes_test_pack/lib/variants.sh"

resolve_smoke_variant aarch64_Android_SNPE_1.68
[[ "$smoke_suite" == snpe-smoke && "$smoke_binary" == seg_crowd_test ]]
[[ "$model_src" == models/seg/human/snpe_1.68/sim.engine ]]
[[ "$input_src" == models/seg/human/human.jpg ]]

resolve_smoke_variant aarch64_Android_RKNN_2.3.2
[[ "$smoke_suite" == rknn-smoke && "$smoke_binary" == ocr_test ]]
[[ "$model_src" == models/ocr/rknn/v6/v6_small/sim.engine ]]

resolve_smoke_variant aarch64_Android_TFLite_2.21.0
[[ "$smoke_suite" == tflite-smoke && "$smoke_binary" == ocr_test ]]
[[ "$model_src" == models/ocr/tflite/v6/v6_small/sim.engine ]]

if resolve_smoke_variant unknown_variant; then
  echo "unknown variant unexpectedly accepted" >&2
  exit 1
fi
```

- [ ] **Step 2: Run it and observe the missing implementation failure**

Run: `bash scripts/hermes_test_pack/tests/test_variants.sh`

Expected: FAIL because `variants.sh` does not exist.

- [ ] **Step 3: Implement the explicit eight-variant mapping**

Create `variants.sh` with `resolve_smoke_variant()`. Parse the exact accepted form
`aarch64_(Android|Linux)_(SNPE|RKNN|TFLite)_<version>` and set:

```bash
platform_dir="aarch64_${system}"
smoke_suite="snpe-smoke|rknn-smoke|tflite-smoke"
smoke_binary="seg_crowd_test|ocr_test"
sdk_library="libdeepseg.so|libdeepocr.so"
config_src="models/seg/human/model.json|models/ocr/<engine>/v6/v6_small/model.json"
model_src="models/seg/human/snpe_<version>/sim.engine|models/ocr/<engine>/v6/v6_small/sim.engine"
input_src="models/seg/human/human.jpg|models/ocr/images/111.png"
```

Reject any engine/version pair outside the approved eight variants with a non-zero return.

- [ ] **Step 4: Run the mapping test**

Run: `bash scripts/hermes_test_pack/tests/test_variants.sh`

Expected: exit 0.

- [ ] **Step 5: Commit the mapping**

```bash
git add scripts/hermes_test_pack/lib/variants.sh scripts/hermes_test_pack/tests/test_variants.sh
git commit -m "build: define Hermes smoke variants"
```

### Task 5: Build minimal payloads and deterministic runners

**Files:**
- Create: `hermes_test_pack.sh`
- Create: `scripts/hermes_test_pack/lib/package.sh`
- Create: `scripts/hermes_test_pack/templates/run.sh.template`
- Create: `scripts/hermes_test_pack/tests/test_packager.py`

- [ ] **Step 1: Write fixture tests before the packager**

In `test_packager.py`, use `unittest`, `tempfile`, `tarfile` and `subprocess`. Build a temporary
repo fixture containing fake executable output, `libdeepocr.so`, two dependency `.so` files,
`model.json`, `sim.engine` and `111.png`. The tests must assert:

```python
self.assertEqual(result.returncode, 0)
self.assertEqual(payload_names, {
    "run.sh", "bin/ocr_test", "lib/libdeepocr.so", "lib/libdep.so",
    "models/ocr/sim.engine", "config/smoke.json", "testdata/111.png",
})
self.assertNotIn("include/public.hpp", payload_names)
self.assertTrue(mode("run.sh") & 0o100)
```

Add separate tests where the model is missing (non-zero), identical duplicate libraries are
deduplicated, and different-content duplicate libraries fail.

- [ ] **Step 2: Run tests and observe the missing CLI failure**

Run:

```bash
python3 -m unittest scripts.hermes_test_pack.tests.test_packager -v
```

Expected: FAIL because `hermes_test_pack.sh` does not exist.

- [ ] **Step 3: Implement package helpers**

`package.sh` must define:

```bash
copy_required SRC DST
copy_library_checked SRC LIB_DIR
copy_libraries_from_dir SRC_DIR LIB_DIR
rewrite_model_dir CONFIG DEST_MODEL_DIR
render_runner TEMPLATE OUT SUITE COMMAND
verify_payload_tree PAYLOAD_DIR
```

`copy_required` fails when the source is absent. `copy_library_checked` compares SHA-256 before
deduplicating a basename. `verify_payload_tree` rejects `include`, `example`, symlinks, and missing
runner/binary/config/model/input directories.

- [ ] **Step 4: Implement `hermes_test_pack.sh`**

Support only:

```text
hermes_test_pack.sh --platform VARIANT --output-dir DIR
```

The script sources `variants.sh` and `package.sh`, creates a temporary staging directory under the
output directory, copies the selected executable, SDK library, model, rewritten config and input,
collects jsoncpp/opencv/license plus the selected engine runtime libraries into `lib/`, preserves
SNPE DSP files under `lib/dsp/`, generates `run.sh`, validates the payload, then creates a single-
top-directory `.tar.gz` and matching `.sha256`.

- [ ] **Step 5: Implement runner result semantics**

The template must run the fixed command, capture its original exit code, and write:

```json
{
  "result_version": 1,
  "task_id": "agent-cli",
  "attempt": 1,
  "status": "COMPLETED",
  "exit_code": 0,
  "duration_sec": 0,
  "cases": {"total": 1, "passed": 1, "failed": 0, "skipped": 0, "failures": []}
}
```

Use `${HERMES_TASK_ID:-agent-cli}` and `${HERMES_ATTEMPT:-1}` in generated JSON; substitute the
actual exit code, integer duration and passed/failed counts. Return the original program exit code.

- [ ] **Step 6: Run all packager tests**

Run:

```bash
bash scripts/hermes_test_pack/tests/test_variants.sh
python3 -m unittest scripts.hermes_test_pack.tests.test_packager -v
```

Expected: all mapping, pruning, missing-file and collision tests pass.

- [ ] **Step 7: Commit the packager**

```bash
git add hermes_test_pack.sh scripts/hermes_test_pack
git commit -m "build: add minimal Hermes test packager"
```

### Task 6: Validate final contract-injected packages

**Files:**
- Create: `scripts/hermes_test_pack/verify_package.py`
- Create: `scripts/hermes_test_pack/tests/test_verify_package.py`

- [ ] **Step 1: Write verifier tests with valid and invalid tar fixtures**

Use `unittest` to create a final archive containing root `manifest.yaml`, root `files.sha256`, and
one payload top directory. Assert the valid fixture returns 0. Add fixtures that contain
`include/public.hpp`, omit `run.sh`, contain a symlink, or reference a missing config; each must
return non-zero.

- [ ] **Step 2: Run tests and observe the missing verifier failure**

Run: `python3 -m unittest scripts.hermes_test_pack.tests.test_verify_package -v`

Expected: FAIL because `verify_package.py` does not exist.

- [ ] **Step 3: Implement verifier checks**

The CLI must accept:

```text
verify_package.py --package FILE --manifest-schema FILE --result-schema FILE
```

Using only safe tar reads, PyYAML and jsonschema, verify root contracts, reject links/traversal,
validate Manifest, recompute every deploy digest, require payload `run.sh`, binary, config, model
and testdata, reject `include/` and `example/`, and validate a representative result document with
the vendored result Schema.

- [ ] **Step 4: Run verifier and full local tests**

Run:

```bash
python3 -m unittest discover -s scripts/hermes_test_pack/tests -v
python3 -m py_compile ci/hermes/*.py scripts/hermes_test_pack/verify_package.py
```

Expected: all tests pass and Python compilation exits 0.

- [ ] **Step 5: Commit the verifier**

```bash
git add scripts/hermes_test_pack/verify_package.py \
  scripts/hermes_test_pack/tests/test_verify_package.py
git commit -m "test: validate Hermes runtime packages"
```

### Task 7: Integrate the dual-package flow into GitLab CI

**Files:**
- Modify: `.gitlab-ci.yml`
- Create: `ci/hermes/requirements.txt`

- [ ] **Step 1: Add pinned Python tool dependencies**

Create `ci/hermes/requirements.txt`:

```text
PyYAML==6.0.2
jsonschema==4.25.1
```

Install these in `before_script` with the runner's Python environment, or fail with a clear message
if organizational policy requires preinstalled dependencies.

- [ ] **Step 2: Extend build jobs without changing SDK publication**

After `release_pack.sh`, add commands that generate the base Hermes package, call vendored
`gen_manifest.py`, call `verify_package.py`, generate `dist/meta/<variant>.json`, and save
`dist/meta/` as job artifacts. Use the existing strict package version and upload helper; upload the
final unique Hermes package only for master/tag, while MR runs stop after verification.

- [ ] **Step 3: Add the complete bundle stage**

Add `publish` to stages and a `publish:bundle` job with explicit `needs` for all eight build jobs.
Set `interruptible: false`. Run vendored `gen_bundle.py` against `dist/meta`, validate with the
vendored bundle schema, and upload only on master/tag. Missing meta or upload errors fail the job.

- [ ] **Step 4: Statistically validate CI structure**

Run:

```bash
python3 - <<'PY'
import yaml
d = yaml.safe_load(open('.gitlab-ci.yml', encoding='utf-8'))
assert 'publish' in d['stages']
j = d['publish:bundle']
assert j['interruptible'] is False
assert len(j['needs']) == 8
PY
rg -n 'hermes_test_pack|gen_manifest|verify_package|write_meta|gen_bundle|publish:bundle' .gitlab-ci.yml
```

Expected: YAML parses, bundle has eight needs, and every pipeline component is referenced.

- [ ] **Step 5: Commit CI integration**

```bash
git add .gitlab-ci.yml ci/hermes/requirements.txt
git commit -m "ci: publish Hermes test package bundles"
```

### Task 8: Final cross-repository verification and integration

**Files:**
- Verify all files changed by Tasks 1–7

- [ ] **Step 1: Verify Hermes from a clean feature worktree**

Run:

```bash
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest contracts/tests ci/tests -q -p no:cacheprovider
cd agent && "$HOME/.local/go/bin/go" test -count=1 ./...
cd ../runtime && "$HOME/.local/go/bin/go" test -count=1 ./...
```

Expected: all suites pass with zero failures.

- [ ] **Step 2: Verify Algo without staging pre-existing untracked files**

Run:

```bash
bash scripts/hermes_test_pack/tests/test_variants.sh
python3 -m unittest discover -s scripts/hermes_test_pack/tests -v
python3 -m py_compile ci/hermes/*.py scripts/hermes_test_pack/verify_package.py
git status --short
```

Expected: all tests pass; status contains no unintended model, build, dist, docs or tool files.

- [ ] **Step 3: Build static packages for available local build outputs**

For each variant whose `bin/<platform>` and `lib/<platform>` currently exist, run
`hermes_test_pack.sh`, inject contracts with vendored `gen_manifest.py`, then run
`verify_package.py`. If no complete build output exists, record this as an environment-dependent
verification not executed; do not claim real binaries passed.

- [ ] **Step 4: Merge Hermes locally and push**

Merge the verified Hermes feature branch into `master`, rerun Hermes tests on the merged tree,
push, and verify local/remote hashes match.

- [ ] **Step 5: Confirm Algo snapshot revision before merge**

Run:

```bash
rev="$(cat ci/hermes/REVISION)"
git -C /home/maxin/Code/hermes_ai_devops cat-file -e "${rev}^{commit}"
cmp ci/hermes/gen_manifest.py \
  <(git -C /home/maxin/Code/hermes_ai_devops show "${rev}:ci/gen_manifest.py")
cmp ci/hermes/contracts/manifest.schema.json \
  <(git -C /home/maxin/Code/hermes_ai_devops show "${rev}:contracts/manifest.schema.json")
```

If Hermes gained a later merge commit, `REVISION` intentionally remains the source feature commit
whose bytes were copied; the `git show` comparisons prove that relationship without relying on the
current Hermes `HEAD`.

- [ ] **Step 6: Merge Algo locally and push**

Merge the verified Algo feature branch into `master`, rerun Algo unit/static tests on the merged
tree, push to its GitLab origin, and verify local/remote hashes match. Preserve all pre-existing
untracked files in the original Algo worktree.

- [ ] **Step 7: Report deferred device evidence accurately**

Report static/package results separately from device results. QCM6125 SNPE/TFLite and future RKNN
device runs remain pending until executed with native Windows `agent-cli.exe`; do not mark those
acceptance rows complete based on unit tests.
