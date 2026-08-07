# 飞书指令回答智能化设计（规则事实 + LLM 表述）

日期：2026-08-07

状态：**已批准**（2026-08-07 评审通过；修订：审计改 command_translations、
§8 措辞修正、实施步骤补共享匹配函数导出）

> 评审修订记录：
> - **硬伤修正**：Express 审计不再落 decisions（其 task_id 是 NOT NULL FK，
>   只读查询无 task），改为复用 command_translations（新增 outcome 值
>   express_ok / express_fallback，output 存表述输出，context_digest 存 Facts
>   摘要），一张表串起"原话 → 翻译 → 表述"完整证据链。
> - **§8 措辞修正**：unquarantine 直接查 FleetOverview（executor.go:651），
>   不经快照；快照 devices 只给翻译 LLM 看。结论不变，描述改准。
> - **实施步骤补**：变体-设备匹配逻辑在 activity/workflow 包（specs.go:166），
>   feishucmd 需导出或下沉共享匹配函数。
> - 开放问题按答复定稿：只做 devices 但 Express 接口命令无关；独立
>   HERMES_EXPRESS_MODEL（未配置回落翻译层配置）；can_test 上限折叠；
>   本轮纯文本（emoji 放行）；不做缓存。

## 1. 背景与问题

当前飞书指令的回答是**规则拼接**：

```
用户："查询当前在线设备"
  ↓ LLM 意图翻译层(已有)     → 封闭命令 devices
  ↓ 规则执行(executor)        → 拉 FleetOverview(全量设备)
  ↓ 规则拼回答(生硬)          → 逐行打印全部设备(含离线/隔离)
```

三个痛点：

1. **不贴合意图**：用户问"在线设备"，回答列出全部（含 OFFLINE/QUARANTINED）。`devices`
   命令语义是"全部注册设备"，没有过滤/聚合。
2. **无洞察**：回答只是"印表"，不告诉用户"能测什么、缺什么、该接什么板"。
3. **生硬**：即使数据对，表述也是固定模板，不是对话。

**已有基础**（不复用就浪费了）：
- LLM 意图翻译层已存在（`feishucmd/translate.go`，`cmd_translate_v2` prompt），
  把自然语言 → 封闭命令。
- 上下文快照已存在（`buildSnapshot`：now/variants/recent_runs/devices），
  每次翻译都带。
- Hermes Analyzer 已有"规则事实 + LLM 补充"的成熟模式（§9 判定权在规则，
  LLM 只解释），回答生成可复用同一套降级哲学。

## 2. 关键决策

1. **事实永远由规则计算，LLM 只负责表述与洞察**（红线，不可违反）。
   哪些在线、能测什么变体、缺口是什么——全部由 Runtime 规则从 store 算出，
   组装成结构化 JSON 传给 LLM；LLM 输出只是"怎么说"。这样：
   - LLM 输出不可信 → 但表述错误不产生决策后果（不触发任何动作）；
   - 事实错误（说某设备在线）被规则挡住，不可能发生。
   与 §3 边界一致：LLM 不在执行关键路径，只做解释。

2. **意图翻译层继续复用，扩展命令参数**（方案 A，随本设计一起做）：
   `devices` 支持过滤参数 `online|all|offline|quarantined`，缺省 `online`。
   LLM 翻译"在线设备"→ `devices`（等价 online）；"所有设备"→ `devices all`。
   这是规则层，保证即使 LLM 表述层挂了，`devices` 也至少是"在线优先"。

3. **新增表述层（方案 B，本设计主体）**：`devices`（以及后续 `status`）执行时，
   规则先算好事实，交给 LLM 生成人性化回答；LLM 不可用/超时 → 降级为规则文本。
   降级文本由规则生成并随载荷下发（与通知卡片的 FallbackText 同一哲学：
   activity 不自行拼文本，单一真源）。

4. **不改 `feishu.Sender`**：新增 `query` 活动或复用现有 executor 的直接回复路径。
   `devices` 是只读命令，不需要 activity；executor 直接调用新接口。

