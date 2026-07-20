# Hermes Deterministic Registry Publication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Hermes packages and bundles byte-stable across GitLab job retries and uniquely address bundles from different pipelines of the same commit on GitLab 13.8.

**Architecture:** Hermes first extends the bundle contract with the GitLab instance-global pipeline ID, makes its Python archive and bundle writers deterministic, and teaches Trigger to fetch by commit plus global pipeline ID. Algo then vendors that exact Hermes snapshot, makes its base payload archive deterministic, and passes GitLab 13.8-compatible identifiers and timestamps through CI.

**Tech Stack:** Bash, Python 3.9+, pytest, unittest, PyYAML, jsonschema, Go, GitLab CI YAML, GNU tar/gzip

---

## Repository and file boundaries

- Hermes worktree: `/home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging`
- Algo worktree: `/home/maxin/.config/superpowers/worktrees/Algo_Super_SDK/hermes-test-packaging`
- Hermes remains authoritative for `ci/gen_manifest.py`, `ci/write_meta.py`,
  `ci/gen_bundle.py`, and `contracts/bundle.schema.json`.
- Algo owns `hermes_test_pack.sh`, `.gitlab-ci.yml`, and Algo-only tests.
- Do not stage generated `__pycache__/` directories or pre-existing files from the
  original Algo worktree.
- GitLab 13.8 provides `CI_COMMIT_TIMESTAMP`, `CI_PIPELINE_ID`, and
  `CI_PIPELINE_IID`; its Pipeline Hook provides global `object_attributes.id` but
  not the IID.

### Task 1: Add the global pipeline ID to Hermes meta and bundle contracts

**Files:**
- Modify: `ci/tests/test_write_meta.py`
- Modify: `ci/write_meta.py`
- Modify: `contracts/tests/test_bundle_schema.py`
- Modify: `contracts/bundle.schema.json`
- Modify: `runtime/internal/trigger/bundle.schema.json`
- Modify: `runtime/internal/trigger/bundle.go`
- Test: `ci/tests/test_write_meta.py`
- Test: `contracts/tests/test_bundle_schema.py`
- Test: `runtime/internal/trigger/bundle_test.go`

- [ ] **Step 1: Write failing meta and schema tests**

Update the valid bundle fixtures to contain `"pipeline_global_id": 42001`. Add this
assertion to the write-meta test after invoking `write_meta` with
`pipeline_global_id=42001`:

```python
assert meta["pipeline_id"] == 42
assert meta["pipeline_global_id"] == 42001
```

Add a negative contract test:

```python
def test_bundle_requires_global_pipeline_id(validators, valid_case):
    bundle = load_example(valid_case)
    bundle.pop("pipeline_global_id")
    with pytest.raises(jsonschema.ValidationError):
        validators["bundle"].validate(bundle)
```

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  ci/tests/test_write_meta.py contracts/tests/test_bundle_schema.py -q
cd runtime && "$HOME/.local/go/bin/go" test ./internal/trigger
```

Expected: Python fails because `write_meta` has no `pipeline_global_id` argument and
the schema does not require it; Go fails after the embedded fixture is updated but
the struct/schema are not.

- [ ] **Step 3: Implement the contract field and writer argument**

In `ci/write_meta.py`, extend the function and CLI:

```python
def write_meta(*, info_file, variant, project, version, commit,
               pipeline_iid, pipeline_global_id, package_name,
               registry_project_url, outdir) -> Path:
```

Add `"pipeline_global_id": pipeline_global_id` immediately after the existing
`"pipeline_id": pipeline_iid` entry in `meta`. Add the CLI option:

```python
parser.add_argument("--pipeline-global-id", required=True, type=int,
                    help="CI_PIPELINE_ID")
