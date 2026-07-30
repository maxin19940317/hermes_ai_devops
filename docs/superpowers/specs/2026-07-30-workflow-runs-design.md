# Workflow Runs 设计

**日期：** 2026-07-30
**状态：** 已批准
**范围：** 三轮交互能力的第一轮，只建立权威 workflow 运行记录、精确 rerun 和项目隔离；不实现飞书按钮。

## 1. 背景

当前 `artifacts` 与 `tasks` 都不保存 `Version` / `RuleVersion`。飞书 `rerun`
只能用 `<sha> <pipeline_iid> [variant]` 从 artifacts 猜回输入，因此会丢失规则版本，
也会把项目相同 commit/pipeline/variant 的数据混在一起。`RecentRuns` 同样只能用
workflow ID 字符串前缀近似关联 task。

这轮新增 `workflow_runs` 作为每次 Temporal workflow 启动输入的权威、不可变索引：

- 新运行在任何设备测试活动之前登记；
- rerun 只接受精确 `source_workflow_id`；
- Version、RuleVersion、项目和目标变体均从源 run 恢复，不从旧数据推断；
- 没有 run 行的历史数据只用于展示，不可触发副作用；
- artifacts 的逻辑键补上 project，消除跨项目串包。

本表不保存 packages URL、workflow 状态或终态输出。包仍由 artifacts 恢复；源 workflow
是否已经关闭及其 `DeviceTestOutput` 由 Temporal 精确确认。

## 2. 数据模型

### 2.1 workflow_runs

```sql
CREATE TABLE IF NOT EXISTS workflow_runs (
    workflow_id       TEXT        PRIMARY KEY,
    project           TEXT        NOT NULL,
    commit_sha        TEXT        NOT NULL,
    pipeline_id       INTEGER     NOT NULL CHECK (pipeline_id > 0),
    version           TEXT        NOT NULL,
    rule_version      TEXT        NOT NULL,
    scope             TEXT        NOT NULL DEFAULT '',
    attempt           INTEGER     NOT NULL CHECK (attempt >= 0),
    variants          TEXT[]      NOT NULL,
    source_workflow_id TEXT       REFERENCES workflow_runs(workflow_id) ON DELETE RESTRICT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (source_workflow_id IS NULL OR source_workflow_id <> workflow_id)
);

CREATE INDEX IF NOT EXISTS workflow_runs_recent_idx
    ON workflow_runs(created_at DESC, workflow_id DESC);

CREATE INDEX IF NOT EXISTS tasks_run_variant_latest_idx
    ON tasks(workflow_id, test_id, attempt DESC, created_at DESC);
```

字段语义：

| 字段 | 契约 |
|---|---|
| `workflow_id` | 实际 Temporal execution ID；必须等于 `DeviceTestInput.WorkflowID()` |
| `project/commit_sha/pipeline_id/version` | 原启动输入，逐字保存 |
| `rule_version` | workflow 缺省归一化后的实际规则版本，禁止空值 |
| `scope/attempt` | workflow ID 派生输入 |
| `variants` | 从 `Packages[].Variant` 取值，去空、去重、字典序排序；允许空数组 |
| `source_workflow_id` | 首次运行为空；显式 rerun 指向直接来源 |
| `created_at` | 数据库写入时间，不参加幂等内容比较 |

`variants` 是集合，不承诺保存原 packages 顺序。rerun 按 canonical variant 顺序构造输入。

### 2.2 不可变与幂等

`RecordWorkflowRun` 的行为固定为：

1. 校验并规范化输入；WorkflowID、Project、Commit、Version、RuleVersion 禁止空值；
2. `INSERT ... ON CONFLICT DO NOTHING`；
3. 若已存在，重新 `SELECT` 并比较除 `created_at` 外的全部字段；
4. 全部相同返回成功；
5. 任一字段不同返回 `ErrWorkflowRunConflict`。

