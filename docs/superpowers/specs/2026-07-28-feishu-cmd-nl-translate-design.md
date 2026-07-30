# 飞书指令层自然语言翻译设计（路线 A）

日期：2026-07-28

状态：**已批准**（2026-07-28 评审通过；修订含 §3.3 bridge 适配、§3.2 查询语义、
§5.2 应答闭集与待确认期输入处理、§6 超时基线；2026-07-30 随
`workflow_runs` 升级到 translation contract/prompt v2）

## 1. 背景与决策

`runtime/internal/feishucmd` 已实现封闭枚举指令层：长连接 listener 收单聊消息 →
白名单 open_id 鉴权 → `Parse` 解析 `status | devices | rerun | unquarantine` →
`Executor.execute` 执行 → 文本回复。不在这四个指令里的输入一律回 usage。

本设计在 `Parse` 与 `execute` 之间插入一层**意图翻译器**：自然语言经 Hermes 翻译成
一行合法指令文本，重走既有解析与校验，执行路径一个字节不变。目标是覆盖"懒得背指令
格式"这个场景（"帮我重跑一下昨天 SNPE 1.68 那个失败的"），**不是**让 Hermes 回答开放
问题（"为什么挂""这块板最近成功率"）——那属于语义层，需要只读工具白名单与多轮对话，
单独设计。

关键决策（已与负责人确认，2026-07-28）：

1. **翻译只在 `help` 分支触发**。能被现有语法解析的输入原样走老路，解析不了才问
   Hermes。省 token，且对既有指令零回归风险——LLM 不在任何一条已能工作的指令的路径上。
2. **翻译输出是一行指令文本，重走 `Parse`**。LLM 返回 `{command, args}`，Runtime 渲染成
   `rerun device-test-grp/project-g9da3b9d9-p56 aarch64_Android_SNPE_1.68`，原样喂回
   `Parse()`。翻译层的值域因此
   等于用户手打的值域，封闭性是结构上的保证，不依赖 prompt 措辞。
3. **副作用指令二次确认，只读指令直接执行**。`rerun` / `unquarantine` 翻译成功后先回执
   待确认，用户回 `y` 才执行；`status` / `devices` 直接执行。LLM 把源 workflow 猜错的
   代价是白跑一轮设备测试。
4. **待确认态存内存**（`Executor` 内 `map[open_id]pending` + TTL 120s，单槽覆盖）。worker
   重启丢失待确认项，代价只是用户重说一遍，绝不会误执行一个跨重启的陈旧 `rerun`。
5. **翻译请求附带 Runtime 组装的只读上下文快照**。"昨天那个失败的"必须有数据才能解析。
   快照由 Runtime 查库定形后随请求下发，Hermes 不直连数据库（§14 红线不放宽）。
6. **翻译不走 Temporal activity**，在 `HandleMessage` 内同步调用（带 context 超时）。
   §3 规则 5"Hermes 不在执行关键路径"约束的是派单/执行/回收；翻译挂了只是用户收到
   "没理解"，在跑的任务不受影响。
7. **审计单开 `command_translations` 表**。`decisions.task_id` 是
   `NOT NULL REFERENCES tasks(task_id)`（`schema.sql:103`），而翻译发生在任何 task 存在
   之前，没有 task_id 可填。
8. **`analyze_bridge` 加一条与 `/analyze` 同构的 `POST /translate`**，而不是把 bridge
   泛化成"prompt + schema 路径"的参数化服务。校验用哪份 schema 必须由服务端写死，
   不能由请求方指定（§3.3）。

不采用的方案：

- 所有消息都过翻译层：一致，但把 LLM 放进每一条指令的路径上，既费 token 又给已工作的
  指令引入新的失败模式。
- 翻译输出直接构造 `Command` 结构体交给 `execute`：省一次解析，但 `Command.Args` 是
  `[]string`，翻译层能造出 `Parse` 永远造不出的组合，封闭性退化为"Schema 约束得够不够严"。
- 待确认态落库：重启不丢，但引入跨重启的待确认——用户半小时后回 `y`，执行的是一个
  早已忘记上下文的 `rerun`。
- 只回建议指令让用户自己复制重发：零状态零风险，但每次都要用户多操作一步，便利性
  被吃掉大半。