```

Pass `args.pipeline_global_id` into `write_meta`. In both bundle schema copies, add
`pipeline_global_id` to `required` and add:

```json
"pipeline_global_id": {
  "type": "integer",
  "minimum": 1,
  "description": "GitLab instance-global CI_PIPELINE_ID; used by Pipeline Hook lookup"
}
```

In `runtime/internal/trigger/bundle.go`, add:

```go
PipelineGlobalID int `json:"pipeline_global_id"`
```

- [ ] **Step 4: Run focused tests and verify GREEN**

Run the commands from Step 2. Expected: all selected Python and Go tests pass.

- [ ] **Step 5: Commit the contract extension**

```bash
git add ci/write_meta.py ci/tests/test_write_meta.py \
  contracts/bundle.schema.json contracts/tests/test_bundle_schema.py \
  runtime/internal/trigger/bundle.schema.json runtime/internal/trigger/bundle.go \
  runtime/internal/trigger/bundle_test.go
git commit -m "feat(contracts): record global pipeline identity"
```

### Task 2: Make Hermes final package generation deterministic and streaming

**Files:**
- Modify: `ci/tests/test_gen_manifest.py`
- Modify: `ci/gen_manifest.py`
- Test: `ci/tests/test_gen_manifest.py`

- [ ] **Step 1: Add failing determinism and streaming tests**

Add a test that invokes `inject_manifest` twice into separate directories with the
same `source_date_epoch=1_700_000_000`:

```python
import hashlib
import io
from pathlib import Path

def test_repeated_injection_is_byte_identical(fake_package, tmp_path):
    package = fake_package()
    common = dict(
        package=package,
        variant=VARIANT,
        variants_file=VARIANTS_FILE,
        schema_file=MANIFEST_SCHEMA,
        project="algo-super-sdk",
        commit="deadbee1",
        pipeline_iid=42,
        build_type="Release",
        package_name="algo-super-sdk",
        source_date_epoch=1_700_000_000,
    )
    first = gen_manifest.inject_manifest(outdir=tmp_path / "one", **common)
    second = gen_manifest.inject_manifest(outdir=tmp_path / "two", **common)
assert Path(first["package_path"]).read_bytes() == Path(second["package_path"]).read_bytes()
assert first["package_sha256"] == second["package_sha256"]
```

Add a unit test for a new `_hash_member` helper using a reader that raises if called
with `size < 0` or more than `1024 * 1024` bytes:

```python
class BoundedReader(io.BytesIO):
    def read(self, size=-1):
        assert 0 <= size <= 1024 * 1024
        return super().read(size)

assert gen_manifest._hash_stream(BoundedReader(b"x" * (2 * 1024 * 1024))) == \
    hashlib.sha256(b"x" * (2 * 1024 * 1024)).hexdigest()
```

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  ci/tests/test_gen_manifest.py -q
```

Expected: FAIL because `source_date_epoch` and `_hash_stream` do not exist and the
current gzip/tar output is timestamp-dependent.

- [ ] **Step 3: Implement deterministic archive helpers**

Add imports and helpers to `ci/gen_manifest.py`:

```python
import copy
import gzip
from contextlib import contextmanager

HASH_CHUNK_SIZE = 1024 * 1024

def _hash_stream(stream):
    digest = hashlib.sha256()
    while True:
        chunk = stream.read(HASH_CHUNK_SIZE)
        if not chunk:
            return digest.hexdigest()
        digest.update(chunk)

def _normalized_member(member, epoch):
    normalized = copy.copy(member)
    normalized.uid = 0
    normalized.gid = 0
    normalized.uname = ""
    normalized.gname = ""
    normalized.mtime = epoch
    normalized.pax_headers = {}
    return normalized

@contextmanager
def _deterministic_tar_writer(path, epoch):
    with Path(path).open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch) as zipped:
            with tarfile.open(fileobj=zipped, mode="w", format=tarfile.GNU_FORMAT) as archive:
                yield archive
```

