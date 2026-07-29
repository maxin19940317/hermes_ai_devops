# 失败归因分离设计（差距 #10）

日期：2026-07-29

状态：**已批准**（2026-07-29 评审通过；修订含 §4.1 callbacks 宕机盲区、§6 历史值归零、§7 签名链路已在生产验证）

## 1. 背景

`docs/device-test-sequence.md:233` 定的目标语义是：设备级 INFRA → `device_fail_streak+1`，
Client/网络级 → `client_fail_streak+1`，终态成功类 → 清零，`device_fail_streak` 连续 3 次
→ QUARANTINED。差距清单 #10 记的现状是"单一 fail_streak，网络问题可能误隔离设备"。

调研后现状比这条描述更严重，三条证据：

1. **`rules.CategoryDevice` 声明了但全仓库无任何代码产出**（`rules/rules.go:59`）。唯一可能
   的来源是 Manifest 签名的 `classify: DEVICE`，而 `ci/variants.yaml` 里 8 个变体的全部签名
   只有 `MODEL` 与 `DELEGATE`，一个 DEVICE 都没有。
2. **`decideV1` 的三条 INFRA 分支没有一条是设备级**：`InfraReason` 非空（Runtime 侧故障）、
   `Status == "FAILED"`（注释原文 "client-side pipeline failure"）、`TIMEOUT`。
3. **终态释放是 `release(d.Category == rules.CategoryInfra)`**（`workflow/devicetest.go:437`）。
   于是 client 侧流水线失败、测试超时、以及 `CheckLease`/`LoadResult` 这类 Runtime 自身故障，
   全部记到**设备**的 `fail_streak` 上。

即：今天连 Runtime 自己的数据库抖动都会把设备推向隔离，而真正的设备故障反而没有任何信号源。

## 2. 关键决策

1. **先拆归因，隔离暂时失效**（已与负责人确认，2026-07-29）。按正确归因，`device_fail_streak`
   在现有证据源下**结构性恒为 0**，§10 的"连续 3 次 INFRA 隔离设备"随之不再触发。这不是回归，
   是把一个一直在误伤的机制停掉；恢复它需要 agent 侧提供设备级信号（见 §7）。
2. **`client_fail_streak` 只计数与展示，不驱动行为**。不做"达阈值停止向该 client 派单"——
   那要改 `AcquireDevice` 的选择逻辑并配恢复路径，且风险是悄悄把整台机器摆出调度池。先把
   事实看见，自动处置另案。
3. **归因由 workflow 在每个调用点显式给出**，不由规则引擎产出，也不由 store 从 reason 推断。
   workflow 知道自己站在哪个失败分支上；规则引擎 v1 的行为不得修改（`rules.go:95` 注释），
   而从 reason 字符串反推"是 client 挂了还是库挂了"是脆的。
4. **`devices.fail_streak` 保留原列名，语义收窄为设备级**；新增 `clients.fail_streak`。
   不重命名既有列——重命名是破坏性变更，而这一列的读者（FleetOverview、飞书 `devices`
   指令、UnquarantineDevice）语义上本来就是设备级。
5. **用 `workflow.GetVersion` 保护在途 workflow 的重放**（见 §5）。

不采用的方案：

- 规则引擎产出 scope：见决策 3。
- 把 `hard deadline`/`TIMEOUT` 启发式归为设备级以保住隔离：测试本身写得慢就会隔离无辜设备，
  用误伤换"机制还活着"的表象。
- 完全不建计数器、只保证不误计：改动最小，但"这个 client 是不是在持续出问题"无处可查。

## 3. 范围

交付：

- `clients.fail_streak` 列 + 迁移文件
- `ReleaseRequest` 增加 `FailScope` 字段（保留 `InfraFail`，只加不删）
- workflow 侧归因表（表驱动纯函数）+ `workflow.GetVersion` 分支
- `store.ReleaseDevice` 按 scope 记账（双实现 + conformance）
- `FleetOverview` 暴露 client 计数，飞书 `status`/`devices` 输出带上
- 文档：`docs/device-test-sequence.md` 差距 #10 状态更新

不交付（明确排除）：

- agent 侧设备级信号（result.json 契约变更）——见 §7，另案
- client 达阈值停止派单
- `devices.fail_streak` 列重命名
- 恢复设备隔离机制

## 4. 归因表

四个取值。命名以"这次失败该记在谁头上"为准：

| scope | 含义 | 计数动作 |
|---|---|---|
| `ok` | 终态且非 INFRA 类判定 | **两个计数器都清零** |
| `device` | 设备级失败 | `devices.fail_streak + 1`；达阈值 → QUARANTINED |
| `client` | Client Agent / 与它之间的网络 | `clients.fail_streak + 1` |
| `none` | Runtime 自身故障、取消、成因两可 | **两个计数器都不动** |

