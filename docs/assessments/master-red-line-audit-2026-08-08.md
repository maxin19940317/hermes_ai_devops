# master 分支红线通审 — 问题清单(2026-08-08)

审计基准:`335adb2`(与 `origin/master` 一致,**无已跟踪文件修改**;
工作区另有若干未跟踪目录,见 A13,以及本文件本身)。
审计口径:CLAUDE.md §3 硬性边界 / §9 状态模型 / §14 红线,外加一轮针对
`66148e0..HEAD`(129 commits)的正确性 review。

`runtime` 与 `agent` 两个 Go module 全部 `go build` + `go test` 通过;
Python 侧 65 passed / 10 error(见 A11)。

**§14 八条红线在结构上全部成立**,下列问题分两类:一类是红线的执行边界有
缺口(A2),一类是红线之外的正确性/契约一致性问题。

---

## 优先级总览

| ID | 级别 | 一句话 | 主要文件 |
|---|---|---|---|
| A1 | 待定级 | 代号别名表在探测链首跳非代号时失效(条件式,需实机定级) | `agent/internal/executor/executor.go`、`agent/internal/reporter/probe.go` |
| A2 | P0 | Manifest `deploy.env` 键无约束 → 设备端 shell 注入 | `contracts/manifest.schema.json`、`agent/internal/adb/adb.go` |
| A3 | P1 | `dev` 版本被最低版本门禁拒绝,与 CLAUDE.md 明文冲突 | `runtime/internal/activity/version.go` |
| A4a | P1 | 取消路径写入枚举外的 verdict `FAILED`(确定 bug) | `runtime/internal/workflow/devicetest.go` |
| A4b | 待决策 | 取消变体的卡片配色与 rerun 候选语义(产品决策,勿顺手改) | 同上 + `runtime/internal/feishucmd/executor.go` |
| A5 | P1 | `devices` 卡片成功后多发一条空文本消息 | `runtime/internal/feishucmd/executor.go` |
| A6 | P1 | 飞书「重试」按钮零记录;`ignore` 已有记录也不含操作者身份;且规范自身冲突需先裁定 | `runtime/internal/feishucmd/executor.go`、CLAUDE.md |
| A7 | P1 | `saveDecision` 靠类型断言降级 + godoc 声称的幂等不存在 | `runtime/internal/feishucmd/executor.go`、`runtime/internal/store/postgres_decisions.go` |
| A8 | P2 | `callbacks/result.schema.json` 缺防漂移测试 | `runtime/internal/callbacks/` |
| A9 | P2 | metrics 卡片缺 `- ` 前缀,不渲染为列表 | `runtime/internal/workflow/devicetest.go` |
| A10 | P2 | `SyncWorkflowRuns` 的 `a.HTTP` 无 nil 兜底 | `runtime/internal/activity/sync_workflow_runs.go` |
| A11 | P2 | mcp_bridge 红线断言从未执行:依赖缺失未锁版本 + 测试 mock 打错目标 + 无 CI | `hermes/mcp_bridge/`、仓库根 |
| A12 | P3 | mcp_bridge 两处文档与代码不符:默认端点 + mTLS「三件套」实为两项 | `hermes/mcp_bridge/mcp_bridge.py` |
| A13 | P2 | `scripts/lint.sh` 当前跑不起来(go/gofmt 不在 PATH);363MB 未跟踪残留 + `git gc` 停摆 | 仓库根、`scripts/lint.sh` |

表按 ID 排列(ID 是派工引用标识,不要重编号),不按优先级。建议处理顺序:

1. **先修门禁**:A13 第 1 步(配 PATH,让 `scripts/lint.sh` 能跑)——
   否则后面每一条改动都没有本地验证手段
2. **需要决策、不需要写码的三条先拍板**:A3(dev 放行 vs 改文档)、
   A4b(配色与 rerun 语义)、A6(规范内部冲突如何裁定)。这三条卡在决策上,
   早拍板早并行
3. **A1 先做实机验证定级**,若正在发生则插到最前;未发生则按 P2 排,
   同时可先用零改码的别名绕过解阻塞
4. **确定 bug 按 P0→P2 推进**:A2 → A4a / A5 / A7(+A6 合并) → A8~A11 → A12
5. A13 剩余的清理动作(删目录、恢复 gc)放最后,且需人工确认

---

## A1 — [待定级] SoC 别名查找只作用于探测链的首个命中值

> **定级说明**:这是一个成立的设计缺口,但它是**条件触发**而非普遍故障,
> 且触发所需的实机属性值与生产环境 `DEVICE_SOC_ALIASES` 实际配置均未确认。
> 请先按「如何确认是否正在发生」一节做实机验证,再定级排期。

### 位置

- `agent/internal/executor/executor.go:345`(部署预检)
- `agent/internal/reporter/probe.go:123`(心跳设备注册)
- 探测链定义:`agent/internal/adb/soc_probe.go:20-23`

### 现状

`adb.ProbeAndroidSOC` 按 `ro.soc.model → ro.chipname → ro.board.platform →
ro.product.board` 顺序探测,返回**第一个**通过 `ValidSOC` 的值,后续属性不再读:

```go
func ProbeAndroidSOC(ctx context.Context, runner Runner, serial string) string {
	for _, prop := range socProbeChain {
		soc, err := getPropQuiet(ctx, runner, serial, prop)
		if err != nil || soc == "" { continue }
		if !ValidSOC(soc) { continue }
		return soc          // ← 首个命中即返回
	}
	return ""
}
```

两个调用点都只拿这**一个**返回值去查别名表:

- `executor.go:345` — `soc := adb.ProbeAndroidSOC(...)`,随后 `e.SOCAliases[soc]`
- `probe.go:123` — `soc := p.probeAndroidSOC(...)`,随后 `p.SOCAliases[soc]`

而现有两张别名表**当前**都以平台代号为键:

- Agent 侧:`agent/dist/start-agent.ps1:26` → `AGENT_SOC_ALIASES="trinket:QCM6125,idp:QCS6490"`
- 服务端:`runtime/cmd/worker/config.go:22` → 注释明写「键=代号,小写」;
  归一化发生在 `runtime/internal/callbacks/handler.go:204-205`

注意:「键必须是代号」只是**当前配置惯例**,不是机制限制。别名表技术上完全
可以用型号串作键(如 `SM6225:QCM6125`),这也是下面「绕过方案」的基础。

### 准确的触发条件

匹配逻辑在 `executor.go:346-368`,对每个 `want` 先做直接匹配、再查别名:

```go
for _, want := range m.Requirements.SOC {
    if soc != "" && strings.EqualFold(soc, want) { matched = soc; break }   // 先直接匹配
    if alias, ok := e.SOCAliases[soc]; ok { ... }                            // 再查别名
}
```

所以**不是**「暴露 `ro.soc.model` 就失效」。失效需要同时满足:

1. 探测链的**首个**有效属性值,既不直接匹配 manifest 的 `requirements.soc`,
   也没有在别名表里配键;**且**
2. 链上**靠后**的某个属性值,本来可以通过别名表匹配上

即:别名本可以救,但因为链在第一跳就返回并停止,别名没有机会参与。

反过来说,以下情况都**不会**出错:

- 首个属性值直接匹配 manifest 要求 → 直接匹配分支命中
- 首个属性值在别名表里配了键(哪怕它是型号串)→ 别名分支命中
- 板子根本不暴露 `ro.soc.model` / `ro.chipname` → 链自然走到代号,与旧行为一致

### 一旦触发的后果

预检与心跳共用同一条链(这是 2026-08-08 P1 修复的本意,见
`soc_probe.go:17-18`),所以两条路径表现一致——不会一边通一边不通:

- 预检失败:`soc mismatch: device soc="<首个值>", required one of [...]`
- 心跳以该值注册 → `SelectTestSpecs` / `HasCapableDevice` 找不到匹配设备
  → 该变体被静默判 `SKIPPED`(「无匹配设备」),不报错、不告警

第二条尤其难排查:没有任何错误,只是变体悄悄不跑了。

### 如何确认是否正在发生(定级前必做)

上一轮 review 断言目标 QCM6125 板报 `ro.soc.model=SM6225`,**本次审计无设备
可连,未证实**;服务端 `DEVICE_SOC_ALIASES` 实际值在 `/deploy/.env`
(已 gitignore),同样无法从仓库确认。定级前请在实机上跑:

```sh
adb -s <serial> shell getprop ro.soc.model
adb -s <serial> shell getprop ro.chipname
adb -s <serial> shell getprop ro.board.platform
adb -s <serial> shell getprop ro.product.board
```

再对照生产的 `AGENT_SOC_ALIASES` / `DEVICE_SOC_ALIASES` 与 manifest 的
`requirements.soc`,判断是否命中上面两个条件。

- **正在发生**(变体被误判 SKIPPED / 预检 soc mismatch)→ 按 P0 排
- **尚未发生** → 按 P2 排,当作加固做

### 有一个零改码的绕过方案

无论定级如何,眼下可以先在别名表里**补一条以首个属性值为键的映射**
(如 `SM6225:QCM6125`),Agent 侧与服务端各配一次即可恢复匹配。
这不修复设计缺口(下一块属性组合不同的板会再次踩到),但可以立刻解阻塞,
把代码改动排到正常节奏里。

### 期望行为

别名表与 manifest 的 SoC 匹配,应对探测链上**每一个**取到的值依次尝试,
任一命中即算匹配;而不是只对首个命中值尝试。

### 建议修法

把 `ProbeAndroidSOC` 改为返回候选列表(或新增 `ProbeAndroidSOCChain`),
调用点对每个候选依次做「直接匹配 → 别名匹配」。注意 `executor.go:352-366`
已有的脏 alias 值拆分逻辑(`;` `,` 空格分隔)需要保留。

心跳注册侧(`probe.go`)建议上报归一化后的型号,保持与预检同源。

### 验收

- 新增用例:设备同时暴露 `ro.soc.model=<型号>` 与 `ro.board.platform=trinket`,
  别名表只配 `trinket:QCM6125`,manifest 要求 `[QCM6125]` → 预检必须通过。
  现有 `TestPrecheckUsesRealSOCModelBeforePlatform` 只覆盖了直接命中,不覆盖此场景。
- 心跳侧对应用例:同样设备注册后,`SelectTestSpecs` 能选中 QCM6125 变体。
- 实机:板子接上后 `adb -s <serial> shell getprop ro.soc.model` 先确认实际值,
  再跑一次完整派单。

---

## A2 — [P0] Manifest `deploy.env` 键无约束,可注入设备端 shell

### 位置

- `contracts/manifest.schema.json` → `properties.deploy.properties.env`
- `agent/internal/manifest/manifest.go:110-117`(`ResolvedEnv`)
- `agent/internal/adb/adb.go:183-199`(`ShellRunEntry`),关键在 **191 行**

### 现状

`ShellRunEntry` 把 env 拼进设备端 shell 命令时,**值加了引号,键是裸拼的**:

```go
for _, k := range keys {
	b.WriteString(" " + k + "=" + Quote(env[k]))   // ← k 未经任何转义/校验
}
```

链路上没有任何一环约束键:

- Schema 里 `deploy.env` 只有 `"additionalProperties": { "type": "string" }`
  —— 约束的是**值的类型**,没有 `propertyNames` 约束**键的字符集**
- `ResolvedEnv()` 只对值做 `{workdir}` 占位符替换,不校验键

### 后果

`manifest.yaml` 中一个形如下面的 env 键,即可在设备上执行任意命令,
绕过整套 ADB 白名单机制:

```yaml
deploy:
  env:
    "A=1; rm -rf /data/local/tmp; B": "x"
```

这直接击穿 §14 红线第 1 条(Client Agent 不提供任意 shell)与 §3.4
(ADB 操作走模板化白名单)。