Use `_hash_stream(tar.extractfile(m))` in `_scan_package`. Add the required
`source_date_epoch` keyword argument to `inject_manifest` and update every existing
test call with the fixed value `1_700_000_000`. Write members in sorted name order.
For copied and injected members, set normalized owner/group/mtime metadata. Close
the tar, gzip, and raw streams in nested `with` blocks or a dedicated contextmanager
so partial files never masquerade as valid output.

Add CLI parsing:

```python
parser.add_argument("--source-date-epoch", required=True, type=int)
```

Reject negative epochs and pass the value to `inject_manifest`.

- [ ] **Step 4: Run focused tests and verify GREEN**

Run the Step 2 command twice. Expected: all tests pass both times and repeated
archive bytes remain identical.

- [ ] **Step 5: Commit deterministic final archives**

```bash
git add ci/gen_manifest.py ci/tests/test_gen_manifest.py
git commit -m "fix(ci): make manifest packages reproducible"
```

### Task 3: Make Hermes bundles stable and globally pipeline-qualified

**Files:**
- Modify: `ci/tests/test_gen_bundle.py`
- Modify: `ci/gen_bundle.py`
- Modify: `ci/README.md`
- Test: `ci/tests/test_gen_bundle.py`

- [ ] **Step 1: Write failing bundle identity tests**

Extend `make_metas` with `pipeline_global_id=42001` and write it into every meta.
Change the valid bundle test to call:

```python
out = gen_bundle.gen_bundle(
    meta_dir=meta_dir,
    variants_file=VARIANTS_FILE,
    schema_file=BUNDLE_SCHEMA,
    outdir=tmp_path,
    created_at="2026-07-17T08:00:00Z",
)
assert out.name == "bundle-gdeadbee1-p42001.json"
```

Add:

```python
def test_retry_is_byte_identical(tmp_path):
    meta_dir = tmp_path / "meta"
    make_metas(meta_dir, all_variants(), pipeline_global_id=42001)
    kwargs = dict(meta_dir=meta_dir, variants_file=VARIANTS_FILE,
                  schema_file=BUNDLE_SCHEMA,
                  created_at="2026-07-17T08:00:00Z")
    one = gen_bundle.gen_bundle(outdir=tmp_path / "one", **kwargs)
    two = gen_bundle.gen_bundle(outdir=tmp_path / "two", **kwargs)
    assert one.read_bytes() == two.read_bytes()

def test_same_commit_new_pipeline_gets_new_name(tmp_path):
    first_meta = tmp_path / "first-meta"
    second_meta = tmp_path / "second-meta"
    make_metas(first_meta, all_variants(), pipeline_global_id=42001)
    make_metas(second_meta, all_variants(), pipeline_global_id=42002)
    common = dict(variants_file=VARIANTS_FILE, schema_file=BUNDLE_SCHEMA,
                  created_at="2026-07-17T08:00:00Z")
    first = gen_bundle.gen_bundle(meta_dir=first_meta,
                                  outdir=tmp_path / "one", **common)
    second = gen_bundle.gen_bundle(meta_dir=second_meta,
                                   outdir=tmp_path / "two", **common)
    assert first.name == "bundle-gdeadbee1-p42001.json"
    assert second.name == "bundle-gdeadbee1-p42002.json"
```

- [ ] **Step 2: Run focused tests and verify RED**

```bash
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  ci/tests/test_gen_bundle.py -q
```

Expected: FAIL because the generator reads the wall clock, ignores the global ID,
and produces `bundle-gdeadbee1.json`.

- [ ] **Step 3: Implement stable bundle generation**

Change:

```python
SHARED_FIELDS = (
    "project", "commit", "pipeline_id", "pipeline_global_id", "version",
)

def gen_bundle(*, meta_dir, variants_file, schema_file, outdir, created_at):
    bundle = {
        "bundle_version": 1,
        **shared,
        "created_at": created_at,
        "packages": [
            {key: metas[variant][key] for key in PACKAGE_FIELDS}
            for variant in variants
        ],
    }
    out = outdir / (
        f"bundle-g{shared['commit']}-p{shared['pipeline_global_id']}.json"
    )
```

