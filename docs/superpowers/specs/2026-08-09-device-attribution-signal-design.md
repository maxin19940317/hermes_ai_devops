# 设备级归因信号源设计(差距 #10 收口)

2026-08-09。目标:让 `device_fail_streak` 第一次拥有真实信号,使
「设备连续 3 次故障 → QUARANTINED」(CLAUDE.md §9/§10)从死代码变成生效的安全网。

## 1. 问题

隔离机制**整条链路早已建好,只差信号**:

- `ReleaseDevice(scope=device)` → `fail_streak++`,达阈值置 `QUARANTINED`
  (`store/devices.go:199-205`、`postgres_devices.go:289-293`)
- `QUARANTINED` 永不可租(`devices.go:219`)
- 阈值 3 次可配(`Config.QuarantineAfter`)

但 `failScope()` 里通往 `FailScopeDevice` 的分支**恒不可达**
(`devicetest.go:507`,代码注释已自认是保留位),因为无人产出 `rules.CategoryDevice`。

### 比"没有信号"更糟的现状

排查中发现真实行为不是"设备故障没人统计",而是**记错了账**:

`decideV1` 在 `rules.go:108` 对 `Status == "FAILED"` 提前返回 `CategoryInfra`,
**早于 112 行的签名判定**。而预检失败、adb 寻址不到、push 失败全部产出
`Status=FAILED`。经 `failScope()` 的 `CategoryInfra + FAILED` 分支,这些
**全部归到 client 头上**。

结论:一块坏板今天不仅不会被隔离,还在持续抬高它所在 client 的失败计数。

这也证伪了 `2026-07-29-fail-streak-attribution-design.md` §7 的原推荐路径
(「在 variants.yaml 声明 `classify: DEVICE` 签名」)——`Status=FAILED` 时
签名根本不被评估,该路径对主要设备故障无效。

## 2. 目标与非目标

**目标**

- 设备故障(预检期的设备不可达 + 运行中的设备故障)能被正确归因到 device
- 隔离阈值真实生效;隔离发生时有通知与审计
- 不引入 rules v2(`decideV1` 已冻结,`rules.go:95`)

**非目标**

- 飞书隔离/解除按钮(UI,单独一轮)
- client 侧自动处置(见 §7 保留的盲区)
- 隔离后的自动恢复(维持现状:仅人工 `unquarantine` 指令解除)

## 3. 核心判断:属性不符不是设备的错

预检失败**不能一律算设备的错**:

| 失败 | 真实原因 | scope |
|---|---|---|
| adb 寻址不到 / getprop 执行失败 | 设备掉线、USB 线/hub、板子挂了 | `device` |
| df 可读但空间不足 | 设备状态 | `device` |
| `abi mismatch` / `soc mismatch` | **任务派错了板**,或服务端配置漂移 | `none` |

这条区分是本设计的关键。2026-08-08 的 A1 审计正是活例:SoC 别名表失效时,
所有 QCM6125 任务都会 `soc mismatch`。若计入隔离,**第 3 次就会隔离掉一块
完全健康的板**,而真正的故障(服务端配置)被掩盖——安全网变成故障放大器。

同理,`none` 意味着配置类连续失败**不触发任何自动处置**,只能靠人从卡片和
Grafana 看出来。这是刻意选择:自动处置的前提是归因可靠。

## 4. 契约变更

`contracts/result.schema.json` 顶层加一个**可选**字段(§13:只加不删):

```json
"failure_scope": {
  "enum": ["device", "client", "none"],
  "description": "终态失败的归因。字段缺省 = 未归因,Runtime 回落既有 category 映射。成功时不填。"
}
```

不设 `ok`:成功由 `status=COMPLETED` 表达,Agent 只在失败时归因。

同步三处 embed 副本(`agent/internal/reporter/`、`runtime/internal/callbacks/`),
三处都已有 `TestEmbeddedSchemaMatchesContract` 防漂移(2026-08-08 A8 补齐)。

## 5. Agent 侧:判定规则

一条原则:**看失败发生在哪一层,而不是失败长什么样**。不做错误文本匹配
(文案一改归因就失效)。

`executor.Summary` 加 `FailureScope string`,各失败站点显式赋值:

判据只有一条,**不看错误文本**:

> ADB 调用**返回了 error** → `device`;
> ADB 调用成功、是**比较结果**不满足 → `none`;
> 根本没碰设备(纯本地失败)→ `client`。

| 站点 | 判据 | scope |
|---|---|---|
| `resolveTransport` 解析不到 | 返回 error | `device` |
| `precheckAndroid` 的 `getprop()` 返回 error | 返回 error | `device` |
| `precheckLinux` 同类路径 | 返回 error | `device` |
| `abi mismatch` | getprop 成功,比较不等 | `none` |
| `soc mismatch` | 探测链成功,比较不中 | `none` |
| 空间不足 | `df` 成功,数值不够 | `device` |
| 下载 / sha256 / 解包 / manifest schema | 未触碰设备 | `client` |
| deploy 阶段 adb push 失败 | 返回 error | `device` |
| collect 阶段 adb pull 失败 | 返回 error | `device` |

这条判据能直接落到现有代码:`getprop()`(`executor.go:313-323`)对
`Runner.Run` 报错**和** `ExitCode != 0` 都已经统一返回 error,不解析 stderr;
而 `abi != m.Requirements.ABI` 是纯比较分支。两类失败在代码里本来就是分开的
控制流,不需要新增探测逻辑,也不需要任何字符串匹配。

