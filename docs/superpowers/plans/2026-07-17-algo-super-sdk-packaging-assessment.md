# Algo_Super_SDK Packaging Assessment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an evidence-backed Algo_Super_SDK packaging compatibility assessment and make its P0 closure a Phase 1 dispatch gate.

**Architecture:** Keep the detailed, reproducible assessment in a dedicated document under `docs/assessments/`. Add only a concise mandatory gate and link to `CLAUDE.md`, preserving that file as the authoritative roadmap without duplicating the full findings. This change is documentation-only and must not modify the Algo_Super_SDK working tree.

**Tech Stack:** Markdown, Git, existing Hermes contracts and CI documentation

---

### Task 1: Add the packaging compatibility assessment

**Files:**
- Create: `docs/assessments/algo-super-sdk-packaging.md`
- Reference: `docs/superpowers/specs/2026-07-17-algo-super-sdk-packaging-assessment-design.md`
- Reference: `contracts/manifest.schema.json`
- Reference: `ci/variants.yaml`
- Reference: `ci/gitlab-ci.example.yml`

- [ ] **Step 1: Create the assessment directory and report**

Create `docs/assessments/algo-super-sdk-packaging.md` with these exact top-level sections:

```markdown
# Algo_Super_SDK 打包适配评估

## 结论
## 评估范围与基线
## 已验证事实
## 规则对照
## P0 整改项
## P1 整改项
## 推荐包边界
## 验收矩阵
## 非目标
## 复核命令
```

The report must record the following immutable evidence:

```text
target commit: 57d3ca01eca9b3860ae3c861a66b282b16261525
sample: Algo_Super_SDK_v1.0.2_aarch64_Android_TFLite_2.21.0.tar.gz
sample sha256: 73cf87bce40381c0cb50bdf5e2572ea9ff4fb32a22df2b95a006e7400fde91d7
regular files: 455
uncompressed bytes: 98367108
```

State the conclusion in two independent dimensions:

```text
SDK 发布完整性：通过当前 release_pack.sh 自校验和样包 SHA-256 校验。
Hermes 测试包适配性：不通过，禁止直接进入自动设备测试派发。
```

Add a rule comparison table covering at least: unique package naming, `manifest.yaml`,
`files.sha256`, executable entry, structured result, runtime library path, deployment file
set, per-variant meta, eight-variant bundle completeness, and device capability selection.
Each row must contain the current observation, Hermes expectation, verdict, and remediation ID.

Record all seven P0 items from the approved design as unchecked Markdown checkboxes with stable
IDs `P0-1` through `P0-7`. Record all five P1 items as `P1-1` through `P1-5`.

- [ ] **Step 2: Add reproducible evidence commands**

Include commands that do not mutate the target repository:

```bash
git -C /home/maxin/Code/560D/Algo_Super_SDK rev-parse HEAD
git -C /home/maxin/Code/560D/Algo_Super_SDK diff --quiet -- \
  .gitlab-ci.yml rebuild.sh release_pack.sh scripts/release_pack ci/resolve_build_env.sh
cd /home/maxin/Code/560D/Algo_Super_SDK/dist
sha256sum -c Algo_Super_SDK_v1.0.2_aarch64_Android_TFLite_2.21.0.sha256
tar -tzf Algo_Super_SDK_v1.0.2_aarch64_Android_TFLite_2.21.0.tar.gz
```

Also record the already reproduced Manifest failure, including the rejected path
`3rd_party/opencv/lib/libc++_shared.so`. Do not claim that the target repository tracks the
sample archive; identify it as a local assessment fixture.

- [ ] **Step 3: Define the acceptance matrix**

The matrix must require all of the following before the gate can close:

```text
Static contract: package SHA, Manifest Schema and every deploy.files digest pass.
Runtime closure: entry, binary, model, config, input and all dynamic libraries exist.
Package scope: include/, example/ and release-only documentation are not deployed.
CI aggregation: all eight meta documents are present before bundle publication.
Scheduling: unsupported device capabilities are rejected before dispatch.
Windows device test: status=COMPLETED, exit_code=0, criteria_met=true.
Collection: results/result.json and required logs are pulled successfully.
```

- [ ] **Step 4: Verify report consistency**

Run:

```bash
rg -n '57d3ca01|73cf87bc|P0-[1-7]|P1-[1-5]|libc\+\+_shared|criteria_met=true' \
  docs/assessments/algo-super-sdk-packaging.md
rg -n 'TODO|TBD|PLACEHOLDER' docs/assessments/algo-super-sdk-packaging.md
git diff --check
```

Expected: the first command finds every evidence family; the placeholder scan prints nothing;
`git diff --check` exits 0.

- [ ] **Step 5: Commit the assessment**

```bash
git add docs/assessments/algo-super-sdk-packaging.md
git commit -m "docs: assess Algo SDK package compatibility"
```

### Task 2: Add the Phase 1 dispatch gate

**Files:**
- Modify: `CLAUDE.md:262`
- Reference: `docs/assessments/algo-super-sdk-packaging.md`

- [ ] **Step 1: Insert the gate after the Phase 1 CI item**

After Phase 1 item 2 and before the `agent-cli` item, add this block without renumbering the
existing roadmap:

```markdown
   **Algo_Super_SDK 适配门禁**：按
   `docs/assessments/algo-super-sdk-packaging.md` 关闭全部 P0 整改项；代表性 Android
   测试包必须通过静态契约检查和原生 Windows 实机验收，之后 Trigger 才能将该业务
   仓库产物派发给 Client Agent。SDK 发布包不得未经裁剪和测试入口适配直接进入设备
   测试链路。
```

- [ ] **Step 2: Verify the authoritative roadmap link and scope**

Run:

```bash
rg -n -A5 -B2 'Algo_Super_SDK 适配门禁' CLAUDE.md
test -f docs/assessments/algo-super-sdk-packaging.md
git diff --check
git status --short
```

Expected: the gate appears under Phase 1 item 2, its linked report exists, whitespace checks pass,
and only the intended documentation change is pending.

- [ ] **Step 3: Commit the roadmap gate**

```bash
git add CLAUDE.md
git commit -m "docs: gate Algo SDK package dispatch"
```

### Task 3: Run final documentation verification and synchronize master

**Files:**
- Verify: `docs/assessments/algo-super-sdk-packaging.md`
- Verify: `CLAUDE.md`

- [ ] **Step 1: Verify spec coverage and repository isolation**

Run:

```bash
git diff HEAD~2 --check
git diff HEAD~2 --name-only
git -C /home/maxin/Code/560D/Algo_Super_SDK status --short
```

Expected: Hermes changes contain only the assessment and roadmap gate. The target status may show
its pre-existing untracked files, but no new tracked diff may be introduced by this work.

- [ ] **Step 2: Verify local documentation content**

Run:

```bash
rg -n 'SDK 发布完整性|Hermes 测试包适配性|P0-7|P1-5|验收矩阵' \
  docs/assessments/algo-super-sdk-packaging.md
rg -n 'Algo_Super_SDK 适配门禁' CLAUDE.md
```

Expected: every required conclusion, final remediation ID, acceptance section, and roadmap gate is
present.

- [ ] **Step 3: Push and verify remote master**

```bash
git push origin master
git rev-parse HEAD
git ls-remote origin refs/heads/master
git status --short --branch
```

Expected: local and remote commit hashes match and the Hermes working tree is clean.