Validate and normalize `created_at` with:

```python
def _normalize_created_at(value):
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise SystemExit(f"invalid created_at: {value!r}") from exc
    if parsed.tzinfo is None:
        raise SystemExit("created_at must include a timezone")
    return parsed.astimezone(timezone.utc).isoformat(
        timespec="milliseconds"
    ).replace("+00:00", "Z")
```

Use `_normalize_created_at(created_at)` in the bundle and add:

```python
parser.add_argument("--created-at", required=True,
                    help="stable RFC3339 timestamp; CI uses CI_COMMIT_TIMESTAMP")
```

Update `ci/README.md` examples and names to `bundle-g{sha}-p{global_id}.json`.

- [ ] **Step 4: Run focused tests and verify GREEN**

Run the Step 2 command. Expected: all bundle tests pass.

- [ ] **Step 5: Commit the bundle identity change**

```bash
git add ci/gen_bundle.py ci/tests/test_gen_bundle.py ci/README.md
git commit -m "fix(ci): qualify reproducible bundles by pipeline"
```

### Task 4: Make Trigger fetch and validate the exact GitLab 13.8 pipeline

**Files:**
- Modify: `runtime/internal/trigger/gitlab.go`
- Modify: `runtime/internal/trigger/gitlab_test.go`
- Modify: `runtime/internal/trigger/handler.go`
- Modify: `runtime/internal/trigger/handler_test.go`
- Modify: `runtime/README.md`
- Test: `runtime/internal/trigger`

- [ ] **Step 1: Write failing GitLab lookup and handler tests**

Change the GitLab client test fixture path to:

```go
"/api/v4/projects/7/packages/generic/algo-super-sdk/1.2.3/" +
    "bundle-gabcd1234-p42001.json": bundle,
```

Call:

```go
raw, found, err := gl.FetchBundle(context.Background(), 7, "abcd1234", 42001)
```

Extend `fakeFetcher` with `gotPipelineGlobalID int`. Update the handler payload
fixture so `object_attributes.id` is `42001`, and assert:

```go
if fetcher.gotPipelineGlobalID != 42001 {
    t.Errorf("global pipeline id = %d", fetcher.gotPipelineGlobalID)
}
```

Add a handler test whose event ID is `42002` but parsed bundle
`pipeline_global_id` is `42001`; expect HTTP 422 and no artifact registration or
workflow start.

- [ ] **Step 2: Run focused Go tests and verify RED**

```bash
cd runtime && "$HOME/.local/go/bin/go" test -count=1 ./internal/trigger
```

Expected: compile/test failure because `FetchBundle` does not accept the global ID
and Handler does not validate it.

- [ ] **Step 3: Implement exact lookup and validation**

Change the interface and client signature:

```go
FetchBundle(ctx context.Context, projectID int, shortSHA string,
    pipelineGlobalID int) (raw []byte, found bool, err error)
```

Build the exact filename:

```go
fileName := fmt.Sprintf("bundle-g%s-p%d.json", shortSHA, pipelineGlobalID)
```

In `Handler.ServeHTTP`, pass `ev.ObjectAttributes.ID` to the fetcher. After parsing
the bundle and before registering artifacts, enforce:

```go
if b.PipelineGlobalID != ev.ObjectAttributes.ID {
    http.Error(w, "bundle pipeline mismatch", http.StatusUnprocessableEntity)
    return
}
```

Update `runtime/README.md` to document the qualified name and GitLab 13.8 global-ID
source.

- [ ] **Step 4: Run focused and complete Runtime tests**

```bash
cd runtime
"$HOME/.local/go/bin/go" test -count=1 ./internal/trigger
"$HOME/.local/go/bin/go" test -count=1 ./...
```

Expected: both commands pass.

- [ ] **Step 5: Commit Trigger compatibility**