Postgres 实现使用事务。不能用 `ON CONFLICT DO UPDATE` 冒充幂等写；并发冲突后必须在新
statement snapshot 中读取胜出的行。MemStore 与 PGStore 通过同一 conformance suite。

### 2.3 artifacts 项目隔离

artifacts 的逻辑键从：

```text
(commit_sha, pipeline_id, variant)
```

改为：

```text
(project, commit_sha, pipeline_id, variant)
```

下列接口及其所有调用方必须增加 `project`：

```go
ListArtifacts(ctx, project, commitSHA, pipelineID)
NextWorkflowAttempt(ctx, project, commitSHA, pipelineID, variant)
NextWorkflowAttemptAll(ctx, project, commitSHA, pipelineID)
```

`RegisterArtifacts`、MemStore map key、PG 查询和冲突键也使用四元组。两个项目使用相同
commit/pipeline/variant 时必须能分别登记、查询和递增 attempt。

旧唯一键曾经静默吞掉的跨项目产物无法由迁移恢复。它们继续视为历史缺失，rerun
fail closed；需要时由原 webhook/kick 重新登记。

新接收的 project 必须是最长 256 字符、无空白的 GitLab namespace path：
`segment[/segment...]`，segment 只含 ASCII 字母、数字、下划线、点和连字符，且不接受
`..`。加上现有 128 字符 variant 上限后，生成的 scoped retry workflow ID 仍小于命令参数
的 512 字符上限。存量不符合该语法的 legacy workflow 继续只读。

## 3. Workflow 写入边界

### 3.1 输入契约

`DeviceTestInput` 新增：

```go
SourceWorkflowID string `json:"source_workflow_id,omitempty"`
```

普通 bundle/kick 为空；rerun 填源 workflow ID。workflow ID 的格式不因该字段改变。

### 3.2 版本门

`DeviceTestWorkflow` 保持现有 rule version 规范化和校验。校验成功后、调用
`SelectTestSpecs` 前增加独立 change ID：

```text
record-workflow-run-v1
```

- 旧 history 的 `DefaultVersion` 分支不安排新 activity；
- 新 workflow 的版本 1 分支先安排 `RecordWorkflowRun`；
- 改动前的 `history-pre-notify-card.json` 必须原样重放通过，不得重录 fixture。

新分支先断言实际 execution ID 等于 `in.WorkflowID()`，随后构造
`RecordWorkflowRunRequest`。`workflow_id` 取实际 execution ID，`rule_version` 取已规范化值，
variants 从 packages 规范化产生。

### 3.3 独立重试策略

Record activity 使用独立的 activity context：

- `StartToCloseTimeout`: 30 秒；
- `InitialInterval`: 2 秒；
- `BackoffCoefficient`: 2；
- `MaximumInterval`: 1 分钟；
- `MaximumAttempts`: 0，表示无限重试。

该 context 不得复用于后续活动。数据库暂态失败时 workflow 保持等待；Record 成功前
`SelectTestSpecs`、Acquire、Dispatch 均为零调用。

`ErrWorkflowRunConflict`、输入校验失败和永久数据库约束错误必须在 activity 中转换成
Temporal non-retryable application error。Store 提供可由 `errors.Is` 判断的
`ErrWorkflowRunPermanent`；PG 实现至少把 SQLSTATE `23502`（NOT NULL）、`23503`（FK）、
`23505`（读回后确认不是幂等写的唯一冲突）和 `23514`（CHECK）归入该错误。连接失败、
超时等暂态错误才无限重试。不可变内容或缺失 source FK 不能永久占住 workflow。

## 4. Store API

新增核心类型：

```go
type WorkflowRun struct {
    WorkflowID       string
    Project          string
    CommitSHA        string
    PipelineID       int
    Version          string
    RuleVersion      string
    Scope            string
    Attempt          int
    Variants         []string
    SourceWorkflowID string
    CreatedAt        time.Time
}

```