## 2. 范围

交付：

- `contracts/command.schema.json`（当前 v2）+ 正反例测试；v1 输出作为 invalid 回归样例
- `hermesclient.Translate` + `prompts/cmd_translate_v2.md` + 内嵌 Schema 校验；
  `cmd_translate_v1.md` 原样保留作历史版本
- `feishucmd/translate.go`：快照组装、渲染、回灌 `Parse`、参数复校、置信度门限
- `feishucmd/executor.go`：`help` 分支旁路 + 待确认态
- `store`：`RecentRuns` / `SaveCommandTranslation` 双实现 + `command_translations` 表
  + 迁移文件
- **`analyze_bridge` 加 `POST /translate` 路由** + `command.schema.json` 部署副本
  + 防漂移测试 + bridge 侧校验打回重试（详见 §3.3）
- 配置项 `FEISHU_CMD_NL`、`FEISHU_CMD_NL_TIMEOUT_SEC` 与 `deploy/.env.example`、
  `deploy/README.md`、`hermes/analyze_bridge/README.md` 更新

不交付（明确排除）：

- 开放问答、多轮对话、Hermes 只读工具白名单（属语义层，另行设计）
- 新增任何指令（指令面严格保持四个 + help）
- 飞书交互卡片按钮确认（本轮确认用纯文本 `y`；卡片属 Phase 2 通知改造）
- 翻译结果的自动纠错重试（Schema 不过即失败回退，不做二次追问循环）

## 3. 架构

翻译层是 `feishucmd` 包内的一条旁路，不是新服务。

```text
飞书单聊消息
  │
  ▼
Listener（不变）──► Executor.HandleMessage
                      │
                      ├─ 白名单判定（不变，永远在最前）
                      │
                      ├─ 待确认槽非空？
                      │    ├─ y/yes ──► 执行待确认指令 ──► 回复（流程终止）
                      │    ├─ n/no  ──► 清槽 + 落 declined ──► 回"已取消"（流程终止）
                      │    └─ 其他  ──► 清槽 + 落 declined ──► 继续往下当新消息处理
                      │
                      ├─ Parse(text)
                      │    ├─ 命中四指令 ──────────────────► execute
                      │    └─ help ──► Translator.Translate
                      │                   │
                      │                   ├─ 组装上下文快照（store.RecentRuns + specCfg + FleetOverview）
                      │                   ├─ hermesclient.Translate（HTTP + command.schema.json 校验）
                      │                   ├─ 渲染 "cmd arg1 arg2 ..." 
                      │                   ├─ 回灌 Parse → 必须命中四指令，否则拒绝
                      │                   ├─ 参数复校（权威 workflow / 变体 / 设备存在性）
                      │                   └─ 置信度门限
                      │                        ├─ 只读指令 ──► execute ──► 回复（附"已按 X 执行"）
                      │                        ├─ 副作用指令 ──► 存待确认 ──► 回执确认提示
                      │                        └─ 任一环节失败 ──► usage + 翻译结果供人工判断
                      │
                      └─ 每条翻译落 command_translations
```

### 3.1 组件与职责

| 组件 | 文件 | 职责 |
|---|---|---|
| `Translator` | `feishucmd/translate.go`（新） | 快照组装、调用、渲染、回灌解析、复校、门限。不碰 `execute`，不碰飞书 SDK |
| `hermesclient.Translate` | `hermesclient/http.go`（扩展） | HTTP 调用 + `command.schema.json` 校验。与 `Analyze` 对称 |
| `POST /translate` | `hermes/analyze_bridge/analyze_bridge.py`（扩展） | 平台适配：prompt 拼装、`hermes -z`、Schema 校验打回重试（§3.3） |
| `Executor` 旁路 | `feishucmd/executor.go`（改） | `help` 分支接入、待确认态存取 |
| `store.RecentRuns` | `store/fleet.go` + `postgres_fleet.go`（扩展） | 快照的历史运行来源 |
| `store.SaveCommandTranslation` | `store/translations.go`（新） | 审计落库 |

`PromptVersion` 常量拆为 `PromptVersionAnalyze` / `PromptVersionTranslate`；现有
`PromptVersion` 只在 `http.go:87` 用了一处，改动面极小。

