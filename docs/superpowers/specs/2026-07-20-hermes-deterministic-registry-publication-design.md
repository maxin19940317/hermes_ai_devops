# Hermes Deterministic Registry Publication Design

## Goal

Make Algo Hermes package and bundle publication safe under both GitLab job retries
and new pipelines for the same commit. Repeated packaging of identical inputs must
produce byte-identical archives, while distinct pipelines must publish distinct,
deterministically addressable bundles.

## Scope

This change spans the authoritative Hermes repository and the independent
`Algo_Super_SDK` repository.

- Hermes owns the bundle naming contract, bundle generator behavior, Trigger
  lookup behavior, schemas, and their tests.
- Algo vendors a pinned Hermes snapshot and owns the payload packager and GitLab
  pipeline integration.
- Device execution behavior, package payload contents, release package versioning,
  and the eight-variant matrix remain unchanged.

## Root Causes

The current base and final package writers inherit filesystem timestamps and gzip
header timestamps. A retry therefore generates different bytes at the same package
filename, and collision-safe Registry upload correctly rejects the mismatch.

The current bundle name, `bundle-g<commit>.json`, identifies only a commit. Its
content also includes `pipeline_id`, pipeline-specific package URLs, and a freshly
generated `created_at`. A job retry can change `created_at`, and a new pipeline for
the same commit necessarily changes the other pipeline-specific fields. Both cases
attempt to publish different bytes at the same Registry path.

## Chosen Design

### Deterministic package archives

Every archive writer receives a reproducible epoch through `SOURCE_DATE_EPOCH`.
GitLab CI derives it from `CI_COMMIT_TIMESTAMP`; local callers may set it explicitly,
and deterministic tests use a fixed fixture value.

The base payload archive is written in sorted pathname order with normalized owner,
group, numeric IDs, and member modification time. Its gzip layer omits the original
filename and uses a fixed timestamp. File contents and executable modes remain
meaningful and unchanged.

The contract-injected final archive applies the same normalization while retaining
the modes required by the Manifest and verifier. Hashing archive members streams
data incrementally rather than loading an allowed large member fully into memory.

Running either writer twice with identical inputs and the same epoch must produce
identical bytes and SHA-256 digests.

### Pipeline-qualified bundle identity

The canonical bundle filename becomes:

```text
bundle-g<CI_COMMIT_SHORT_SHA>-p<CI_PIPELINE_ID>.json
```

GitLab 13.8 Pipeline Hook exposes the instance-global pipeline `id`, but not the
project-local IID. Meta and bundle documents therefore add `pipeline_global_id`
while retaining the existing `pipeline_id` IID for compatibility. `created_at` is
no longer read from the wall clock inside the generator; callers provide the stable
RFC 3339 `CI_COMMIT_TIMESTAMP`, which is also the source for
`SOURCE_DATE_EPOCH`. Re-running a job in one pipeline thus recreates identical
bundle bytes. Running a new pipeline for the same commit uses a different global ID
in the filename and cannot collide.

The Generic Package version remains the strict CMake version, such as `1.0.2`.
Pipeline identity stays in filenames and does not alter release-version semantics.

### Trigger lookup

GitLab 13.8 pipeline webhooks provide the commit SHA and instance-global pipeline
ID. Trigger uses both values to request the exact pipeline-qualified filename. It
continues probing package versions because the webhook does not contain the CMake
package version. After parsing, Trigger also requires the bundle's
`pipeline_global_id` to equal the webhook `object_attributes.id`.

Trigger must not fall back silently to the legacy commit-only name: doing so could
dispatch an artifact produced by a different pipeline. A successful webhook with no
matching qualified bundle remains an idempotent skip, consistent with current MR
behavior.

### Snapshot ownership

Hermes changes are implemented, tested, committed, and pushed first. Algo then
copies the approved Hermes generator, schema, variant configuration, and supporting
files byte-for-byte and records that exact Hermes commit in `ci/hermes/REVISION`.
Snapshot comparison tests continue to prove provenance.

## Data Flow

```text
CI_COMMIT_TIMESTAMP
  -> SOURCE_DATE_EPOCH
  -> deterministic base payload archive
  -> deterministic contract-injected archive
  -> stable per-variant meta

CI_COMMIT_TIMESTAMP + CI_PIPELINE_ID + eight meta files
  -> bundle-g<sha>-p<global-id>.json
  -> collision-safe Registry upload
  -> Trigger lookup by commit + global pipeline ID
  -> exact DeviceTestWorkflow input
```

## Failure Handling

- Missing or invalid reproducible timestamps fail before publication with a clear
  error; writers do not fall back to the current clock in CI.
- Existing Registry objects are accepted only when their size and SHA-256 match the
  locally regenerated deterministic file.
- A same-pipeline retry that produces different bytes is treated as a regression and
  fails publication.
- A webhook cannot consume a bundle whose filename, commit, or global pipeline ID
  differs from the event.
- Partial eight-variant builds still cannot publish a bundle.

## Compatibility and Migration

Existing `bundle-g<sha>.json` objects remain immutable historical artifacts but are
not selected by the updated Trigger. No Registry deletion or overwrite is required.
Package and bundle schemas retain their v1 document fields; the change affects file
identity and generator inputs, not the JSON object shape.

The Trigger and Algo CI changes must be deployed together. Until the updated Trigger
is deployed, newly qualified bundles may be inspected and passed manually to
`agent-cli`, but automated webhook consumption must not be claimed.

## Test Strategy

Hermes tests cover:

- two bundle generations with identical inputs and timestamps are byte-identical;
- a new global pipeline ID produces a different qualified filename;
- Trigger requests the exact commit-and-global-pipeline-qualified bundle;
- legacy or mismatched bundle identities are not consumed;
- schema snapshots remain synchronized.

Algo tests cover:

- two base payload builds with identical fixtures and epoch are byte-identical;
- two final contract-injected builds are byte-identical;
- large-member hashing is streaming;
- CI passes the stable commit timestamp and global pipeline ID into all generators;
- Registry retry verification accepts identical regenerated artifacts;
- same-commit, different-pipeline bundles have distinct names.

After focused red-green cycles, run the complete Hermes Python and Go suites, the
complete Algo packaging suite, Python compilation checks, CI YAML static checks, and
snapshot byte comparisons. Real GitLab and Windows-device evidence remains a
separate acceptance gate.

## Acceptance Criteria

1. Identical package inputs plus the same reproducible epoch yield identical archive
   SHA-256 values across repeated invocations.
2. A retried bundle job in one pipeline yields identical bundle bytes and filename.
3. Two pipelines for one commit yield distinct global-ID-qualified bundle filenames.
4. Trigger derives and fetches the exact qualified name from the webhook.
5. Existing collision verification remains strict and succeeds for legitimate job
   retries.
6. All local suites pass without claiming GitLab Registry or device success.