新增方法：

```go
RecordWorkflowRun(ctx context.Context, run WorkflowRun) error
GetWorkflowRun(ctx context.Context, workflowID string) (*WorkflowRun, error)
```

`GetWorkflowRun` 不存在时返回可由 `errors.Is` 判断的 `ErrWorkflowRunNotFound`。
变体状态读取 API 本轮不提供：当前生产路径没有消费者。RecentRuns 直接在自己的权威查询中
按精确 `workflow_id/test_id` 关联 tasks；rerun 的失败集合来自 Temporal output。若后续出现
独立消费者，再按实际调用契约新增该 API。

活动层的 Store 子接口只增加 `RecordWorkflowRun`。飞书命令层增加 `GetWorkflowRun`。

## 5. 精确 rerun

### 5.1 新语法

唯一支持的语法：

```text
rerun <source_workflow_id> [variant]
```

workflow ID 和 variant 都是无空白参数，单项最长 512 字符；真实 workflow ID 中的项目路径
包含 `/`，命令契约必须允许该字符。

旧的 `rerun <sha> <pipeline_iid> [variant]` 必须返回明确迁移提示：

```text
旧 rerun 语法已停用，请使用 rerun <source_workflow_id> [variant]
```

不得把旧参数误当 workflow ID 后返回含糊的“查无记录”。

旧语法在查 workflow_runs 前机械识别：

1. `len(args) == 3`，直接返回迁移提示；
2. `len(args) == 2`，且第一个参数是合法 SHA、第二个参数是正整数，返回迁移提示；
3. 其余只允许 1 到 2 个参数，按新语法解析；
4. 新语法的第二个参数始终解释为 variant。

### 5.2 源运行校验

执行顺序：

1. 精确 `GetWorkflowRun(source_workflow_id)`；没有权威行即拒绝；
2. 通过 Temporal `DescribeWorkflowExecution` 确认源 execution 已关闭；
3. Temporal 无记录、查询失败或仍在运行均 fail closed，不启动新 workflow；
4. 从源 run 继承 Project、Commit、PipelineID、Version、RuleVersion；
5. artifacts 只按源 run 的 project/commit/pipeline 精确查询。

`WorkflowStarter` 增加只读方法：

```go
WorkflowClosed(ctx context.Context, workflowID string) (bool, error)
WorkflowResult(ctx context.Context, workflowID string) (*wf.DeviceTestOutput, error)
```

生产实现分别使用 Temporal Describe 和 `GetWorkflow(...).Get(...)`；测试 fake 必须覆盖
running、closed、not found、无可读结果和查询失败。

### 5.3 目标变体

指定 variant：

- 必须属于 `source.variants`；
- 允许显式重跑 PASSED、失败或原先 SKIPPED 的变体；
- 必须存在对应 artifact，否则拒绝；
- 新输入 `Scope = variant`。

未指定 variant：

- 读取已关闭源 workflow 的权威 `DeviceTestOutput`；
- 只选择 `verdict != PASSED && verdict != SKIPPED` 的 TaskSummary；
- 不从 tasks 缺行推断 SKIPPED；
- Temporal 结果不存在或不可读取时 fail closed；
- 没有可重跑失败变体时明确返回，不分配 attempt、不启动 workflow；
- 新输入保留源 `Scope`。

这里不能只查询 tasks：`CreateTask` 活动自身失败时 workflow 会生成失败 `TaskSummary`，但
数据库没有对应 task 行；只查 tasks 会把真实失败误当成 SKIPPED。Temporal output 是该
终态集合的权威来源。指定 variant 是用户的显式选择，只要求源 workflow 已关闭和成员关系
成立，不依赖 output，因此仍可重跑原先 SKIPPED 的变体。

两种路径都只把目标 variants 的 packages 放入新输入，并设置
`SourceWorkflowID = source.workflow_id`。

