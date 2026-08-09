# 设备级归因信号源设计(差距 #10 收口)

2026-08-09。目标:让 `device_fail_streak` 第一次拥有真实信号,使
「设备连续 3 次故障 → QUARANTINED」(CLAUDE.md §9/§10)从死代码变成生效的安全网。

> 2026-08-09 评审后修订:§5 判定规则按调用层级重写(初稿的「ADB 返回 error →
> device」会误隔离);新增 §6 主失败约束与 Runtime 纵深校验;§9 通知改走既有
> outbox(初稿的迁移 bool 会丢通知)。

## 1. 问题

隔离机制**整条链路早已建好,只差信号**:

- `ReleaseDevice(scope=device)` → `fail_streak++`,达阈值置 `QUARANTINED`
  (`store/devices.go:199-205`、`postgres_devices.go:289-293`)
- `QUARANTINED` 永不可租(`devices.go:219`)
- 阈值 3 次可配(`Config.QuarantineAfter`)

但 `failScope()` 里通往 `FailScopeDevice` 的分支**恒不可达**
(`devicetest.go:507`,代码注释已自认是保留位),因为无人产出 `rules.CategoryDevice`。

### 比"没有信号"更糟的现状

真实行为不是"设备故障没人统计",而是**记错了账**:

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
- client 侧自动处置(见 §8 保留的盲区)
- 隔离后的自动恢复(维持现状:仅人工 `unquarantine` 指令解除)

## 3. 核心判断:属性不符不是设备的错

| 失败 | 真实原因 | scope |
|---|---|---|
| 已定位 transport 后,针对它的调用失败 | 设备掉线、USB 线/hub、板子挂了 | `device` |
| df 成功但空间不足 | 设备当下状态(清理或换板即恢复) | `device` |
| `abi mismatch` / `soc mismatch` | **任务派错了板**,或服务端配置漂移 | `none` |

这条区分是本设计的关键。2026-08-08 的 A1 审计正是活例:SoC 别名表失效时,
所有 QCM6125 任务都会 `soc mismatch`。若计入隔离,**第 3 次就会隔离掉一块
完全健康的板**,而真正的故障(服务端配置)被掩盖——安全网变成故障放大器。

同理,`none` 意味着配置类连续失败**不触发任何自动处置**,只能靠人从卡片和
Grafana 看出来。这是刻意选择:自动处置的前提是归因可靠。

## 4. 契约变更

`contracts/result.schema.json` 顶层加两个**可选**字段(§13:只加不删):

```json
"failure_scope": {
  "enum": ["device", "client", "none"],
  "description": "导致最终非 PASSED 结局的**主失败**归因。字段缺省 = 未归因,Runtime 回落既有 category 映射。旁路/可选/清理失败一律不填(见 §6)。"
},
"failure_stage": {
  "type": "string",
  "enum": ["resolve", "precheck", "download", "unpack", "deploy", "run", "collect"],
  "description": "主失败发生的阶段,供通知与排障展示。不参与任何判定。"
}
```

不设 `ok`:成功由 verdict 表达,Agent 只在失败时归因。

`failure_stage` 是枚举而非自由文本:通知要展示"为什么隔离",而
`rules.go:108` 只能给出泛化的 `client-side pipeline failure`;用枚举避免把
错误文本(可能含路径、序列号)带进飞书,也避免下游对文本做匹配。

同步三处 embed 副本(`agent/internal/reporter/`、`runtime/internal/callbacks/`),
三处均已有 `TestEmbeddedSchemaMatchesContract` 防漂移(2026-08-08 A8 补齐,
callbacks 侧在独立的 `schema_drift_test.go`)。

## 5. Agent 侧:按调用层级归因

初稿写的「ADB 调用返回 error → device」是错的,会误隔离。`adb.ExecRunner.Run`
(`adb.go:223-244`)返回 error 至少有三种来源,其中两种与设备无关:

| error 来源 | 实际含义 | 正确 scope |
|---|---|---|
| `exec` 启动失败(`default` 分支) | adb 可执行文件缺失/损坏、私有 server 起不来 | `client` |
| `ctx.Err()` | 超时或取消 | `none` |
| 已定位 transport 后该 transport 调用失败 | 设备证据 | `device` |

### 5.1 判定规则

按**调用层级 + 因果性**分类,而非按"是否返回 error":

| 层级 | scope | 说明 |
|---|---|---|
| 本地进程启动、私有 adb server 故障 | `client` | 与任何具体设备无关 |
| 全局 `adb devices`(不带 `-s`) | `client` | 是 server/宿主机能力,不是某台设备 |
| **已成功定位目标 transport 之后**,针对该 transport 的强设备证据 | `device` | 唯一产出 device 的来源 |
| 属性读取成功、比较不符(`abi`/`soc` mismatch) | `none` | 派单/配置问题 |
| ctx 取消或超时 | `none` | 多义,不归任何一方 |
| 无法可靠区分 | `none` | **保守默认** |