5. **回答的结构化输出必须有 Schema 约束**：LLM 返回的不是自由文本，而是
   `{ summary, sections[], footer }` 之类受控结构（见 §4），防止 LLM 注入
   格式或编造字段。表述自由但结构封闭。

6. **审计与可回放：复用 `command_translations` 表**（评审修订）。Express 是只读
   查询，无 task_id 可填，`decisions.task_id` 是 NOT NULL 外键（schema.sql:146），
   落不进 decisions——当年翻译审计正是因此单独建了 `command_translations`
   （2026-07-28-command-translations.sql:3 注释写明）。Express 复用同一张表：
   `outcome` 新增 `express_ok` / `express_fallback`，`output`(JSONB) 存表述
   结构化输出，`context_digest` 存 Facts 摘要 sha256，`raw_text` 存用户原话。
   一张表串起"原话 → 翻译 → 表述"完整证据链，比散到 decisions 更好回放。

## 3. 范围（本轮）

- **只做 `devices` 命令**（用户当前痛点）。`status`、`rerun` 等的表述层
  留作下一轮（同一机制可平铺）。
- **事实计算**：在线设备 + 每台的 soc/os/能力 + **可测变体**（用 variants.yaml
  的 requirements 匹配）+ **调度缺口**（fleet 无匹配设备的变体、缺哪种板）。
- **LLM 表述**：基于事实 JSON 生成回答。
- **降级**：LLM 挂 → 规则文本（等价今天，但只列在线 + 折叠离线）。

明确不做（本轮）：NL 自由问答（方案 C）、`status` 表述层、回答里的"为什么"推理
（LLM 只能基于传入事实，不能自由查库）。

## 4. 设计

### 4.1 命令参数（方案 A）

```
devices [online|all|offline|quarantined]
  缺省 online:只列 IDLE/BUSY;离线/隔离折叠为一行提示
  all:全部(现状);offline:仅离线;quarantined:仅隔离
```

`command.schema.json` 扩展 `args` 枚举（或新增 `devices_scope` 字段，见 §8 迁移）。
意图翻译 prompt 增加映射规则："在线设备/设备怎么样" → `devices`(online)，
"所有设备" → `devices all`。

### 4.2 事实计算（规则层，确定性）

`executor.devices` 执行时先算 `DeviceFacts`：

```json
{
  "now": "2026-08-07T09:00:00Z",
  "online": [
    { "id": "825485946", "os": "linux", "soc": "idp", "capabilities": ["hexagon"],
      "can_test": ["aarch64_Linux_QCS6125_SNPE_1.68", "aarch64_Linux_QCS6490_SNPE_2.21"],
      "can_test_count": 2 }
  ],
  "offline_count": 2,
  "quarantined": ["b5bb1018d94b26da"],
  "gaps": [
    { "variants": ["aarch64_Android_Qualcomm_TFLite_2.21.0", "aarch64_Android_QCM6125_SNPE_1.68"],
      "reason": "无 IDLE 的高通 Android 板(QCM6125/QCM6490 在线板均离线)" }
  ],
  "suggestions": ["接入高通 Android 板即可调度 3 个待测变体"]
}
```

- `online` 每台附带 `can_test`：用 variants.yaml 的 requirements（os/soc/
  capabilities，与 SelectTestSpecs 同一匹配函数）算该台可测的变体。
  **can_test 有上限**（评审定稿：如每台最多列 N=5 个，超出只保留前 N 个并在
  `can_test_count` 记录总数）——Facts 进 prompt，无界会让 token 与延迟失控。
- `gaps`：fleet 无匹配设备（任意状态）的变体集合 + 人类可读缺口原因
  （复用 `skipReason`/`noDeviceReason` 的领域语言）。
- `suggestions`：从 gaps 反推可行动建议（"接入 X 板即可调度 N 个变体"）。

事实计算是纯函数，表驱动单测（输入 store 状态 → 输出 DeviceFacts）。
**匹配函数共享**（评审补充）：变体-设备匹配逻辑当前在 activity/workflow 包
（specs.go:166 SelectTestSpecs），feishucmd 要复用需导出或下沉为共享函数
（如 `activity.MatchDeviceSelector` 或独立 `internal/matching` 包），
实施步骤第 3 步包含此重构。