### 这是漏网,不是全局疏忽

同一份 Schema 对其它进入 shell 的字段都做了约束,唯独 env 键漏掉:

| 字段 | 约束 | 在 shell 中的处理 |
|---|---|---|
| `deploy.files[].mode` | `^0[0-7]{3}$` | 裸拼(安全,字符集已限死) |
| `collect[]` | `^[A-Za-z0-9._*-][A-Za-z0-9._*/-]*$` | 裸拼(保留 glob 展开,安全) |
| `deploy.workdir` | `^/[A-Za-z0-9._/-]+$` | `Quote()` |
| `test.entry` / `test.args[]` | `^\./[A-Za-z0-9._/-]+$` / maxLength | `Quote()` |
| **`deploy.env` 的键** | **无** | **裸拼** ← 缺口 |

### 实际暴露面(如实说明)

manifest 由 `ci/gen_manifest.py` 在打包期从 `ci/variants.yaml` 渲染,
当前 `variants.yaml` 里的 env 键都是干净的(`LD_LIBRARY_PATH`、
`ADSP_LIBRARY_PATH`)。利用需要控制 `variants.yaml`、打包步骤,或向
Registry 投放构造过的包。整包 sha256 在派单载荷中校验,所以下载环节的
MITM 不构成路径。

因此这是**纵深防御缺口**,不是可远程触发的活漏洞。但 Schema 本身就是这条
红线的执行边界,边界上有洞就应当补。

### 建议修法

`contracts/manifest.schema.json` 的 `deploy.env` 增加:

```json
"propertyNames": { "pattern": "^[A-Za-z_][A-Za-z0-9_]*$" }
```

注意 CLAUDE.md §13 的契约变更规则:只加字段不删字段。加约束会让原本合法的
畸形 manifest 变非法,属于收紧——需确认 `variants.yaml` 现有键全部符合
(已核对:符合)。同步更新 `agent/internal/manifest/manifest.schema.json`
的 embed 副本(有防漂移测试会挡住不同步)。

防御性起见,`ShellRunEntry` 里也可对键再做一次断言,不合法直接返回错误。

### 验收

- Schema 反例测试:非法 env 键的 manifest 必须校验失败。
- `ci/gen_manifest.py` 生成后的 Schema 校验必须仍然通过(12 个变体全过)。

---

## A3 — [P1] `dev` 版本被最低版本门禁拒绝,与 CLAUDE.md 明文冲突

### 位置

- `runtime/internal/activity/version.go:9-17`(`compareVersion`)
- `runtime/internal/activity/acts.go:141-155`(`AcquireDevice` 里的门禁)
- `agent/internal/version/version.go`(未打 ldflags 时 `Version = "dev"`)

### 现状

```go
// Handles "dev" as always < any formal version.
func compareVersion(a, b string) int {
	if a == b { return 0 }
	if a == "dev" { return -1 }      // ← dev 恒小于任何正式版本
	...
}
```

门禁对 `< 0` 直接拒绝并释放租约:

```go
if compareVersion(cv, a.Cfg.MinAgentVersion) < 0 {
	_ = a.Store.ReleaseDevice(...)
	return nil, fmt.Errorf("acquire device: client %s version %s below minimum %s", ...)
}
```

### 与契约的冲突

CLAUDE.md §12 Phase 3 明写:

> Agent 版本上报 + 最低版本门禁✅(ldflags 注入版本,`MIN_AGENT_VERSION` 空=不启用,**dev 永远放行**)

代码里 `MIN_AGENT_VERSION` 为空时确实不启用(`acts.go:141` 的外层 if),
但「dev 永远放行」这半句**没有实现**,`compareVersion` 反而把 dev 定义成最小。

### 后果

部署设了 `MIN_AGENT_VERSION`、而 Agent 未打 ldflags(或用
`git describe --always` 在无 tag 仓库上得到裸 commit hash,会被解析成 `0.0.0`)时:
每次 `AcquireDevice` 都先占租约再释放,任务重试耗尽后全部落 `INFRA_ERROR`。
现象是「设备明明在线,所有任务却报基础设施错误」,排查成本高。

### 期望行为

二选一,**需要人决策**:

- **方案 A(按现有文档)**:`compareVersion` 或门禁处对 `cv == "dev"` 短路放行。
  好处是开发/调试机不被门禁误伤,符合 CLAUDE.md 现文。
- **方案 B(按现有代码)**:保持 dev 被拒,改 CLAUDE.md §12 的措辞。
  好处是生产环境不会混进未标版本的 Agent。

倾向 A:文档是评审定稿的契约,代码应向契约靠拢;真要禁 dev 应该是显式配置项
而非隐式语义。但这条由项目决定。

### 验收

无论选哪个方案,补一条表驱动用例覆盖 `cv="dev"` + `MinAgentVersion="0.1.0"`
的组合,把选定语义锁死。

---

## A4 — 取消路径写入枚举外的 verdict `FAILED`

> 本条拆成两件事:**A4a** 是枚举违规,确定 bug,可直接修(P1);
> **A4b** 是卡片配色与 rerun 候选语义,是产品决策,**改 A4a 不会顺带解决它,
> 也不要顺手一起改**。

### 位置

`runtime/internal/workflow/devicetest.go:1448-1454`(`runParallelSpecs`)

### 现状

```go
if err := workflow.Await(ctx, func() bool { return sums[i] != nil }); err != nil || sums[i] == nil {
	out.Tasks = append(out.Tasks, TaskSummary{
		TestID: specs[i].TestID, Variant: specs[i].Variant,
		Verdict: "FAILED", Reason: "workflow canceled",
	})
	continue
}
```

填 nil 摘要避免 panic 的意图是对的(注释已说明),但选的 verdict 值不对。

### 为什么是错的