`none` 与 `ok` 的区别是本设计的要点：Runtime 挂了不是设备健康的证据，所以不能清零；
也不是设备的错，所以不能加一。今天这两种情况都被当成"设备又坏了一次"。

各释放点的归因：

| 释放点（`devicetest.go`） | 失败原因 | scope | 理由 |
|---|---|---|---|
| CreateTask 失败 | `create task: …` | `none` | Runtime/DB 侧；设备刚拿到手还没用过 |
| Dispatch 失败 | `dispatch: …` | `client` | POST 不到 Client Agent |
| awaitResult | `lease expired (no heartbeat)` | `client` | Agent 停止心跳＝失联 |
| awaitResult | `check lease: …` | `none` | CheckLease 活动自身失败＝Runtime/DB |
| awaitResult | `hard deadline exceeded` | `none` | 成因两可（设备卡死或测试本身慢），不猜 |
| awaitResult | `workflow canceled` | `none` | 人为取消 |
| LoadResult 失败/无行 | `load result: …` | `none` | outbox/DB 链路异常 |
| 终态 | `d.Category == DEVICE` | `device` | 目前不可达，为 §7 预留 |
| 终态 | `d.Category == INFRA` 且 `res.Status == "FAILED"` | `client` | decideV1 该分支的原意就是 client 侧流水线失败 |
| 终态 | `d.Category == INFRA` 且 `res.Status == "TIMEOUT"` | `none` | 超时是工作负载属性，不是某一方的故障 |
| 终态 | 其余（PASSED / TEST_FAILED / PERF 等） | `ok` | 设备与 client 都把活干完了 |

终态那三行需要 `res.Status`，而 `d.Category` 单独不足以区分 FAILED 与 TIMEOUT——两者都是
`CategoryInfra`。归因函数因此同时吃 category 与 status。

### 4.1 已知盲区：callbacks 宕机会被归成 client 失联

`lease expired (no heartbeat)` → `client` 这一行有一个 workflow 视角内**无法区分**的情形：
若 callbacks 进程自身宕机 ≥120s，Client 的心跳送不达，租约照样过期，CheckLease 照样判
"失联"——但故障方是 Runtime，不是 Client。

workflow 只能看到"租约没续上"，看不到"是谁没送到"。要区分需要额外事实（如 callbacks 侧
的自身可用性记录），本轮不做。

本轮代价为零：`client_fail_streak` 只计数不驱动行为（决策 2），误计不会导致任何自动处置。
**但若将来用它做自动处置（达阈值停止向该 client 派单），这条必须先解决**，否则 Runtime
自己重启一次就会把整个 fleet 的 client 全停掉。

可用的判别特征：callbacks 宕机时**全 fleet 的 client 计数同时 +1**，而单个 Client Agent
故障只影响它自己。这个特征可以作为将来自动处置的护栏（同一时间窗内多 client 同时计数
上涨 → 判为 Runtime 侧故障，不计）。

归因是一个纯函数，表驱动单测：

```go
// failScope 决定一次释放该记在谁头上(设计文档 §4)。
// 输入是 workflow 已知的事实,不解析 reason 字符串。
func failScope(site releaseSite, category rules.Category, resultStatus string) string
```

## 5. Temporal 重放兼容

`ReleaseRequest` 是 activity 输入，进 workflow history。改变 workflow 传给 `ReleaseDevice`
的载荷会让**在途 workflow 重放时命令与历史不匹配**，判非确定性失败。

因此：

1. `ReleaseRequest` **只加不删**：保留 `InfraFail bool`，新增 `FailScope string`。
2. workflow 侧用 `workflow.GetVersion(ctx, "release-fail-scope", workflow.DefaultVersion, 1)`
   分支：旧版本走原样 `InfraFail`，新版本填 `FailScope`。这与仓库已有的 `rule_version`
   路由是同一思路——行为演进靠版本分支，不靠原地改。
3. Activity 侧兼容两种载荷：`FailScope` 非空时按 scope 记账；为空时按旧语义
   （`InfraFail=true` → `device`，`false` → `ok`）。旧 workflow 的重放因此保持原行为。

不采用"挑无在途任务的窗口部署"：Phase 1 规模下确实可行，但把正确性寄托在部署时机上，
下次规模变大就会踩到。

## 6. 数据模型

```sql
-- clients 表加 client 级失败计数(差距 #10)。
-- devices.fail_streak 保留原名,语义收窄为"设备级"。
ALTER TABLE clients ADD COLUMN IF NOT EXISTS fail_streak INTEGER NOT NULL DEFAULT 0;

-- 历史值按旧(错误)语义累计:client 侧失败、超时、Runtime 自身故障都记在设备头上。
-- 语义既然收窄为"设备级",旧值就不该带进新语义——归零重新开始计。
-- (线上现值恰好是 0,这一句是把语义意图写明,不是修数据。)
UPDATE devices SET fail_streak = 0;
```