"强设备证据"限定为:transport 已解析成功后,针对该 transport 的 adb 调用
出现传输层失败或非零退出(not found / offline / unauthorized 语义)。

### 5.2 必须先修的三处证据丢失

初稿断言"现有控制流本来已分开",**这不成立**。要落地上表,得先让错误来源
不被压平:

1. **`adb.ExecRunner.Run` 需区分错误种类**(`adb.go:223`)。
   目前 `exec` 启动失败与其它错误都包成同一个 `fmt.Errorf`,调用方无从分辨。
   改为返回带类型的错误(如 `*adb.LaunchError`)或在 `Result` 上带一个
   `Kind` 字段;`ctx.Err()` 路径保持可用 `errors.Is(err, context.Canceled)` 判别。

2. **`resolveTransport` 未检查 `adb devices` 的退出码**(`executor.go:271-274`)。
   只判了 `err != nil`,`ExitCode != 0` 时会拿着空/残缺 stdout 继续,最终落到
   `device %q not found via adb` —— 一个 **server 级故障被伪装成设备不存在**。
   须显式检查退出码并归 `client`。
   同一函数内层探测 `ro.serialno` 的错误也被 `if err == nil &&` 静默吞掉,
   同样需要保留。

3. **`ProbeAndroidSOCChain` 静默丢弃全部 getprop 错误**(`soc_probe.go:40`,
   2026-08-08 A1 新增)。设备在 ABI 检查之后掉线时,四次 getprop 全部失败 →
   返回空链 → 上层报 `soc mismatch` → 按 §5.1 归 `none`。
   **真设备故障被误判成配置问题**,方向恰好与 A1 相反但同样有害。
   须改为 `([]string, error)`,把传输层失败与"属性不存在/无效"分开。

这三处都是既有的证据丢失,不修则本设计的判定表落不了地。

### 5.3 落点

`executor.Summary` 加 `FailureScope` 与 `FailureStage`,由**主失败站点**赋值
(见 §6);`reporter` 写进 result.json。

## 6. 主失败约束与 Runtime 纵深校验

初稿要求"collect 阶段 adb pull 失败 → device"是危险的:`collect` 是
**best-effort**(`executor.go:494` 注释:「单项失败只记日志不中断」),
一个可选附件拉取失败不影响测试结论。于是可以出现:

> 可选附件 pull 失败 → 测试本身 PASSED → `failure_scope=device`
> → 连续三次 → **隔离一台健康设备**

两道防线,缺一不可:

**防线 1(Agent)**:`failure_scope` 只描述**导致最终非 PASSED 结局的主失败**。
旁路失败、cleanup 失败、可选 collect 失败**一律不填**。实现上:只有走
`fail(err)` 返回路径的站点才赋值,best-effort 路径不碰这两个字段。

**防线 2(Runtime)**:最终 verdict 为 `PASSED` 时,**强制 `FailScopeOK`,
忽略任何上报的 scope**。这是纵深防御——即便 Agent 因 bug 或版本错配填了
device,也不会让成功的任务扣设备的分。

注意**不能**在 JSON Schema 里按 `status` 禁止该字段:`status=COMPLETED`
不等于成功(退出码非零、用例失败都可能是 COMPLETED),Schema 层无法表达
"最终 verdict"。校验必须在 Runtime 拿到 verdict 之后做。

## 7. Runtime 侧:消费