```bash
git add runtime/internal/trigger/gitlab.go runtime/internal/trigger/gitlab_test.go \
  runtime/internal/trigger/handler.go runtime/internal/trigger/handler_test.go \
  runtime/README.md
git commit -m "fix(runtime): fetch pipeline-qualified bundles"
```

### Task 5: Verify and record the authoritative Hermes snapshot

**Files:**
- Verify all Hermes files changed by Tasks 1–4

- [ ] **Step 1: Run complete Hermes verification**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  contracts/tests ci/tests -q -p no:cacheprovider
cd agent && "$HOME/.local/go/bin/go" test -count=1 ./...
cd ../runtime && "$HOME/.local/go/bin/go" test -count=1 ./...
```

Expected: all Python, Agent Go, and Runtime Go suites pass with zero failures.

- [ ] **Step 2: Confirm schema copies and clean tracked state**

```bash
cmp contracts/bundle.schema.json runtime/internal/trigger/bundle.schema.json
git diff --check
git status --short
```

Expected: schema comparison and diff check exit 0; status contains no unintended
tracked or untracked files.

- [ ] **Step 3: Record the exact snapshot revision**

```bash
git rev-parse HEAD
```

Save the full output as `HERMES_SNAPSHOT_REV`. Do not use a later documentation or
merge commit when updating Algo.

### Task 6: Make the Algo base payload archive deterministic

**Files:**
- Modify: `scripts/hermes_test_pack/tests/test_packager.py`
- Modify: `hermes_test_pack.sh`
- Test: `scripts/hermes_test_pack/tests/test_packager.py`

- [ ] **Step 1: Write a failing repeated-package test**

Add:

```python
def test_identical_inputs_and_epoch_produce_identical_archive(self):
    env = os.environ.copy()
    env["SOURCE_DATE_EPOCH"] = "1700000000"
    self._package(env=env)
    first = self._archive().read_bytes()
    first_digest = hashlib.sha256(first).hexdigest()
    time.sleep(1.1)
    self._package(env=env)
    second = self._archive().read_bytes()
    self.assertEqual(first, second)
    self.assertEqual(first_digest, hashlib.sha256(second).hexdigest())
```

Import `time`. Add a negative test setting `SOURCE_DATE_EPOCH=invalid` and assert a
non-zero exit plus `SOURCE_DATE_EPOCH must be a non-negative integer` in stderr.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
cd /home/maxin/.config/superpowers/worktrees/Algo_Super_SDK/hermes-test-packaging
python3 -m unittest \
  scripts.hermes_test_pack.tests.test_packager.PackagerTest.test_identical_inputs_and_epoch_produce_identical_archive \
  scripts.hermes_test_pack.tests.test_packager.PackagerTest.test_invalid_source_date_epoch_is_rejected -v
```

Expected: determinism assertion fails and invalid epoch is currently accepted.

- [ ] **Step 3: Implement normalized tar and gzip output**

Before staging in `hermes_test_pack.sh`, require:

```bash
source_date_epoch="${SOURCE_DATE_EPOCH:-}"
if [[ ! "${source_date_epoch}" =~ ^[0-9]+$ ]]; then
    echo "ERROR: SOURCE_DATE_EPOCH must be a non-negative integer" >&2
    exit 1
fi
```

Replace the final `tar -czf` with atomic deterministic output:

```bash
archive="${output_dir}/${payload_name}.tar.gz"
archive_tmp="${work_dir}/${payload_name}.tar.gz.tmp"
tar -C "${work_dir}" \
    --sort=name \
    --format=gnu \
    --mtime="@${source_date_epoch}" \
    --owner=0 --group=0 --numeric-owner \
    -cf - "${payload_name}" |
    gzip -n >"${archive_tmp}"
mv -- "${archive_tmp}" "${archive}"
```

- [ ] **Step 4: Run focused and full Algo packaging tests**