`FAILED` 不在 verdict 枚举内。`runtime/internal/rules/rules.go:68-72` 定义的是
`PASSED | TEST_FAILED | PERF_REGRESSION | INFRA_ERROR | INCONCLUSIVE`,
CLAUDE.md §9 另有明确规则:

> `CANCELED → INCONCLUSIVE`

### 下游行为(注意:以下多数不因改 verdict 值而改变)

`FAILED` 是枚举外的值,下游按「非 PASSED 即失败」处理:

1. `runtime/internal/feishucmd/executor.go:529-534` — `rerun` 只排除
   `PASSED`/`SKIPPED`,所以被取消的变体会出现在重试候选集里。
   **措辞更正**:这不是「自动重跑」——`rerun` 需要用户显式执行
   (指令或卡片按钮),取消的变体只是会**出现在候选集中**。
2. `cardHeaderTemplate`(`devicetest.go:1184-1190`)只放行 `PASSED`/`SKIPPED`,
   `INFRA_ERROR` 判橙,其余一律判红。
3. `SyncWorkflowRuns` 把 verdict 原样发到 workflow-assets 排行榜。

### 拆成两件事

**A4a — 枚举违规(确定 bug,可直接修)**

`Verdict: "FAILED"` → `Verdict: string(rules.VerdictInconclusive)`,
`Reason` 保留 `"workflow canceled"`。

依据明确无歧义:`FAILED` 不在枚举内,§9 明文规定 `CANCELED → INCONCLUSIVE`。
直接收益是排行榜不再收到枚举外的字符串,以及类型/契约层面的自洽。

**A4b — 卡片配色与 rerun 语义(产品决策,不要顺手改)**

改完 A4a 后,下面两条行为**依然如故**,因为它们判定的是「非 PASSED/SKIPPED」
而不是具体值:

- 被取消的变体在卡片上仍显示红色(`INCONCLUSIVE` 不在 `cardHeaderTemplate`
  的放行名单里,也不走 `INFRA_ERROR` 的橙色分支)
- 被取消的变体仍会进入 `rerun` 候选集

这两条要不要改,是独立的产品决策,各有取舍:

| 决策点 | 选项 A | 选项 B |
|---|---|---|
| 卡片配色 | `INCONCLUSIVE` 单独给中性色(灰/蓝),区分「没结论」与「失败了」 | 保持红色,宁可误报也不漏报 |
| rerun 候选 | 排除 `INCONCLUSIVE`(取消=人主动放弃) | 保留(活儿没干完,补跑是对的) |

**注意**:`INCONCLUSIVE` 不只来自取消——规则引擎在证据不足时也会判它。
所以按 verdict 值做特殊处理会**连带影响非取消场景**。若确实要区分「取消」
与「其它 INCONCLUSIVE」,更稳妥的做法是在 `TaskSummary` 上加一个显式的
取消标记字段,而不是重载 verdict 语义。

### 验收

**A4a**:故障注入测试——并发多变体运行中途取消 workflow,断言每个未完成
变体的 verdict 为 `INCONCLUSIVE`(**只断言这一条**)。
`runtime/internal/workflow/fault_injection_test.go` 已有取消场景可扩展。

**A4b**:决策落定后再补对应断言。在决策前不要写「不出现在 rerun 候选集中」
这类断言——那会把一个未决的产品选择固化成测试。

---

## A5 — [P1] `devices` 卡片成功后多发一条空文本消息

### 位置

- `runtime/internal/feishucmd/executor.go:444`(`devices` 卡片分支返回)
- `runtime/internal/feishucmd/executor.go:194`(`HandleMessage` 无条件回复)
- `runtime/internal/feishucmd/executor.go:311-319`(`reply`)

### 现状

卡片发送成功后返回空串,表示「已回过了」:

```go
if e.replyCard(ctx, card) {
	return "", nil // 卡片已发送,不重复文本
}
```

但 `HandleMessage` 不认这个约定,拿到什么都发:

```go
e.reply(ctx, prefix+reply)
```

而 `reply` 只挡 `Sender == nil`,不挡空内容:

```go
func (e *Executor) reply(ctx context.Context, text string) {
	if e.Sender == nil { return }
	if err := e.Sender.SendText(ctx, text); err != nil { ... }
}
```

### 后果

每次成功执行 `devices` 指令,卡片之后都会再发一条内容为空(或只有 prefix)的
消息。飞书要么渲染出一个空气泡,要么直接拒收、于是每次调用都记一条错误日志。

2026-08-08 新增的测试只覆盖了**卡片失败降级文本**的分支,没覆盖成功分支的这个副作用。

### 建议修法

在 `reply` 开头加空串短路,或更清晰地让 `execute` 返回一个「已回复」哨兵,
由 `HandleMessage` 判断后跳过。倾向前者(改动面小,且对所有指令一致生效),
但要确认没有哪个指令依赖「发一条只有 prefix 的消息」这一行为。

### 验收

用例:`devices` 走卡片成功路径时,`Sender.SendText` 的调用次数为 0。

---

## A6 — [P1] 卡片操作审计不完整:retry 零记录、ignore 无操作者身份

### 位置

`runtime/internal/feishucmd/executor.go:865`(`retry` 分支)

### 现状

同一个 `switch` 里,两个按钮的记录待遇不同:

- `ignore` 分支(871-895 行):查到真实 `task_id` → 写 `decisions` 表
  (`actor="human"`,output 存按钮载荷)→ 再记一条 `Info` 日志(890-894 行)
- `retry` 分支(856-869 行):`retryVariant` 起了新 workflow 后直接
  `return text, "success", nil` —— **既不写 `decisions`,也不写 `audit_log`,
  成功路径上连日志都没有**

listener 侧只在失败时记录,所以成功的重试在系统里不留任何痕迹。
`openID` 在上下文里现成可用(890 行 `ignore` 的日志用了它),却没有被使用。