### 3.2 `RecentRuns` 的已知耦合与查询语义

2026-07-30 起，`workflow_runs` 是新运行输入的权威、不可变索引。`RecentRuns` 以
`workflow_runs.workflow_id = tasks.workflow_id` 精确关联新数据，不再用 workflow ID
前缀推断身份。每行快照携带 `workflow_id` 和 `authoritative=true`，使翻译器能输出
`rerun <source_workflow_id> [variant]`。

迁移不从历史 `artifacts` 或 `tasks` 回填 `workflow_runs`，因为缺失的 Version、
RuleVersion 和项目归属无法可靠恢复。旧查询只作为 `authoritative=false` 的 display-only
fallback；它可以帮助用户看历史，但不携带可执行 workflow 身份，翻译器必须拒绝把它
渲染成 rerun。源 workflow 是否关闭和终态 `DeviceTestOutput` 由执行器向 Temporal 精确
读取，不能从 task 行或 ID 形状推断。

### 3.3 `analyze_bridge` 适配（对岸改造）

`hermesclient` 对面是 `hermes/analyze_bridge/analyze_bridge.py`（FastAPI，跑在专用
hermes-agent 实例容器内，**不在 `deploy/docker-compose.yml` 里**，由实例内
`start-analyze-bridge` 脚本启停）。它今天只有 `POST /analyze` 一条业务路由，且：

- `REQUIRED_FIELDS = ("task_id", "prompt", "rule_category", "evidence")` 是硬编码的
  （`analyze_bridge.py:44`），翻译请求没有 `task_id` / `rule_category` / `evidence`，
  直接 400
- `ANALYSIS_SCHEMA` 从单一文件加载（`analyze_bridge.py:40`），校验对象写死

因此必须给 bridge 加一条 `POST /translate`，与 `/analyze` **同构**：

| 关注点 | `/analyze`（现状） | `/translate`（新增） |
|---|---|---|
| 必填字段 | `task_id, prompt, rule_category, evidence` | `prompt, context`（无 task_id——翻译发生在任何 task 之前） |
| prompt 拼装 | 平台 prompt + rule_category + evidence JSON | 平台 prompt + 上下文快照 JSON + 用户原文 |
| 调用形态 | `hermes -z <prompt> -t ""` | 同左（工具集全禁，§3 工具白名单不放宽） |
| 校验 | `analysis.schema.json` | `command.schema.json` |
| 打回重试 | ≤ `ANALYZE_MAX_ATTEMPTS`（缺省 3） | 同一常量，同一机制 |
| 失败 | 502 → Runtime 降级规则引擎 | 502 → Runtime 回 usage |

不把 bridge 泛化成"prompt + schema 路径"参数化服务：那会让调用方能指定校验用哪份
schema，等于把契约选择权交给请求方，与"平台输出永远不可信、由服务端定形校验"的
原则相悖。两条同构路由的重复量很小（可共用 `run_with_schema(payload, schema, ...)`
内部函数），换来的是每条路由的契约在服务端写死。

**校验在两侧都做，这是既有形态而非重复**：bridge 侧校验并把错误喂回重试（
`analyze_bridge.py:123`），是"让模型自我修正"的机制；`hermesclient` 侧再校验一次
（`http.go:124`），是"跨进程边界不信任对端"的防御。翻译沿用同一对称结构。

`command.schema.json` 需要一份 bridge 目录下的部署副本，并照搬既有的防漂移测试
（`test_analyze_bridge.py::test_schema_copy_matches_contracts`）——`analysis.schema.json`
已经是这个形态，不照做契约会两边跑偏。

部署影响：analyzer 容器需随本轮重新部署（拉新代码 + 重启 `start-analyze-bridge`），
这一步要写进 `hermes/analyze_bridge/README.md`。它不在 compose 里，`docker compose up`
不会带上它——容易漏。

## 4. 契约

### 4.1 `contracts/command.schema.json`（LLM 输出，v2）

```json
{
  "translation_version": 2,
  "command": "rerun",
  "args": ["device-test-grp/project-g9da3b9d9-p56", "aarch64_Android_SNPE_1.68"],
  "confidence": 0.92,
  "reason": "指代最近一次 SNPE 1.68 的失败运行"
}
```

