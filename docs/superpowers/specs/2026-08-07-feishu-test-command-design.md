# 飞书 `test` 指令设计（按变体名触发测试）

日期：2026-08-07

状态：**待实施**（2026-08-07 需求确认：路径 2，新增 `test <variant>` 命令）

## 1. 背景与动机

用户想在飞书里直接输入指令测试某个包（变体）。现状：

| 命令 | 能力 | 缺口 |
|---|---|---|
| `plan <需求>` | 自然语言 → Plan DSL | **只规划不执行**（executor.go:771） |
| `rerun <source_workflow_id> [variant]` | 重跑**历史** run | 必须已有源 workflow |
| CI kick | 构建后自动测 | 不可人工按需触发 |

**缺的是"按变体名直接触发新测试"**：校验变体名 → 查最近 artifact → 启动 workflow。

## 2. 关键决策

1. **新增封闭命令 `test <variant> [commit]`**：
   - `<variant>` 必填：变体名白名单校验（`e.Variants`，与 plan 上下文同源）。
   - `[commit]` 可选：指定 commit（short sha）；缺省取该变体**最近一次构建**。
   - 变体名是封闭枚举 → LLM 翻译层可识别（"测试 SNPE 2.21" → `test aarch64_Linux_QCS6490_SNPE_2.21`）。
2. **复用 rerun 的启动链路**：查 artifact → `NextWorkflowAttempt` 取 attempt → 构造
   `DeviceTestInput`（scope=variant）→ `StartDeviceTest`。
3. **副作用二次确认**：走 pendingCmd 机制（与 rerun 同款）——设备测试是副作用，
   防误触发。回显"将测试 <variant>（<commit>），确认？"。
4. **输入来源**：`ListArtifacts` 按 (project, commit, variant) 找包。缺省 commit 时
   取该变体 `created_at` 最新的 artifact（跨 project/commit 的"最近一次构建"）。
5. **通知**：变体级 workflow（scope 非空）→ 方案 A 正常发卡片。test 命令启动的
   workflow 与 kick 启动的无差别。
6. **不落新表**：审计走既有 `command_translations`（outcome 沿用翻译/执行路径），
   无需新 schema。

## 3. 设计

### 3.1 命令形态

```
test <variant> [commit]
  variant 必填: 合法变体名(白名单 e.Variants)
  commit 可选:  short sha;缺省 = 该变体最近一次构建
```

`command.schema.json`：`test` 加入 Command 枚举（`args: [variant, commit?]`）。

### 3.2 执行流程

```
test aarch64_Linux_QCS6490_SNPE_2.21
  → 校验 variant ∈ e.Variants(否则报"未知变体,可用列表...")
  → 查 artifact:该变体最近一次(commit 缺省)或指定 commit
      → 无 artifact:报"该变体暂无构建记录"
  → 构造 pendingCmd(确认:回显 variant+commit+包名)
  → 用户确认
      → NextWorkflowAttempt(project, commit, pipeline, variant) 取 attempt
      → DeviceTestInput{ scope: variant, attempt: n, packages: [pkg] }
      → StartDeviceTest
      → 回复"已启动: <workflow_id>"
```

### 3.3 与 rerun 的差异

| 项 | rerun | test |
|---|---|---|
| 来源 | 源 workflow 的失败变体 | 任意变体(最近构建) |
| 前置 | 源 workflow 已结束 | 无(只要 artifact 存在) |
| scope | 继承源 | variant |
| attempt | NextWorkflowAttempt | 同 |

## 4. 测试

- 命令解析：`test <variant>`、`test <variant> <commit>`、缺参数/未知变体。
- 校验：未知变体报错；无 artifact 报错。
- 确认：回显确认 → 确认后启动；拒绝/超时不启动。
- 启动：fakeStarter 断言 DeviceTestInput(scope=variant, attempt 递增, packages 正确)。
- 翻译：LLM 映射"测试 SNPE" → test 命令。

## 5. 不做（本轮）

- `test` 指定多变体（一次测多个）：语法复杂，变体级 workflow 天然支持
  单变体；多变体走 `plan` 演进（Phase 2b）或 bundle。
- `test` 指定设备：设备由调度器按约束选择，不人工指定。
- test 结果汇总卡片：沿用方案 A（变体级发卡）。