注:「空间不足」判 `device` 而非 `none`,是因为它反映的是**设备当下的状态**
(磁盘被占满),不是任务与设备的属性错配——清一次或换板即可恢复。

`reporter` 把 `Summary.FailureScope` 写进 result.json。

## 6. Runtime 侧:消费

数据通路(沿用既有权威读,差距 #2):

```
result.json → callbacks(schema 校验)→ results 表
            → LoadResult 活动 → TaskResultSignal.FailureScope → failScope()
```

改动点:

1. `TaskResultSignal` 加 `FailureScope string`;callbacks 持久化,`LoadResult` 回读
2. `failScope()` 在 **`siteTerminal` 分支**最前面加一条:
   ```
   若 reportedScope 非空 → 直接采用(device/client/none)
   否则 → 落到现有 category 映射(零行为变化)
   ```

**只在 `siteTerminal` 采信**。`siteDispatchFailed` / `siteLeaseExpired` /
`siteHardDeadline` 等站点一律不变——见 §7。

`rules.Decide` 与 `Category` 完全不动:verdict 仍按今天的规则算,新字段只影响
**归因(scope)**,不影响**判定(verdict)**。这是不需要 rules v2 的原因。

## 7. 安全边界(不变量)

**归因只能来自明确信号,不能来自沉默。**

租约过期、派单失败、心跳丢失都属于"沉默",它们是多义的:设备死了?Agent 死了?
网络断了?Runtime 自己挂了?`devicetest.go:493` 的注释记录了这个盲区——
callbacks 进程宕机 ≥120s 时,全 fleet 租约同时过期,会把 Runtime 自身故障
记成 client 失联。

因此:

- 沉默类站点**永不产出 `device`**。坏板必须由 Agent **主动报告**才计数
- 该盲区继续存在于 client 侧。可接受,因为 `clientFailStreak` 只被写入和展示
  (`feishucmd/executor.go:1449`),**不驱动任何行为**——本设计也不给它加行为
- 若将来要对 client 做自动处置,必须先解决该盲区,否则 Runtime 重启一次
  就会停掉整个 fleet

其余不变量:

- 阈值维持 3 次连续(§10),成功即清零(现有实现)
- `QUARANTINED` 仅人工 `unquarantine` 解除,本轮不加自动恢复
- 配置类失败(`none`)不驱动任何自动处置

## 8. 隔离生效与通知

计数与置位是现成的,**唯一新增是可见性**:设备被自动隔离后必须有人知道,
否则一块板从池子里消失且无声无息。

- `ReleaseDevice` 返回 `quarantined bool`(是否**本次**发生了 IDLE→QUARANTINED 迁移)
- activity 在该迁移发生时:
  - 写 `audit_log`(`actor="activity:release_device"`,`action="device_quarantined"`,
    `target=device_id`),与现有四类动作同款 fire-and-forget
  - 经既有 `Acts.Notify`(`activity/notify.go`)发一条飞书纯文本,内容为
    设备 ID、client、连续失败次数、最后一次失败原因

只在**迁移瞬间**通知,不在已隔离状态的后续释放上重复通知。

## 9. 版本兼容

字段可选 + `result.schema.json` 顶层未声明 `additionalProperties: false`,
两个方向都安全:

| 组合 | 行为 |
|---|---|
| 旧 Agent + 新 Runtime | 字段缺省 → 回落既有 category 映射 = 今天的行为 |
| 新 Agent + 旧 Runtime | 多余字段被 schema 接受并忽略 = 今天的行为 |

无需版本协商,可分别滚动升级。

## 10. 测试策略

- **Agent(表驱动)**:每个失败站点 → 期望 scope。必须含
  `soc mismatch → none` 与 `设备不可达 → device` 两行,它们是 §3 的核心区分
- **契约**:新增 valid/invalid 例子(合法枚举值、非法值被拒)
- **Runtime(扩表)**:`failScope()` 现有表驱动测试加 reported-scope 行;
  另加一组断言:`siteLeaseExpired` 等沉默站点**即使 result 里带 device 也不采信**
- **Store**:连续 3 次 device → QUARANTINED;中间成功一次 → 计数清零不隔离
- **故障注入(端到端)**:
  - 设备不可达 3 次 → QUARANTINED + 通知发出 + audit 落库
  - `soc mismatch` 3 次 → **不隔离**(§3 的回归护栏)

最后一条是本设计最重要的测试:它锁住"配置错误不许误伤好板"。

## 11. 实施顺序

1. 契约字段 + 三处 embed 同步 + 契约正反例
2. Agent:`Summary.FailureScope` + 各站点赋值 + 表驱动测试
3. Runtime:`TaskResultSignal` 字段 + callbacks 持久化 + `LoadResult` 回读
4. `failScope()` 采信 reported scope(仅 siteTerminal)+ 扩表测试
5. `ReleaseDevice` 返回迁移标志 + audit + 通知
6. 端到端故障注入两例
7. 更新 CLAUDE.md §9/§10 与 `docs/device-test-sequence.md` 差距 #10
   (删掉"暂不触发"的说明,它们将不再成立)

第 7 步不能漏:CLAUDE.md 有两处、时序图有一处写着"当前无信号源,暂不触发",
本设计落地后这些描述会变成错的。