约束：

- `additionalProperties: false`；`translation_version`、`command`、`confidence` 必填
- `command`：闭枚举 `status | devices | rerun | unquarantine | none`
  （`none` = 信息不足或根本不是指令）
- `rerun.args`：1 到 2 项，依次为最多 512 字符且不含任何 Unicode 空白的
  `source_workflow_id` 与可选 variant；workflow ID 允许 `/`
- `status/devices/none` 不接受参数；`unquarantine` 最多 1 项
- `confidence`：`number`，`0 ≤ x ≤ 1`
- `reason`：`string`，`maxLength: 200`，回执时展示给用户

`args` 的 `not: {"pattern":"\\s"}` 是全设计最吃重的一条：**禁掉全部空白字符**，使"渲染成文本再
`strings.Fields` 切回来"成为可逆操作。LLM 无法用含空格的 arg 把一行伪造成两个 token，
也无法用换行伪造多条指令。方案 1 的封闭性由这条 pattern 承担。

### 4.2 上下文快照（Runtime → 平台）

```json
{
  "now": "2026-07-28T09:12:00Z",
  "variants": ["aarch64_Android_SNPE_1.68", "aarch64_Android_SNPE_2.21"],
  "recent_runs": [
    { "workflow_id": "device-test-grp/project-g9da3b9d9-p56",
      "commit": "9da3b9d9", "pipeline_iid": 56,
      "version": "1.4.0", "rule_version": "v2",
      "variant": "aarch64_Android_SNPE_1.68",
      "verdict": "TEST_FAILED", "ended_at": "2026-07-27T14:03:00Z",
      "authoritative": true }
  ],
  "devices": [
    { "device_id": "dev-1", "serial": "513cd3de", "status": "QUARANTINED" }
  ]
}
```

- `now` 必须显式给：LLM 不知道今天几号，"昨天""最近一次"全靠它锚定
- `recent_runs` 上限 10 条（`RecentRuns(ctx, 10)`）
- `variants` 取 `specCfg`（与 `ExpectedVariants` 同源）
- `devices` 取 `FleetOverview`，只含 `device_id`/`serial`/`status`
- 快照只含调度元数据，**不含日志、不含 evidence、不含 result.json**
- 整体量级几 KB；`context_digest` = 快照 JSON 的 sha256，落审计
- 传输形态：作为 `POST /translate` 请求体的 `context` 字段（与用户原文 `raw_text`
  一起），由 bridge 拼进 prompt（§3.3）

### 4.3 `command_translations` 表

```sql
CREATE TABLE IF NOT EXISTS command_translations (
    translation_id BIGSERIAL PRIMARY KEY,
    open_id        TEXT        NOT NULL,
    raw_text       TEXT        NOT NULL,              -- 用户原话
    prompt_version TEXT        NOT NULL DEFAULT '',
    model          TEXT        NOT NULL DEFAULT '',
    context_digest TEXT        NOT NULL DEFAULT '',   -- 快照 sha256,可回放"当时看到了什么"
    output         JSONB       NOT NULL DEFAULT '{}', -- LLM 原始输出(校验失败也存,截断至 4KB)
    rendered       TEXT        NOT NULL DEFAULT '',   -- 渲染出的那行指令
    outcome        TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS command_translations_open_id_idx
    ON command_translations(open_id, created_at DESC);
```

`outcome` 取值：

| 值 | 含义 |
|---|---|
| `executed` | 只读指令，翻译后直接执行 |
| `pending_confirm` | 副作用指令，已回执待确认 |
| `confirmed` | 用户回 `y`，已执行 |
| `declined` | 用户回 `n`，或其他输入被重新处理后未产生替代待确认，放弃 |
| `expired` | TTL 到期，或被新待确认取代（含其他输入被重新处理后又翻译出一个需确认指令的情形，见 §5.2） |
| `rejected_schema` | 平台返回不符合 `command.schema.json` |
| `rejected_none` | 平台明确返回 `command = none`（信息不足或不是指令） |
| `rejected_args` | 渲染后回灌 `Parse` 或参数复校未通过 |
| `rejected_low_confidence` | `confidence < 0.75` |
| `translator_error` | 平台不可达/超时/非 2xx |