**另有一处连带缺陷**:`ignore` 虽然写了 `decisions`,但写进 `Output` 的
`wf.ButtonValue` 只有 action / source_workflow_id / variant 三个字段,
**不含操作者身份**。所以严格说,当前**两个按钮的操作者都无法从库中追溯**,
区别只是 ignore 至少留了行、retry 连行都没有。修复范围应覆盖两者(详见修法)。

### 契约本身存在内部不一致(需先裁定)

这条不能直接照着「补齐审计」实现,因为 CLAUDE.md 自己就有冲突:

| 出处 | 说法 |
|---|---|
| §3.7 总则 | 「所有 Hermes/人工决策落 `decisions` 表;**所有操作**落 `audit_log`」 |
| §11 | 只明确写了「忽略记 decisions 表(actor="human")供审计」,**未提 retry** |
| §12 Phase 3 | audit_log 只列了 dispatched / device_leased / device_released / escalated **四类** action |

按总则,retry 两张表都该写;按 §11 与 Phase 3 的具体列举,现状(只有 ignore
写 decisions、audit_log 只有四类)恰好是符合的。

**所以第一步是裁定规范,而不是改代码。** 建议的裁定方向:
retry 是**触发真实执行**的人工决策,副作用比 ignore 大,没有理由记录反而更少
——应当补齐,同时把 §11 / Phase 3 的列举改为与 §3.7 总则一致(或明确写成
「以下为当前已实现范围,总则为目标态」)。但这需要项目拍板。

### 裁定为「补齐」后的修法

**decisions**:`retryVariant` 成功后补一条(`actor="human"`,output 含
`source_workflow_id` / `variant` / 新 workflow_id)。`task_id` 取法照抄
`ignore` 分支的 `LatestTaskIDForVariant`——注意 872-874 行注释记录的坑:
`decisions.task_id` 有 FK 指向 `tasks`,不能直接写 workflow_id。

但只写这些**仍然回答不了「谁触发了重试」**,还有两个前置改动:

**(a) 必须把 `open_id` 写进 output / audit payload。**
`actor="human"` 只是角色,不含身份。`openID` 是 `HandleCardAction` 的形参
(`executor.go:833`),在 retry 分支作用域内现成可用,补进去零成本。

⚠️ **同一缺陷在现有 `ignore` 分支里也存在**,不只是 retry 的问题:
`ignore` 写的 `Output` 是 `json.Marshal(value)`,而 `wf.ButtonValue`
(`devicetest.go:1046-1050`)只有 `Action` / `SourceWorkflowID` / `Variant`
三个字段,**同样不含任何身份信息**。所以现状是「谁忽略了什么」其实也查不到,
之前审计里「库里能查到谁忽略了什么」的说法过于乐观。
**两个分支应当一起补 `open_id`**,否则修完 retry 仍有一半审计是匿名的。

**(b) `retryVariant` 需要改返回值,不要从文本反解析。**
当前签名(`executor.go:785-787`)只返回展示文本:

```go
func (e *Executor) retryVariant(
	ctx context.Context, source *store.WorkflowRun, variant string,
) (string, error)
```

新 workflow ID 被拼进 `"已启动重试 %s: %s"` 里。要结构化记录就得反解析这个
字符串——脆弱且会随文案改动静默失效。应显式增加返回值(如
`(text string, newWorkflowID string, err error)`)。

同时注意该函数有**四种非错误业务结果**,其中三种根本没有启动新 workflow:

| 返回文本 | 是否启动了新 workflow |
|---|---|
| `变体 %s 的 artifact 不存在` | 否 |
| `重试正在进行中: %s` | 否(已有在跑) |
| `workflow 已存在: %s` | 否(Temporal 去重) |
| `已启动重试 %s: %s` | **是** |

(此外还有三类 `err != nil` 的错误返回:`ListArtifacts`、`NextWorkflowAttempt`、
`StartDeviceTest` 各一处。它们由调用方按错误处理,不在上表的业务结果之列。)

记录 decision 时必须区分这四种业务结果——否则会把「没跑成」也记成一次人工
重试决策,反而污染审计。建议只在真正启动的路径上记 `retry_triggered`,
其余路径若要留痕应使用不同的 action 值。

**audit_log**(改动稍大):`feishucmd` 的 `Store` 依赖里**目前没有 `WriteAudit`**,
需要先把它加进接口。参考 `runtime/internal/activity/acts.go:36` 已有的声明。
顺带一提:这正好和 A7 要做的「把 `SaveDecision` 提进 Store 接口」是同一处改动,
**建议 A6 与 A7 合并处理**,一次把 `feishucmd` 的 Store 依赖补全,避免第二次
用类型断言绕过。

### 验收

- 触发 retry 卡片操作后,`decisions` 表出现对应 `actor="human"` 行,
  且 `task_id` 能通过 FK 约束
- **该行的 output 中能读出 `open_id`**,即从库里可以回答「谁触发的」;
  `ignore` 分支同样满足
- 未真正启动 workflow 的三条路径(artifact 不存在 / 已在跑 / Temporal 去重)
  **不**产生 `retry_triggered` 记录
- 若裁定同时补 audit_log:对应 action 的行落库,且失败不阻断主链路
  (与现有 `writeAudit` 的 fire-and-forget 语义一致)

---

## A7 — [P1] `saveDecision` 靠类型断言降级 + godoc 声称的幂等不存在

### 位置

- `runtime/internal/feishucmd/executor.go:902-914`
- `runtime/internal/store/postgres_decisions.go:29-31`
- `runtime/internal/store/schema.sql:150-151`

### 问题一:类型断言降级为日志

```go
// Store 接口里没有 SaveDecision,需要通过类型断言;如果 store 不支持则降级记日志。
func (e *Executor) saveDecision(ctx context.Context, row wf.DecisionRow) error {
	type decisionSaver interface {
		SaveDecision(ctx context.Context, row wf.DecisionRow) error
	}
	if ds, ok := e.Store.(decisionSaver); ok {
		return ds.SaveDecision(ctx, row)
	}
	e.log().Warn().Str("task_id", row.TaskID).Msg("store does not support SaveDecision, card action unrecorded")
	return nil          // ← 审计丢失,但返回成功
}
```