`ReleaseDevice` 不需要新增 client 参数：`devices.client_id` 已经记录归属，
按 device 反查即可。

`FleetOverview` 的 `DeviceStatus` 增加两个字段：

```go
ClientID        string // 归属 client
ClientFailStreak int   // 该 client 的连续失败计数(差距 #10)
```

飞书 `status` / `devices` 的输出各加一段，形如
`513cd3de soc=QCM6125 status=IDLE fail=0 client=c1 client_fail=2`。

## 7. 设备级信号源（本轮不做，记录去向）

`device_fail_streak` 拆出来之后没有信号源。要让它真正动起来、进而恢复隔离机制，需要
**agent 侧能区分"设备挂了"与"我自己挂了"**。两条候选路径：

- `result.json` 契约加 `failure_scope` 字段（`device|client`），由 agent 在
  adb 断连、设备离线、预检不过等情形下填 `device`；
- 或约定一组保留签名 id（如 `adb_disconnected`），由 `ci/variants.yaml` 以
  `classify: DEVICE` 声明，走既有的签名→类别链路，无需改契约。

第二条明显更省事，而且成本比"复用现成链路"还要低一档：`88caf07`（feat(runtime): let rule
engine consume runtime-extracted signature hits）之后，**Runtime 侧确定性提取的签名命中已经
直接进规则判定**（`devicetest.go:416` 的 `mergeSignatureHits` → 二次 `rules.Decide`）。
也就是说签名 → `CategoryDevice` 这条路今天是**已在生产中跑通的链路，零 rules 改动**，
唯一前置是 agent 把 adb 层错误写进它采集的日志流（logcat/stderr），再在
`ci/variants.yaml` 里以 `classify: DEVICE` 声明一条签名。

届时隔离机制可以在正确的信号源上复活——不需要改判定逻辑，只需要让真正的设备故障
第一次拥有信号。

在此之前，`device_fail_streak` 恒为 0 是**预期状态**，不是缺陷。代码与文档都要写明，
否则下一个读代码的人会把它当 bug"修"回去。

## 8. 错误处理

- 归因函数是纯函数，无失败路径；未覆盖的组合返回 `none`（保守：不加不减）。
- `ReleaseDevice` 的既有幂等语义不变：非租约持有者释放、租约已易主、重复释放一律无副作用。
  scope 只在实际发生释放时才影响计数。
- 释放与计数在**同一事务**内完成（两列分属 `devices` 与 `clients` 两张表）。失败则整体回滚，
  由 activity 重试——`ReleaseDevice` 本就是幂等的，重试安全。
  不采用"计数失败也要放行设备"：那会产生"设备已回池但计数没记上"的中间态，而设备即使
  没被释放也会在租约过期后被 `AcquireDevice` 懒回收（`devices.go:148` 的 `leasable`），
  所以放行的紧迫性并不足以换取一致性。

## 9. 测试

**归因表（`workflow` 包，表驱动）**：§4 每一行一个用例，断言 `failScope` 的输出。
特别包含两条今天会误伤的：`check lease` 失败 → `none`（不是 `device`），
终态 INFRA + FAILED → `client`（不是 `device`）。

**重放兼容（`workflow` 包）**：用 Temporal 测试框架跑一个旧版本 history 的重放，
断言不出现非确定性错误。若测试框架构造旧 history 成本过高，退而断言
activity 侧对空 `FailScope` 的兼容分支（见下）。

**store conformance（双实现）**：
- `device` → 设备计数 +1，client 计数不动；达阈值 → QUARANTINED
- `client` → client 计数 +1，设备计数不动，设备回到 IDLE
- `none` → 两个计数器都不动，设备回到 IDLE
- `ok` → 两个计数器都清零
- 幂等：重复释放、非持有者释放不改任何计数
- 旧语义兼容：`FailScope` 为空且 `InfraFail=true` → 等价于 `device`

**飞书输出**：`status` / `devices` 的回复文本包含 client 计数。

## 10. 验收标准

- Runtime 自身故障（`check lease` / `load result`）不再改变任何计数器（有测试）
- client 级失败只增 `clients.fail_streak`，设备计数不变（有测试）
- 终态成功类清零两个计数器（有测试）
- 在途 workflow 重放不因本次改动失败（有测试或明确的版本分支覆盖）
- `device_fail_streak` 恒为 0 这一事实在代码注释与 `device-test-sequence.md` 差距 #10 中写明
- 飞书 `status`/`devices` 可看到 client 计数