确认流程**只追加不更新**：待确认时写一行 `pending_confirm`，用户回 `y` 时再写一行
`confirmed`（`raw_text='y'`，`rendered` 为同一行指令）。只需一个 insert 方法，同 open_id
按时序读即完整证据链。`context_digest` 存摘要而非快照全文，与 `persistEvidenceSnapshot`
的思路一致，表不会被撑爆。

`output` 落库前**截断至 4KB**：Schema 校验失败时平台返回的可能是任意长度的自由文本
（模型跑飞时尤其如此），审计要留证但不该让一行把表撑坏。截断时在末尾标 `...(truncated)`。

DDL 同时进 `runtime/internal/store/schema.sql` 与
`deploy/postgres/migrations/2026-07-28-command-translations.sql`（仓库现有惯例）。

## 5. 数据流

### 5.1 只读指令（无确认）

```text
用户: "看下设备都什么状态"
  → Parse → help → Translator
  → 快照 {now, variants, recent_runs, devices}
  → LLM: {"command":"devices","args":[],"confidence":0.95,"reason":"询问设备状态"}
  → 渲染 "devices" → Parse → Command{devices} ✓
  → 落审计 outcome=executed
  → execute(devices)
  → 回复: "(已理解为: devices)\n513cd3de  soc=QCM6125 status=IDLE fail_streak=0"
```

回复带上"已理解为 X"这行，用户下次可以直接打 X，翻译层因此是自我消解的——用得越久，
用户越不需要它。

### 5.2 副作用指令（两段式）

```text
用户: "帮我重跑一下昨天 SNPE 1.68 那个失败的"
  → Parse → help → Translator
  → LLM: {"translation_version":2,"command":"rerun",
          "args":["device-test-grp/project-g9da3b9d9-p56","aarch64_Android_SNPE_1.68"],
          "confidence":0.9,"reason":"指代最近一次 SNPE 1.68 的失败运行"}
  → 渲染 → Parse ✓ → workflow ID 对应 authoritative 快照行 ✓ → 变体属于源 run ✓
  → 落审计 outcome=pending_confirm；pending[open_id] = {cmd, 到期时间 now+120s}
  → 回复: "将执行: rerun device-test-grp/project-g9da3b9d9-p56 aarch64_Android_SNPE_1.68
           (依据: 指代最近一次 SNPE 1.68 的失败运行)
           回复 y 确认,n 取消,120 秒后自动失效"

用户: "y"
  → 待确认命中且未过期 → 落审计 outcome=confirmed → execute(rerun ...)
  → 精确读取源 run + Temporal 终态输出 → 分配新 attempt → 回复执行结果
```

待确认态语义：

- 单槽：同一 open_id 的新待确认覆盖旧待确认，被覆盖的落一行 `expired`。触发方式
  有两种：并发覆盖（两个消息几乎同时处理，均在 `takePending` 读到空槽，`putPending`
  内部兜底判定）与顺序覆盖（待确认期间的输入被当作新消息重新处理后又翻译出一个
  需确认指令，见下面"其余任何输入"）
- TTL 120s：过期后回 `y` 视同未理解，回 usage，并落 `expired`
- **应答词是一个 4 元闭集**，`strings.TrimSpace` 后小写精确匹配，绝不模糊匹配：
  - `y` / `yes` → 执行待确认指令，清槽，流程终止
  - `n` / `no` → 清槽 + 落 `declined` + 回"已取消"，**流程终止，不再往下走**
- **其余任何输入：清槽 + 把该输入当作新消息继续处理**（继续走 `Parse`，未命中则
  继续走翻译）。旧待确认项的处置取决于重新处理的结果：若该输入本身又翻译出一个
  需确认指令，旧项视为被"取代"，落 `expired`（即上面"单槽"的顺序覆盖情形）；
  否则（执行了只读指令、翻译判定为 `none`、或翻译失败）旧项视为被"放弃"，落
  `declined`。例如用户在待确认期间直接改主意打 `devices`：待确认取消并执行
  `devices`（落 `declined`），而不是被吞掉。回复里带一句"已取消上一条待确认"，
  避免用户以为确认成功了