### 5.4 attempt 分配

所有目标和 artifact 完整性校验通过后才分配 attempt：

- 指定 variant：调用带 project 的 `NextWorkflowAttempt`；
- 未指定 variant：调用带 project 的 `NextWorkflowAttemptAll`。

`NextWorkflowAttemptAll` 在锁定该项目/commit/pipeline 下全部 artifact 行后计算
`MAX(workflow_attempt)+1`，并把每一行都设置到这个相同水位；新 workflow 仍只携带失败
variants。统一水位保证 bundle 级和后续变体级 `-r{N}` 共用单一单调序列，不会与已有
workflow ID 碰撞。Temporal start 失败可以留下未使用的 attempt，不回退计数器。

## 6. RecentRuns 与 NL 快照

`RecentRun` 增加：

```go
WorkflowID    string
Version       string
RuleVersion   string
Authoritative bool
```

`RecentRuns(limit)` 的顺序和 limit 契约：

1. 从 `workflow_runs` 按 `created_at DESC, workflow_id DESC` 读取并展开 variants；
2. 同一 run 内按 canonical variants 顺序；
3. task 结论只按精确 `(workflow_id, test_id)` 关联；
4. limit 按展开后的变体行计算；
5. 不足 limit 时，从旧 artifacts 查询补尾；
6. fallback 排除已有任一权威 run 覆盖的 `(project,commit,pipeline,variant)`；
7. fallback 行 `Authoritative=false`，WorkflowID/Version/RuleVersion 为空；
8. 权威行永远排在 fallback 前，不做全局时间重排。

NL snapshot 的 recent run 增加 `workflow_id` 与 `authoritative`。只有 authoritative 行可以被
模型渲染为 rerun；Runtime 执行时仍重新 GetWorkflowRun 和检查 Temporal 终态，不能信任快照。
prompt、command schema、示例和参数校验同步改成新语法。该变化与旧 rerun 参数语义不兼容，
因此当前 translation contract 与 prompt 一并升到 v2；保留 `cmd_translate_v1.md` 不改，
确保历史 `prompt_version=cmd_translate_v1` 仍可解释。

## 7. 数据库迁移与部署

同时更新 embedded `schema.sql` 和新增独立 migration。migration 在一个事务中：

1. 创建 `workflow_runs` 及索引；
2. 创建 artifacts 新四元组唯一索引；
3. 删除旧三元组唯一约束；
4. 保留 artifacts 原数据，不做 workflow_runs 回填。

旧二进制的 `ON CONFLICT (commit_sha,pipeline_id,variant)` 在旧约束删除后无法工作，因此这次
不是可混跑升级。部署顺序固定为：

1. 先独立部署当前已合入但尚未上线的按需预签、evidence v3 和归因功能；
2. 完成既定观察期并确认该批功能稳定；未通过 go/no-go 时不得开始本轮迁移；
3. 停止 trigger/worker 等 artifact 写入方；
4. 执行 migration；
5. 整组部署包含四元组逻辑键的新二进制；
6. 启动 worker，再恢复 trigger/入口流量。

不得 migration 后继续运行旧写入方，也不得先部署新 worker 再迁移。部署文档必须写明该
短暂停写窗口。本轮代码完成、合并或构建镜像都不自动授权执行生产迁移；生产 go/no-go
必须在前一批功能独立稳定后另行确认。

## 8. 失败处理

| 场景 | 行为 |
|---|---|
| Record 暂态 DB 错误 | activity 无限重试，设备活动零调用 |
| 同 workflow ID 内容不同 | non-retryable conflict，workflow 失败 |
| legacy run 没有 workflow_runs | 只展示，rerun 拒绝 |
| 源 workflow 仍运行或 Temporal 无法确认 | rerun 拒绝 |
| 无 variant 且 Temporal output 不可读取 | rerun 拒绝 |
| 源 variant 无 artifact | rerun 拒绝，attempt 不递增 |
| 无失败变体 | 返回“没有可重跑的失败变体”，不启动 |
| Temporal start 返回 AlreadyStarted | 视为幂等成功，不分配第二个 ID |
| start 真失败 | 返回错误；已分配 attempt 不回收 |