```bash
python3 -m unittest \
  scripts.hermes_test_pack.tests.test_packager.PackagerTest.test_identical_inputs_and_epoch_produce_identical_archive \
  scripts.hermes_test_pack.tests.test_packager.PackagerTest.test_invalid_source_date_epoch_is_rejected -v
python3 -m unittest scripts.hermes_test_pack.tests.test_packager -v
```

Expected: both commands pass.

- [ ] **Step 5: Commit deterministic base packaging**

```bash
git add hermes_test_pack.sh scripts/hermes_test_pack/tests/test_packager.py
git commit -m "fix(build): make Hermes payload archives reproducible"
```

### Task 7: Vendor the fixed Hermes snapshot and wire GitLab 13.8 CI

**Files:**
- Modify: `ci/hermes/REVISION`
- Modify: `ci/hermes/README.md`
- Modify: `ci/hermes/gen_manifest.py`
- Modify: `ci/hermes/write_meta.py`
- Modify: `ci/hermes/gen_bundle.py`
- Modify: `ci/hermes/contracts/bundle.schema.json`
- Modify: `.gitlab-ci.yml`
- Create: `scripts/hermes_test_pack/tests/test_ci_wiring.py`
- Test: `scripts/hermes_test_pack/tests/test_ci_wiring.py`

- [ ] **Step 1: Write failing static CI tests**

Create `test_ci_wiring.py` with `unittest` and `yaml.safe_load`. Read
`.gitlab-ci.yml` as both text and parsed YAML, then assert:

```python
self.assertIn('SOURCE_DATE_EPOCH=', self.ci_text)
self.assertIn('CI_COMMIT_TIMESTAMP', self.ci_text)
self.assertIn('--source-date-epoch "${SOURCE_DATE_EPOCH}"', self.ci_text)
self.assertIn('--pipeline-global-id "${CI_PIPELINE_ID}"', self.ci_text)
self.assertIn('--created-at "${CI_COMMIT_TIMESTAMP}"', self.ci_text)
self.assertIn('bundle-g${CI_COMMIT_SHORT_SHA}-p${CI_PIPELINE_ID}.json', self.ci_text)
publish = self.ci["publish:bundle"]
self.assertFalse(publish["interruptible"])
self.assertEqual(8, len(publish["needs"]))
```

- [ ] **Step 2: Run the static test and verify RED**

```bash
python3 -m unittest scripts.hermes_test_pack.tests.test_ci_wiring -v
```

Expected: FAIL because the current CI passes neither reproducible timestamps nor
the global pipeline ID and still expects the legacy bundle name.

- [ ] **Step 3: Copy the authoritative Hermes snapshot**

From the Algo worktree, copy exact bytes:

```bash
cp /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging/ci/gen_manifest.py ci/hermes/
cp /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging/ci/write_meta.py ci/hermes/
cp /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging/ci/gen_bundle.py ci/hermes/
cp /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging/contracts/bundle.schema.json \
  ci/hermes/contracts/bundle.schema.json
git -C /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging rev-parse HEAD \
  > ci/hermes/REVISION
```

Update `ci/hermes/README.md` to document deterministic archives, the global
pipeline-qualified bundle name, and the GitLab 13.8 variable sources.

- [ ] **Step 4: Wire deterministic values through CI**

At the start of each build script block, derive and export:

```bash
[[ -n "${CI_COMMIT_TIMESTAMP:-}" ]] || {
  echo "ERROR: CI_COMMIT_TIMESTAMP is required"
  exit 1
}
SOURCE_DATE_EPOCH="$(date -u -d "${CI_COMMIT_TIMESTAMP}" +%s)"
export SOURCE_DATE_EPOCH
```

Pass these arguments:

```text
gen_manifest.py --source-date-epoch "${SOURCE_DATE_EPOCH}"
write_meta.py --pipeline-global-id "${CI_PIPELINE_ID}"
gen_bundle.py --created-at "${CI_COMMIT_TIMESTAMP}"
```