数据通路(沿用既有权威读,差距 #2):

```
result.json → callbacks(schema 校验)→ results 表
            → LoadResult 活动 → TaskResultSignal.FailureScope → failScope()
```

改动点:

1. `TaskResultSignal` 加 `FailureScope` / `FailureStage`;callbacks 持久化,
   `LoadResult` 回读
2. `failScope()` 在 **`siteTerminal` 分支**最前面加:
   ```
   若 verdict == PASSED            → FailScopeOK        (§6 防线 2,优先级最高)
   否则若 reportedScope 非空        → 直接采用
   否则                            → 落到现有 category 映射(零行为变化)
   ```

**只在 `siteTerminal` 采信**。`siteDispatchFailed` / `siteLeaseExpired` /
`siteHardDeadline` 等站点一律不变——见 §8。

`rules.Decide` 与 `Category` 完全不动:verdict 仍按今天的规则算,新字段只影响
**归因(scope)**,不影响**判定(verdict)**。这是不需要 rules v2 的原因。

## 8. 安全边界(不变量)

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
- 配置类失败(`none`)与取消(`none`)不驱动任何自动处置

## 9. 隔离生效与通知

计数与置位是现成的,**新增的是可见性**:设备被自动隔离后必须有人知道,
否则一块板从池子里消失且无声无息。

### 9.1 为什么不用"迁移 bool + 直接发通知"

初稿让 `ReleaseDevice` 返回 `bool` 再由 activity 发通知。有两个缺陷:

- **数据不够**:通知需要 client、实际 streak、失败阶段,而 `ReleaseRequest`
  (`devicetest.go:267`)只有 device/task/scope,bool 也带不回这些
- **有丢通知窗口**:隔离已提交、进程在发通知前崩溃 → activity 重试时
  `ReleaseDevice` 走幂等早返回(`row.Released` 已为 true),bool 变成 false
  → **永远不再通知**。飞书临时失败同理

### 9.2 采用:隔离事务内写 outbox(at-least-once)

既有 `outbox` 表是通用的(`schema.sql:182`,`aggregate_type` 注释已写
"task / device / ..."),Relay 按 `event_type` 分派。复用它即可,不必新建机制,
也与原则 3(单事务 + 独立投递)一致。

- `ReleaseDevice` 在**置 QUARANTINED 的同一事务内**插入 outbox 行:
  - `aggregate_type = "device"`,`aggregate_id = <device_id>`
  - `event_type = "device-quarantined"`
  - `event_key = "<device_id>:quarantined:<fail_streak>"` —— 幂等键。
    带上 streak,使"隔离→解除→再次隔离"能产生新事件,而同一次隔离的重试不会重复发
  - `payload` = `{device_id, client_id, serial, display_name, fail_streak, task_id, failure_stage}`
- Relay 的 `deliver` 增加该 `event_type` 分支,调用飞书通知;失败按既有重试与
  `outbox_backlog` 监控走
- 同时写 `audit_log`(`actor="activity:release_device"`,
  `action="device_quarantined"`,`target=device_id`),与现有四类动作同款

**保证等级:at-least-once**。飞书可能收到重复通知(Relay 重试),但绝不丢。
`event_key` 保证同一次隔离只产生一个事件行。

状态迁移是 **BUSY → QUARANTINED**:释放发生时设备仍持有租约(BUSY),
`ReleaseDevice` 要么置 QUARANTINED、要么置 IDLE(`devices.go:199-213`)。

## 10. 版本兼容

字段可选 + `result.schema.json` 顶层未声明 `additionalProperties: false`,
两个方向都安全:

| 组合 | 行为 |
|---|---|
| 旧 Agent + 新 Runtime | 字段缺省 → 回落既有 category 映射 = 今天的行为 |
| 新 Agent + 旧 Runtime | 多余字段被 schema 接受并忽略 = 今天的行为 |

无需版本协商,可分别滚动升级。

## 11. 测试策略

- **Agent(表驱动)**:每个失败站点 → 期望 (scope, stage)。必须含:
  - `soc mismatch → none`(§3 核心区分)
  - 已定位 transport 后掉线 → `device`
  - **adb 可执行文件起不来 → `client`**(§5.2 第 1 处)
  - **`adb devices` 非零退出 → `client`**,不得落成 device(§5.2 第 2 处)
  - **ABI 检查后掉线导致空探测链 → `device`**,不得落成 `soc mismatch/none`
    (§5.2 第 3 处)
  - ctx 取消 → `none`
- **契约**:新增 valid/invalid 例子(合法枚举、非法枚举被拒)
- **Runtime(扩表)**:
  - `failScope()` 加 reported-scope 行
  - **PASSED + reported device → `FailScopeOK`,计数清零不隔离**(§6 防线 2)
  - `siteLeaseExpired` 等沉默站点即使 result 带 device 也不采信
- **Store**:连续 3 次 device → QUARANTINED 且同事务写出 outbox 行;
  中间成功一次 → 计数清零不隔离;重复释放不产生第二行 outbox(`event_key` 幂等)
- **Relay**:`device-quarantined` 事件投递成功/失败重试;未知 event_type 不回归
- **故障注入(端到端)**:
  - 设备不可达 3 次 → QUARANTINED + 通知发出 + audit 落库
  - `soc mismatch` 3 次 → **不隔离**
  - PASSED 但 collect 拉取失败 3 次 → **不隔离**(§6 回归护栏)

最后两条是本设计最重要的测试:它们锁住"配置错误"和"旁路失败"都不许误伤好板。

## 12. 实施顺序

1. **证据保真**(§5.2 三处):`adb.Run` 错误分类、`resolveTransport` 查退出码、
   `ProbeAndroidSOCChain` 返回 error。**这一步不动归因逻辑,可独立合入并单测**
2. 契约两字段 + 三处 embed 同步 + 契约正反例
3. Agent:`Summary.FailureScope/FailureStage` + 各主失败站点赋值 + 表驱动测试
4. Runtime:`TaskResultSignal` 两字段 + callbacks 持久化 + `LoadResult` 回读
5. `failScope()`:PASSED 强制 OK → 采信 reported scope → 回落既有映射;扩表测试
6. `ReleaseDevice` 同事务写 outbox + audit;Relay 增加 `device-quarantined` 分支
7. 端到端故障注入三例
8. 更新 CLAUDE.md §9/§10 与 `docs/device-test-sequence.md` 差距 #10
   (删掉"暂不触发"的说明,它们将不再成立)

第 1 步先行的理由:它修的是既有缺陷(错误来源被压平),本身就有价值,
且后续判定表完全依赖它。第 8 步不能漏——CLAUDE.md 有两处、时序图有一处
写着"当前无信号源,暂不触发",本设计落地后这些描述会变成错的。