workflow_runs 不保存 package URL，因此同源重跑依赖 artifacts 行保持不可变。未来若允许更新
artifact URL/hash，必须先设计 run-package 快照；本轮禁止把 `DO NOTHING` 改成 upsert。

## 9. 测试与验收

### 9.1 Store conformance

MemStore 和 PGStore 共用测试，机械验证：

- Record/Get 完整字段；
- variants 去空、去重、排序；
- 同 ID 同内容重复写成功；
- 同 ID 任一字段不同返回 conflict；
- 跨项目相同 commit/pipeline/variant 分别登记、查询、递增；
- exact run/task 关联不串 base、scope、`-rN` 或对抗项目；
- RecentRuns 权威优先、fallback 补尾、去重、展开后 limit；
- 返回切片是防御性副本。

### 9.2 Workflow/activity

- 新 workflow 的首个 scheduled activity 是 `RecordWorkflowRun`；
- 请求使用实际 workflow ID、规范化 RuleVersion、canonical variants；
- Record 失败时 Select/Acquire/Dispatch 零调用；
- conflict 是 non-retryable；
- 缺失 source FK、CHECK/NOT NULL 违反也是 non-retryable；
- Record context 的无限重试配置不泄漏给 Select；
- 原 `history-pre-notify-card.json` 不修改且继续重放通过。

### 9.3 rerun

- 旧语法明确迁移提示；
- 旧 2/3 参数与新 1/2 参数分别覆盖，检测顺序不会混淆；
- legacy/nonexistent/running/Temporal unknown 源均拒绝；
- Version/RuleVersion/Project/SourceWorkflowID 原样继承；
- 未指定时从 Temporal output 只选失败 summary，排除 PASSED/SKIPPED；
- AcquireDevice 耗尽、CreateTask 耗尽等没有 task 行的失败仍被正确选中；
- FinishTask 落库失败导致 task 状态滞后时，仍以 workflow output 为准；
- 指定时成员校验并允许显式重跑 PASSED/SKIPPED；
- 缺 artifact 时 attempt 与 starter 均零调用；
- project-aware artifact 查询不串项目；
- artifact 查询结果按目标 variants 过滤，且每个目标必须恰好命中一行；
- bundle retry 只携带失败包，但 attempt 分配保持 ID 唯一；
- AlreadyStarted 返回幂等结果。

### 9.4 NL snapshot

- golden snapshot 含 workflow_id、version、rule_version、authoritative；
- fallback 行不可生成 rerun；
- prompt/schema 只接受 `rerun <workflow_id> [variant]`；
- Runtime 在执行前再次精确校验，不信任模型输出。

### 9.5 Migration

- fresh `schema.sql` 初始化通过；
- 从含旧 artifacts 三元组约束的 schema 执行 migration 通过；
- migration 后可插入两个项目的相同 commit/pipeline/variant；
- `pgtest` 清理包含 workflow_runs；
- migration 和 schema 的最终约束一致。

## 10. 完成定义

- 所有新 workflow 在测试前拥有不可变、权威 workflow_runs 行；
- 旧 history 无需重录即可重放；
- rerun 不再从 sha/pipeline 猜 Version 或 RuleVersion；
- 无 variant 的 rerun 只运行失败变体；
- 跨项目 artifacts 不串包、不共享 attempt；
- RecentRuns 不再用前缀关联新数据，legacy 只读降级；
- runtime 全测试、store PG conformance、`go vet ./...` 全绿；
- 文档删除旧 rerun 用法，并记录停写迁移顺序；
- 本轮不加入任何飞书 action/button/audit_log。