Change both bundle path references to:

```bash
bundle_file="dist/bundle-g${CI_COMMIT_SHORT_SHA}-p${CI_PIPELINE_ID}.json"
```

Change the bundle artifact glob to `dist/bundle-g*-p*.json`. Do not change strict
package version `X.Y.Z`, MR upload skipping, eight-job `needs`, or collision byte
verification.

- [ ] **Step 5: Run snapshot, static CI, and vendored tool tests**

```bash
test "$(cat ci/hermes/REVISION)" = \
  "$(git -C /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging rev-parse HEAD)"
cmp ci/hermes/gen_manifest.py \
  /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging/ci/gen_manifest.py
cmp ci/hermes/write_meta.py \
  /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging/ci/write_meta.py
cmp ci/hermes/gen_bundle.py \
  /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging/ci/gen_bundle.py
cmp ci/hermes/contracts/bundle.schema.json \
  /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging/contracts/bundle.schema.json
python3 -m unittest scripts.hermes_test_pack.tests.test_ci_wiring -v
python3 -m py_compile ci/hermes/*.py scripts/hermes_test_pack/verify_package.py
```

Expected: every command exits 0.

- [ ] **Step 6: Commit the snapshot and CI wiring**

```bash
git add .gitlab-ci.yml ci/hermes \
  scripts/hermes_test_pack/tests/test_ci_wiring.py
git commit -m "fix(ci): publish reproducible pipeline bundles"
```

### Task 8: Final cross-repository verification, review, and push

**Files:**
- Verify all files changed by Tasks 1–7

- [ ] **Step 1: Run the complete Hermes matrix**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  contracts/tests ci/tests -q -p no:cacheprovider
cd agent && "$HOME/.local/go/bin/go" test -count=1 ./...
cd ../runtime && "$HOME/.local/go/bin/go" test -count=1 ./...
```

Expected: every suite passes with zero failures.

- [ ] **Step 2: Run the complete Algo matrix**

```bash
cd /home/maxin/.config/superpowers/worktrees/Algo_Super_SDK/hermes-test-packaging
bash scripts/hermes_test_pack/tests/test_variants.sh
python3 -m unittest discover -s scripts/hermes_test_pack/tests -v
python3 -m py_compile ci/hermes/*.py scripts/hermes_test_pack/verify_package.py
git diff --check
```

Expected: mapping, all unittest cases, compilation, and diff checks pass.

- [ ] **Step 3: Reproduce the original two failures directly**

Run the new determinism tests twice and the same-commit/different-pipeline bundle
test once. Expected: identical same-pipeline bytes and distinct global-ID-qualified
filenames, with all tests passing.

- [ ] **Step 4: Request independent code review**

Review each repository from its original feature base through current `HEAD`.
Resolve every Critical and Important finding before proceeding; re-run the relevant
focused test after each correction and the complete matrices afterward.

- [ ] **Step 5: Push Hermes first**

```bash
git -C /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging \
  push -u origin feature/algo-hermes-packaging
```

Verify the remote branch resolves to the local `HEAD` before continuing.

- [ ] **Step 6: Reconfirm Algo snapshot provenance and push Algo**

```bash
rev="$(cat ci/hermes/REVISION)"
git -C /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging \
  cat-file -e "${rev}^{commit}"
cmp ci/hermes/gen_bundle.py \
  <(git -C /home/maxin/Code/hermes_ai_devops/.worktrees/algo-hermes-packaging \
      show "${rev}:ci/gen_bundle.py")
git push origin feature/hermes-test-packaging
```

Expected: provenance checks exit 0 and the remote Algo feature branch advances to
the verified local `HEAD`.

- [ ] **Step 7: Hand off GitLab and device evidence without overclaiming**

Create or refresh the Algo MR. Treat MR pipeline success, master Registry upload,
bundle retry behavior, and native Windows device execution as separate evidence
gates. Local verification does not prove any of those external gates.