### 4.3 LLM 表述层（命令无关）

新增 `hermesclient.Express` 接口（与 `Translator`/`Planner` 同款）。
**命令无关**（评审定稿）：入参是 Facts JSON + 场景标识，不绑定 devices——
status 下一轮直接平铺复用，不重写。

```go
type ExpressRequest struct {
    RawText string          // 用户原话("查询当前在线设备")
    Scene   string          // 场景标识:"devices" | "status"(未来) | ...
    Facts   json.RawMessage // 规则算好的事实(DeviceFacts)
    Model   string
}
type ExpressResponse struct {
    Summary  string   `json:"summary"`   // 一句话总览
    Sections []string `json:"sections"`  // 分段(每段可多行,LLM 组织)
    Footer   string   `json:"footer"`    // 可选,建议/提醒
}
```

- 输出受 `express.schema.json` 约束（封闭结构，见 §4.4）。
- prompt 版本化：`express_v1.md`，进 Git（`PromptVersionExpress`）。
- **prompt 铁律**：LLM 只能引用 Facts 里的设备/变体/缺口，禁止编造；
  事实冲突时以 Facts 为准。这是防幻觉的第一道闸。
- **模型配置**（评审定稿）：独立 `HERMES_EXPRESS_MODEL`，表述在交互路径上
  对延迟最敏感；**未配置时回落翻译层同款模型配置**，不让 Express 缺省不可用。

### 4.4 express.schema.json（封闭结构）

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["summary", "sections", "footer"],
  "properties": {
    "summary":  { "type": "string", "minLength": 1, "maxLength": 200 },
    "sections": { "type": "array", "items": { "type": "string", "maxLength": 500 },
                  "minItems": 1, "maxItems": 6 },
    "footer":   { "type": "string", "maxLength": 200 }
  }
}
```

- `sections` 每段 ≤500 字（rune），总回答 ≤~3.2KB——防 LLM 长篇。
- 与 command.schema.json 同款：校验不过 → 翻译失败降级规则文本。

### 4.5 执行流程（devices）

```
用户消息
  → 意图翻译(已有 LLM)          → devices
  → executor.devices
      → 规则算 DeviceFacts        (确定性)
      → 若 LLM 可用:
          → Express(Facts) → 结构化回答
          → 校验 schema;失败降级
          → 落 command_translations(outcome=express_ok)
      → 若 LLM 不可用/超时/降级:
          → 规则文本(只列在线 + 折叠离线 + gaps 提示)
          → 落 command_translations(outcome=express_fallback)
  → 回复