`n` / `no` 短路而不是走"其余输入"那条路，是为了与 `y` / `yes` 对称：确认问句给的是
两个答案，不是"一个答案加一堆非答案"。附带好处是省掉一次必然落 `rejected_none` 的
LLM 调用。规则本身没变复杂——`n` 只是从"非应答词"挪进了应答闭集。
- 待确认态检查在 `Parse` **之前**：这样用户打 `y` 不会被当成未知指令

### 5.3 参数复校（渲染之后、执行之前）

回灌 `Parse` 只保证了"形状是合法指令"，参数正确性仍要复校，全部复用既有函数：

| 检查 | 依据 | 失败回复 |
|---|---|---|
| 回灌 `Parse` 命中四指令 | `Parse` | 拒绝，落 `rejected_args` |
| `rerun` 参数是 1 或 2 项且各项 ≤512 字符、无空白 | command v2 schema + Runtime 复校 | 拒绝，落 `rejected_args` |
| `rerun` source 是快照中的 authoritative workflow ID | `RecentRuns` 精确身份 | 同上 |
| `rerun` 可选变体属于源 run | authoritative 快照行 | 同上 |
| `unquarantine` device_id 存在 | 快照 `devices` 成员判定 | 同上 |
| `confidence ≥ 0.75` | 门限常量 | 落 `rejected_low_confidence` |

快照校验只决定翻译结果能否进入确认态。`execute` 仍独立读取 immutable
`workflow_runs`、精确查询 Temporal 是否关闭并取得 `DeviceTestOutput`，再按源输入恢复
Version/RuleVersion/Project 与 packages；不能因为翻译层见过该 ID 就跳过执行期校验。

## 6. 错误处理

原则：**翻译层的任何失败都退化为今天的行为**（回 usage），绝不放大能力、绝不阻塞。

| 情形 | 行为 |
|---|---|
| `HERMES_ENDPOINT` 为空 / `FEISHU_CMD_NL=false` | 翻译层不启用，`help` 分支直接回 usage（今天的行为） |
| 平台超时/不可达/非 2xx | 落 `translator_error`，回 "翻译服务暂时不可用" + usage |
| 响应不符合 Schema | 落 `rejected_schema`，回 "没理解" + usage |
| `command = none` | 落 `rejected_none`，回 "没理解" + usage，附 `reason` |
| 渲染后回灌 `Parse` 未命中 | 落 `rejected_args`，回 "没理解" + usage |
| 参数复校失败 | 落 `rejected_args`，回 "你是想说 `<渲染的指令行>` 吗?该指令参数不合法: <原因>" |
| `confidence < 0.75` | 落 `rejected_low_confidence`，回 "不太确定,你是想说 `<指令行>` 吗?确认请直接发这行" |
| 审计落库失败 | 记 error 日志，**不阻断**执行（与 `persistEvidenceSnapshot` 的降级一致） |
| 快照组装失败（查库出错） | 记日志，用降级快照继续（仅含 `now`，其余为空数组；LLM 大概率返回 `none`，安全降级） |

非白名单 open_id 的处理**完全不变**：在翻译之前静默忽略，翻译层永远看不到非白名单
消息，token 消耗不受外部消息影响。

超时：翻译请求单独配 `FEISHU_CMD_NL_TIMEOUT_SEC`，**缺省 60s**，不复用
`HERMES_TIMEOUT_SEC`。

这个值不能按"人在飞书里等回复"的直觉往小了设。`hermes/analyze_bridge/README.md`
记录的实测基线（2026-07-22，deepseek-v4-pro）是 `-t ""` 冷/热约 **76s / 13s**，
并建议 Runtime 侧 `HERMES_TIMEOUT_SEC ≥ 120`。热路径 13s 对交互勉强可接受，冷启动
76s 会超；60s 是"绝大多数热调用能过、冷启动失败并明确告知"的折中——超时的代价只是
一句"翻译服务暂时不可用"加 usage，用户手打指令永远不受影响。若实测冷启动频繁命中，
调大这个环境变量即可，无需改代码。

## 7. 配置