§11 把 `decisions` 定为「一切裁决可回放」的依据,这里却把它降级成 best-effort:
换一个不实现该方法的 Store 实现,人工决策会静默丢失,调用方还收到 nil。

**修法**:把 `SaveDecision` 提进 `Executor` 依赖的 Store 接口,让缺失在
**编译期**暴露而不是运行时降级。`runtime/internal/activity/acts.go:24` 已经
在自己的接口里声明了这个方法,照搬即可。

### 问题二:godoc 声称的幂等不存在

```go
// saveDecision 落 decisions 表(actor="human" 的卡片操作)。
// 重复插入(相同 task_id+actor)静默成功(幂等)。   ← 假的
```

实际实现是一条无冲突处理的普通 INSERT:

```go
INSERT INTO decisions (task_id, actor, input_digest, model, prompt_version, output, evidence_snapshot_id)
```

而 `decisions` 表只有 `decision_id BIGSERIAL PRIMARY KEY`,**没有任何唯一约束**
(`schema.sql:150-151`)。同一张卡片点两次「忽略」会写入两行 `actor='human'`,
把 §11 指定为回放依据的审计轨迹撑胖。

**修法**:二选一——要么删掉这句 godoc(承认非幂等),要么真加
`UNIQUE (task_id, actor)` 加 `ON CONFLICT DO NOTHING`。

注意:加唯一约束会影响 `actor='rule'` / `actor='hermes'` 的写入——同一 task
的多次 attempt 或规则/LLM 双裁决可能合法地需要多行。**加约束前必须先确认
现有写入模式**,不要直接加。倾向先改 godoc(零风险),唯一约束单独评估。

---

## A8 — [P2] `callbacks/result.schema.json` 缺防漂移测试

### 位置

`runtime/internal/callbacks/handler.go:24`(`//go:embed result.schema.json`)

### 现状

仓库里共 7 处 `go:embed *.schema.json` 副本,其中 6 处都有
`TestEmbeddedSchemaMatchesContract` 之类的防漂移测试:

| embed 位置 | 防漂移测试 |
|---|---|
| `agent/internal/manifest/` | ✅ `manifest_test.go:13` |
| `agent/internal/server/`(dispatch) | ✅ `schema_test.go:44` |
| `agent/internal/reporter/`(result) | ✅ `result_test.go:22` |
| `runtime/internal/trigger/`(bundle) | ✅ `bundle_test.go:73` |
| `runtime/internal/evidence/` | ✅ `evidence_test.go:17` |
| `runtime/internal/hermesclient/`(plan/command/express) | ✅ `hermesclient_test.go:19,387,397` |
| **`runtime/internal/callbacks/`(result)** | **❌ 无** |

当前文件内容与 `contracts/result.schema.json` **完全一致**(已 diff 确认),
所以不是活故障。

### 风险

`contracts/result.schema.json` 演进时,Agent 侧(`reporter`,有防漂移测试)会
被强制同步,Runtime 侧(`callbacks`)不会——结果是**同一份契约在生产者和
消费者两端静默分叉**,且分叉方向恰好是 Runtime 用旧 Schema 校验新 result.json。

### 修法

照抄 `agent/internal/reporter/result_test.go:22` 的 `TestEmbeddedSchemaMatchesContract`,
读 `../../../contracts/result.schema.json` 与 embed 内容比对。

---

## A9 — [P2] metrics 卡片缺 `- ` 前缀,不渲染为列表

### 位置

- `runtime/internal/workflow/devicetest.go:546-559`(`formatMetricsCard`)
- `runtime/internal/workflow/devicetest.go:1283-1286`(元素构造)

### 现状

元素声明了 bullet 列表样式:

```go
md := CardElement{
	Tag: "markdown", Content: lines,
	ElementStyle: &CardElStyle{Display: "list", ListType: "bullet"},
}
```

但 `formatMetricsCard` 产出的行没有 `- ` 前缀:

```go
lines = append(lines, fmt.Sprintf("%s  **%.1fms**", escapeCardText(name), m[k]))
```

### 为什么这是 bug

同文件的 `cardReasonLines` 记录了一条实测验证过的飞书渲染规则:

> markdown 元素在 display=list 时,content 里必须**保留 `- ` 前缀**……
> 去掉前缀后飞书……只按普通文本逐行显示(实测 2026-08-06 r8)

所以 metrics 块的实际渲染是普通多行文本,而非 1279-1280 行注释所描述的
「每指标一行 markdown bullet 列表」。纯显示问题,不影响判定。

### 修法

`formatMetricsCard` 的两个 `fmt.Sprintf` 都加 `- ` 前缀。
注意 `escapeCardText` 只应作用于指标名,不要把前缀一起转义。

### 验收

现有卡片渲染测试里加断言:metrics 元素 content 的每一行都以 `- ` 开头。

---

## A10 — [P2] `SyncWorkflowRuns` 的 `a.HTTP` 无 nil 兜底

### 位置

`runtime/internal/activity/sync_workflow_runs.go:55`

### 现状

直接解引用:

```go
resp, err := a.HTTP.Do(httpReq)
```

而同一个 `Acts` 上的兄弟方法 `postEscalation`(`escalate.go:128-131`)有兜底:

```go
hc := a.HTTP
if hc == nil {
	hc = http.DefaultClient
}
resp, err := hc.Do(hr)
```

### 后果

生产装配路径(`runtime/cmd/worker/main.go:149`)会注入 HTTP,所以当前不是活故障。
但任何其它装配路径(测试夹具、未来新增的 cmd)漏注入就会 panic;
Temporal 随后把该 activity 重试到耗尽,而 `DeviceTestWorkflow` 正阻塞在运行
最末尾的 `.Get()` 上——整个 run 卡死在最后一步,现象很难归因。

### 修法

照抄 `escalate.go` 的兜底三行。或者更彻底:在 `Acts` 构造函数里统一兜底,
消除这类不对称。