```

### 4.6 降级矩阵

| 场景 | 行为 |
|---|---|
| LLM 正常 | 结构化人性化回答（outcome=express_ok） |
| LLM 超时 | 规则文本（不等待，超时上限沿用 HERMES_TIMEOUT_SEC；express_fallback） |
| LLM 返回不合 Schema | 规则文本 + 记日志（提示 prompt 需迭代；express_fallback） |
| LLM 服务不可用 | 规则文本（与现状等价，但已在线优先；express_fallback） |

降级文本由规则生成，**不是** LLM 输出——保证任何时刻回答都可用。
**输出格式**（评审定稿）：本轮纯文本消息；emoji 在纯文本里无害可放行；
lark_md/卡片化随通知卡片机制演进时一起做（现在扩展 schema + 转义是白费功）。

## 5. 边界与安全

- **LLM 不触发任何动作**：表述层是只读命令的"润色"，输出不含命令执行。
- **事实单一真源**：LLM 拿到的 Facts 是序列化 JSON，且 prompt 禁止编造；
  输出结构封闭（summary/sections/footer），无自由字段。
- **不违反 §3**：Hermes 仍只经 Runtime、结构化输入输出、不在执行关键路径。
  表述层是"解释"不是"决策"。
- **审计**：每次 Express 落 `command_translations`（outcome=express_ok/express_fallback），
  可回放；LLM 挂 → 规则文本，无黑洞。不落 decisions（task_id NOT NULL FK
  挡只读查询，评审硬伤修正）。

## 6. 测试

- 事实计算：`TestDeviceFacts`（表驱动：多设备/缺口/可测变体匹配/can_test 上限折叠）。
- 翻译扩展：`devices online|all|offline` 意图映射（fakeTranslator 注入）。
- 表述层：`TestDevicesReplyUsesExpress`（LLM 正常 → 结构化回答）、
  `TestDevicesReplyDegradesToRuleText`（LLM 挂/超时/不合 Schema → 规则文本）。
- Schema：`express.schema.json` 正反例。
- 审计：Express 调用落 `command_translations`（outcome=express_ok / express_fallback，
  output 存表述输出，context_digest 存 Facts 摘要）。

## 7. 实施步骤

1. `contracts/express.schema.json` + 校验测试。
2. `hermesclient`：`Express` 接口（命令无关：RawText+Scene+Facts+Model）+
   `express_v1.md` prompt + `PromptVersionExpress` + `HERMES_EXPRESS_MODEL`
   配置（未配置回落翻译层模型）。
3. **下沉共享匹配函数**：把变体-设备匹配从 activity/workflow 包
   （specs.go:166）导出为共享函数（或独立 `internal/matching` 包），
   SelectTestSpecs 与 DeviceFacts 共用同一匹配逻辑（防漂移）。
4. 规则事实：`DeviceFacts` 计算（复用共享匹配 + skipReason 领域语言 +
   can_test 上限折叠）。
5. `devices` 命令参数化（online/all/offline/quarantined）+ 翻译 prompt 扩展。
6. executor.devices 接线：算 Facts → Express → 降级 → 落 command_translations。
7. 全部测试 + 部署 + 实测（"查询在线设备" → 人性化回答；拔掉
   HERMES_ENDPOINT → 规则文本）。

## 8. 迁移与兼容

- `devices` 缺省从"全部"改为"在线"是行为变更：help 文本与快照 devices
  （LLM 翻译上下文）同步。
- **unquarantine 措辞修正**（评审修订）：unquarantine 直接查
  `FleetOverview`（executor.go:651）做 device_id 存在性校验，**不经过翻译快照**；
  快照里的 devices 是给翻译 LLM 看的上下文。两者不冲突——回答展示按 scope
  过滤，unquarantine 校验仍用全量，互不影响。
- 若担心 `devices all` 语义，可加 `devices --all`，但当前无 `--` 惯例，
  用子命令 `devices all` 更一致（与 `rerun <id> [variant]` 同款）。
- command.schema.json 的 `args` 扩展向后兼容（新枚举值，旧 LLM 不会产出，
  新 LLM 产出旧命令也不破坏）。
- `command_translations.outcome` 追加新值是向后兼容的（旧行不受影响；
  outcome 无 CHECK 约束，纯追加语义）。

## 9. 开放问题（评审定稿）

1. **范围**：只做 `devices`。Express 接口按命令无关设计（Scene 字段），
   `status` 下一轮直接平铺复用，不重写。
2. **模型**：独立 `HERMES_EXPRESS_MODEL`；未配置时回落翻译层同款模型配置，
   不让 Express 缺省不可用。
3. **Facts 粒度**：can_test 与 gaps 都算；can_test 加上限（每台最多列
   N=5 个变体，超出计数折叠为 can_test_count），防 token/延迟失控。
4. **输出格式**：本轮纯文本；emoji 在纯文本里无害可放行；lark_md/卡片化
   随通知卡片机制演进一起做，现在扩展 schema + 转义是白费功。
5. **缓存**：不做。设备状态高频变化，缓存键（Facts 全等）命中率低且引入
   失效复杂度——每次实时，简单且正确。

## 10. 不做（本轮）

- NL 自由问答（方案 C）：需要更强的防幻觉（事实引用强制、白名单），
  且与封闭命令面设计有张力，留作独立设计。
- `status`/`rerun` 表述层：机制同 devices，下一轮平铺（Express 已命令无关）。
- 回答卡片化：先纯文本，卡片化（含转义）随通知卡片机制演进。