| 变量 | 缺省 | 说明 |
|---|---|---|
| `FEISHU_CMD_NL` | `false` | 翻译层总开关。灰度用：先让四指令跑稳，再打开 |
| `FEISHU_CMD_NL_TIMEOUT_SEC` | `60` | 翻译请求超时（§6 有实测依据） |
| `HERMES_ENDPOINT` | 空 | 复用分析用的同一 bridge 基址；为空则翻译层不启用 |
| `HERMES_AUTH_TOKEN` | 空 | 复用（对应 bridge 的 `ANALYZE_BRIDGE_TOKEN`） |
| `HERMES_MODEL` | 空 | 复用 |

`HERMES_ENDPOINT` 现状是 `/analyze` 的完整 URL（`hermesclient.Config.Endpoint` 的注释
即"完整调用 URL"）。翻译需要同一 bridge 的 `/translate`，实现上取 `HERMES_ENDPOINT`
的路径部分替换为 `/translate`——不新增环境变量，避免两个 URL 配到不同实例上。
这条替换规则要有单测覆盖（含尾斜杠、带端口、带子路径三种形态）。

启用条件是三者的合取：`FEISHU_CMD_NL=true` 且 `HERMES_ENDPOINT` 非空 且指令 listener
本身已启用（`FEISHU_CMD_WHITELIST` 非空）。任一不满足，`help` 分支回今天的 usage，
并在启动日志打印 `feishu cmd nl=disabled (原因)`——与现有
`feishu cmd listener=disabled (FEISHU_CMD_WHITELIST empty)` 的日志风格一致。

## 8. 测试

### 8.1 契约测试（`contracts/tests/`）

按仓库现有 `examples/{valid,invalid}` 惯例，为 `command.schema.json` 建正反例：

- valid：`translation_version=2`；四个指令与 `none` 各一例；`rerun` 分别带 1 项
  source workflow ID、2 项 source workflow ID + variant；`confidence` 边界 0 与 1
- invalid：`translation_version=1`、未知 `command`、`rerun` 为 0 项或超过 2 项、
  任一 arg 含 Unicode 空白（空格、换行、NBSP、全角空格等，**关键用例**）、任一 arg
  超过 512 字符、`confidence` 越界、多余字段、缺 `translation_version`

### 8.2 `feishucmd` 单测（fake Translator，不打网络）

- **渲染-解析往返**：对 schema 允许的全部 args 形态，断言
  `Parse(render(cmd)) == cmd`（这是方案 1 封闭性的核心断言）
- 只读指令直接执行；副作用指令进待确认不执行（断言 fake Starter 未被调用）
- `y` / `yes` 确认执行；`n` / `no` 清槽回"已取消"且**不触发翻译**（断言 fake
  Translator 零调用——这是短路的全部意义）
- 应答闭集外的输入清槽后**继续处理该输入**：待确认期间打 `devices` → 落 `declined`
  且 `devices` 被执行
- TTL 过期后 `y` 不执行
- 新翻译覆盖旧待确认（旧的落 `expired`）
- 待确认态检查先于 `Parse`：用户打 `y` 时不落到 usage
- 权威成员校验：source workflow ID 必须来自快照中的 `authoritative=true` 行；可选
  variant 必须属于同一个 source workflow，不能借用另一 source 的同名或异名 variant
- 逐条错误路径：schema 不过 / `none` / 回灌失败 / 伪造或 non-authoritative workflow /
  跨 source variant / 设备不存在 / 低置信度 —— 各断言回复文本与 `outcome` 落库值
- 非白名单发自然语言：Translator **零调用**（断言 fake 计数为 0）
- 翻译层禁用时 `help` 分支行为与今天逐字节一致

### 8.3 `hermesclient.Translate` 单测

沿用 `hermesclient_test.go` 的 httptest 模式：2xx + 合法 JSON、2xx + 不合法 JSON、
非 2xx、超时、响应非 JSON —— 断言错误分类与 `Analyze` 对称。另加 `/analyze` →
`/translate` 的 URL 推导测试（尾斜杠、带端口、带子路径，§7）。

### 8.4 `analyze_bridge` 测试（pytest）

沿用 `test_analyze_bridge.py` 的假 hermes CLI 驱动模式，为 `/translate` 补：