---

## A11 — [P2] mcp_bridge 红线断言从未被执行过(三层原因)

### 现状

```
$ python -m pytest ci/tests hermes -q
65 passed, 2 warnings, 10 errors in 3.55s

ModuleNotFoundError: No module named 'mcp'   (hermes/mcp_bridge/mcp_bridge.py:30)
```

`hermes/mcp_bridge/test_mcp_bridge.py` 的 10 个用例全部 error,其中包括这条
**红线断言**:

```python
# 不允许出现任何直连 ADB 的任意命令工具
assert not t.startswith("adb_"), f"违规工具: {t}"
```

### 根因有三层,只补依赖不够

**第一层:依赖缺失且必须锁版本。**
`mcp` 不在任何 requirements 文件里(仓库只有 `contracts/tests/requirements.txt`)。
但直接 `pip install mcp` 装到当前的 **2.0.0 会更糟**——该版本已无
`mcp.server.fastmcp`,而 `mcp_bridge.py:30-31` 正是从这里导入:

```python
from mcp.server.fastmcp import FastMCP
from mcp.server.transport_security import TransportSecuritySettings
```

实测 `mcp==1.29.0` 可正常导入。**必须锁版本,不能只写 `mcp`。**

**第二层:测试 mock 打错了目标,装上依赖也不会绿。**
实测装好 `mcp==1.29.0` 后结果是 **7 failed, 3 passed**,而非全绿。

生产代码为支持 mTLS 已改用 `httpx.Client` 上下文管理器
(`mcp_bridge.py:82-83`,见 72 行注释「mTLS 证书(cert/verify)必须在 Client
级别配置,httpx.post 顶层不接受」):

```python
with httpx.Client(**client_kwargs) as client:
    resp = client.post(RUNTIME_CMD_API_URL, json=payload, headers=headers)
```

而测试仍在 monkeypatch **模块级函数** `httpx.post`(8 处:第 56、68、80、92、
104、119、129、140 行):

```python
monkeypatch.setattr("httpx.post", fake_post)
```

打上去的桩根本不在调用路径上,于是这 7 个用例会真的去连
`http://fake-runtime/`。剩下 3 个通过的是不走 HTTP 的用例
(工具白名单断言、未配置 token 的短路分支等)。

值得注意的是,测试文件自己的 docstring(第 3 行)写的是
「用 `httpx.MockTransport` 替换 Runtime 调用」——这正是配合 `httpx.Client`
的正确做法。**说明生产代码从 `httpx.post` 迁到 `httpx.Client` 时,测试没跟着改,
docstring 反而保留了本来的正确意图。**

**第三层:本仓库没有任何 CI**(无 `.gitlab-ci.yml`,无 `.github/`)。
`ci/gitlab-ci.example.yml` 是给业务仓库 `algo-super-sdk` 用的模板,
不跑本仓库的测试。所以上面两层问题没有任何机制会暴露出来。

### 后果

守卫 §3.1/§3.3/§14 边界的这条红线断言(`test_mcp_bridge.py:45`),
写了但从未被执行过。其余 Go 侧红线测试同样只靠人手动跑。

### 修法(三步缺一不可)

1. **锁定依赖**:建 `hermes/requirements.txt`(或根级 `requirements-dev.txt`),
   写 `mcp==1.29.0`(或其它经验证可用的 1.x 版本),连同 `httpx`、`fastapi`、
   `jsonschema`、`pytest` 一并收进去。升 2.x 需要单独评估
   `mcp.server.fastmcp` 的替代 API,不在本条范围内。
2. **改测试 mock**:8 处 `monkeypatch.setattr("httpx.post", ...)` 需要换掉。
   但**光创建一个 `MockTransport` 是进不了调用路径的**——生产代码在函数内部
   直接构造 client(`mcp_bridge.py:82`:`with httpx.Client(**client_kwargs) as client:`),
   没有任何参数能把 transport 传进去。必须先解决「怎么注入」,二选一:

   | 方案 | 做法 | 取舍 |
   |---|---|---|
   | **B1 生产代码开注入点** | 加一个可替换的 client/transport factory(模块级变量或函数参数),测试注入携带 `MockTransport` 的 client | 最干净,测试不碰私有构造;但要改生产代码,需确认 mTLS 参数仍在被测路径上 |
   | **B2 patch 构造器** | monkeypatch `httpx.Client`,让它返回一个**真实的** `httpx.Client(transport=MockTransport(handler), **kwargs)` | 不改生产代码;**关键是返回真 Client 而非 fake**,这样仍能断言传入的 `verify` / `cert` / `timeout` |

   ⚠️ **B2 有一个递归陷阱**:替换函数内部若直接再调 `httpx.Client(...)`,
   调到的是已被 patch 的自己 → 无限递归。**必须先把原始类存下来**:

   ```python
   real_client_cls = httpx.Client          # 先存,再 patch
   def fake_client(**kwargs):
       return real_client_cls(transport=httpx.MockTransport(handler), **kwargs)
   monkeypatch.setattr("httpx.Client", fake_client)
   ```

   **不要用纯 fake Client 对象替换**——那会把 mTLS 参数构造
   (`mcp_bridge.py:76-81`:`ssl_ctx` / `verify` / `cert` 的组装)整段旁路掉,
   等于放弃了对 Phase 3 mTLS 配置的验证。选 B2 时务必保留对 `client_kwargs`
   的断言。
3. **加最小 CI**:`go build ./... && go test ./...`(两个 module)
   \+ `pytest ci/tests contracts/tests hermes`。
   顺带把 `scripts/lint.sh` 纳进来(注意它当前跑不起来,见 A13)。

### 验收

`pytest hermes/mcp_bridge` 全绿(10 passed),且在干净环境里按
requirements 安装后可复现。**不要接受「skip 掉就算过」**——
`test_mcp_bridge.py:45` 那条红线断言必须真的执行。