- 缺必填字段 → 400；鉴权失败 → 401
- 合法输出一次通过；不合法输出打回重试后通过；连续失败耗尽 → 502
- `-t ""` 始终存在于命令行（工具白名单断言，与 analyze 同款）
- `command.schema.json` 部署副本与 `contracts/` 一致（照搬
  `test_schema_copy_matches_contracts`）

### 8.5 store 一致性测试


`RecentRuns` / `SaveCommandTranslation` 进 `conformance_test.go`，MemStore 与 PGStore
双实现跑同一组断言（仓库既有约束）。

`RecentRuns` 的测试覆盖 §3.2 的权威与 fallback 边界：

- 新 run 只按 `workflow_runs.workflow_id = tasks.workflow_id` 精确关联，不接受相似前缀
- authoritative 行携带完整 workflow ID、Version、RuleVersion 与 canonical variants
- 有任何 `workflow_runs` 身份覆盖的 artifact key 不再追加 legacy fallback
- 没有 run 身份的 legacy 行标记 `authoritative=false`，只展示且不能生成 rerun

### 8.6 手工验收

前置：**analyzer 容器已拉新代码并重启**（`start-analyze-bridge`）——它不在 compose 里，
`docker compose up` 带不上，漏了这步全部自然语言都会 502。先 `curl` 一次
`POST /translate` 确认路由存在。

`FEISHU_CMD_NL=true` 打开后，白名单账号在飞书单聊依次验证：

1. "看下设备状态" → 直接返回 devices 列表，回复含"已理解为: devices"
2. "帮我重跑昨天 SNPE 1.68 那个失败的" → 回执待确认 → 回 `y` → workflow 启动，
   ID 含 `-r{N}` 后缀
3. 同上但回 `n` → 回"已取消"，`command_translations` 有 `pending_confirm`
   + `declined` 两行，且 bridge 侧无新增调用日志
4. 同上但改口打 `devices` → 待确认取消，`devices` 被执行（验证 §5.2 的"继续处理"）
5. "今天天气怎么样" → 回 usage，落 `rejected_none`
6. 停掉 bridge → 自然语言回"翻译服务暂时不可用" + usage，四个手打指令照常工作

## 9. 验收标准

- 指令面仍是四个 + help；rerun 当前语义为精确 source workflow，旧参数形式 fail closed
- `Parse(render(x)) == x` 对 schema 允许的全部输入成立（有测试）
- 翻译层禁用或失败时，行为与本轮改动前逐字节一致
- 每次翻译在 `command_translations` 留痕，含原文、`context_digest`、渲染结果、outcome
- 副作用指令未经用户确认不会执行（有测试）
- 非白名单消息不触发任何 LLM 调用（有测试）
- bridge `/translate` 与 `/analyze` 同构：工具集全禁、服务端定形校验、打回重试上限
  共用同一常量（有测试）
- `command.schema.json` 的 bridge 部署副本与 `contracts/` 一致（有防漂移测试）

## 10. 后续（不在本轮）

- Hermes 语义层：只读工具白名单、多轮对话、开放问答（"为什么挂""这块板成功率"）。
  本轮沉淀的 prompt 管理、输出 Schema 校验、翻译审计三件积木可直接复用。
- 飞书交互卡片确认按钮替代文本 `y`（与 Phase 2 卡片改造合并）。
- **回复路由到消息发送者**。`feishu.Sender.SendText` 现在只发往静态
  `FEISHU_RECEIVE_ID`（`feishu.go:260`）。白名单只有 1 人时无差别；多人白名单那天，
  A 的指令回执会发到 B 眼前。届时给 `Sender` 加 `SendTextTo(ctx, openID, text)`，
  指令回复走它，通知仍走静态目标。本设计的其余部分不受影响。
- **按 `message_id` 去重**。飞书长连接重连时可能重投 `im.message.receive_v1` 事件；
  每次直接文本 `rerun` 都会分配新的 attempt 和 workflow ID，Temporal
  `RejectDuplicate` 挡不住重复指令。正解是先按 `message_id` 去传输重投；下一轮按钮
  还必须用持久化 claim 固定 attempt 与 target workflow ID，覆盖真实重复点击和并发点击。