另需至少一条用例断言:mTLS 的**两个**配置项
(`MTLS_CA_FILE` = 校验服务端的 CA 文件;`MTLS_CLIENT_CERT` = 客户端证书与私钥
的合体文件)都非空时,`verify`(ssl context)与 `cert` 被正确传入 client 构造
——防止改 mock 的过程中把这段逻辑悄悄旁路掉。

注:是**两个**配置项而非三个,见 `mcp_bridge.py:41-42`,判断条件
`if MTLS_CA_FILE and MTLS_CLIENT_CERT:`(第 77 行)也只检查这两个。
生产注释里「三件套」的说法不准确,见 A12。

---

## A12 — [P3] mcp_bridge 内两处文档与代码不符

同一文件 `hermes/mcp_bridge/mcp_bridge.py` 里的两处纯文档问题,一并修掉。

### A12a — docstring 默认端点与代码不符

- 第 20 行 docstring:`RUNTIME_CMD_API_URL  Runtime 受控接口,缺省 http://trigger:8090/api/v1/cmd`
- 第 36 行代码:`os.environ.get("RUNTIME_CMD_API_URL", "https://worker:8091/api/v1/cmd")`

协议、主机、端口三项全不一致。按代码为准修文档即可。

### A12b — mTLS 注释的「三件套」说法不准确(且自相矛盾)

第 40 行注释:

```python
# CA 证书用于校验服务端;cert 是客户端证书+私钥合体。三件套任一空 → 纯 HTTP。
```

同一句话内部就是矛盾的:前半句明说 cert 是「客户端证书+私钥**合体**」
(即两者在同一个文件里),后半句却称「三件套」。

实际只有**两个**配置项(第 41-42 行),判断条件(第 77 行)也只检查这两个:

```python
MTLS_CA_FILE = os.environ.get("MTLS_CA_FILE", "")
MTLS_CLIENT_CERT = os.environ.get("MTLS_CLIENT_CERT", "")
...
if MTLS_CA_FILE and MTLS_CLIENT_CERT:
```

建议改为「两项任一为空 → 纯 HTTP」。

这条虽是 P3,但会误导排障:照「三件套」去找第三个环境变量会白费工夫。
A11 的 mTLS 验收用例也依赖这里表述正确。

---

## A13 — [P2] 本地质量门禁跑不起来 + 未跟踪残留 + git gc 停摆

### 现状

```
?? .review-go/    333M   ← 一整份 Go 工具链源码
?? .review-old/    30M
?? .review-tmp/    12K
```

三者都未被 `.gitignore` 覆盖,每次 `git status` 都刷出来。

同时 `git gc` 已停摆:

```
warning: The last gc run reported the following. Please correct the root cause
and remove .git/gc.log.
warning: There are too many unreachable loose objects; run 'git prune' to remove them.
```

### 删 `.review-go` 之前必须先配 PATH

`.review-go/go/bin` 里有**可用的 go / gofmt 二进制**,与
`/home/maxin/.local/go/bin` 下的**逐字节相同**(均为 go1.26.5,`cmp` 已确认)。
所以它不是废弃源码树,而是一份重复的工具链。

但 `scripts/lint.sh` 是靠 **PATH** 找工具的:

```sh
if ! command -v gofmt >/dev/null 2>&1; then echo "gofmt 不可用"; exit 1; fi
...
(cd runtime && go vet ./... ...)
```

而当前 shell 里 `which go gofmt` **两者皆空**——两个位置都不在 PATH 里。
实测:

```
$ bash scripts/lint.sh
== gofmt ==
gofmt 不可用
```

**即本地质量门禁现在就是坏的**,与 `.review-go` 删不删无关。

### 修法(有顺序要求)

1. **先**把 `/home/maxin/.local/go/bin` 配进 PATH(shell profile),
   确认 `bash scripts/lint.sh` 能跑通
2. **再**删除 `.review-go`(确认无用后),否则会出现「删完 lint 更跑不了」
   的错误归因
3. `.review-old` / `.review-tmp`:确认无用后删除,或加进 `.gitignore`
   (若还要保留 `.review-old` 作对照快照)
4. 处理完 `.git/gc.log` 提示的根因后删除该文件,让自动 gc 恢复

**这一条涉及删除,动手前请人工确认 `.review-old` 是否还有参考价值。**

---

## 附:通过项(无需处理)

以下为本轮确认无问题的部分,列出以免重复审计:

- Agent HTTP 路由严格等于 §8.1 的六条,无多余端点;全仓唯一 `exec.Command`
  在 `agent/internal/adb/adb.go:224`
- `/api/v1/diagnostics` 四探测白名单 + `DisallowUnknownFields` + prop 名正则
  + 输出截断,无任意 shell 通道
- 所有 ADB 命令携带 `-s <serial>`;唯一例外 `Devices()` 有注释论证
  (设备发现阶段 serial 尚未知)
- 私有端口 5137 强制注入且剔除继承值(`commandEnv()`),全仓无 5037 代码路径
- `awaitResult` 是 signal channel + selector,timer 仅用于租约续期与硬超时
  判定,不是轮询(§14 合规)
- `tasks` 表 status / verdict 为两个独立列,未合并
- 产物文件名含 `CI_COMMIT_SHORT_SHA` + `CI_PIPELINE_IID`;bundle 命名
  CI 模板与 `gen_bundle.py`、`trigger` 三方一致
- 附件走预签名 PUT 直传 MinIO,Runtime 只下发 URL,不中转字节
- Plan / Manifest / result / bundle / evidence / dispatch 六处消费点全部
  先过 JSON Schema 再反序列化
- `hermes/` 三个 bridge 只经 Runtime cmd API,工具集封闭无 `adb_*`;
  `workflow_bridge` 写的是 hermes 平台自己的 leaderboard SQLite,
  方向为 Runtime → hermes,不构成「Hermes 直连数据库写」
- `runtime` 与 `agent` 两个 module `go build` + `go test` 全绿
