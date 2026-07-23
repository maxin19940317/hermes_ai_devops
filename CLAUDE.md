# CLAUDE.md — Hermes AI DevOps 开发板自动测试系统

> 本文件是项目的权威上下文。所有架构决策已经过评审定稿,**按此实现,不要重新发明**。
> 如果实现中发现契约有缺陷,先在代码注释中标记 `CONTRACT-ISSUE:`,提出修改建议,不要静默偏离。

---

## 1. 一句话定位

一套由 Hermes(LLM Agent)驱动的自动编译、部署、开发板测试、分析与通知系统:
**用户只描述目标,Hermes 负责理解/规划/分析/反馈;底层由确定性 Runtime 可靠完成编译、部署、测试、恢复,不因 LLM 上下文、服务重启或网络抖动失去执行一致性。**

## 2. 物理环境(事实,不可假设更改)

- **服务器**:Linux,运行 GitLab、GitLab Runner、Package Registry,以及本项目的所有服务端组件。
- **Client 机器**:Windows,与服务器同一局域网,通过 USB 连接目标开发板(Android,ADB 访问)。
- **GitLab 版本:13.8**。Generic Package 版本号强制 strict `X.Y.Z`,不接受 `+` 和预发布后缀。这是已踩过的坑,产物唯一性靠**文件名**编码(见 §7)。
- 现有 CI 已能构建 8 个变体并上传 Registry(见 §6 现状)。
- 第一阶段:1 台 Client、1~2 块开发板。但数据模型从第一天按多 Client 多设备设计。

## 3. 三层架构与硬性边界(最重要的一节)

```text
语义层  Hermes          决定"做什么、为什么、下一步"     (LLM,不在执行关键路径)
执行层  Workflow Runtime 保证"可靠执行、状态不丢、不重复"  (确定性,Temporal)
设备层  Client Agent     在 Windows/开发板上"真正干活"    (确定性,Go,无 LLM)
```

**不可违反的规则(实现任何功能前先对照这里):**

1. Hermes 与 Client Agent 之间**禁止任何直接通信**。Hermes 只对 Runtime 说话,Client 只认 Runtime 一个上游。Client 代码中不允许出现 Hermes 的概念。
2. Hermes 对 Runtime 的所有输入必须是**受 JSON Schema 约束的结构化数据**(Plan DSL、控制操作),Runtime 拒绝自由文本。
3. Hermes 不能在任务执行中临时生成任意 ADB/Shell 命令。设备上执行什么,由构建包内的 **Manifest** 在打包期声明,Client 严格照做。
4. Client Agent 不提供任意 Shell 接口。所有 ADB 操作走模板化白名单,一律 `adb -s <serial>`。
5. Hermes 不可用时,已开始的确定性任务必须能继续完成——LLM 不在派单/执行/回收的关键路径上。
6. Plan 中的声明式策略**优先于** Hermes 的临场判断。Hermes 要改策略必须走显式 `update_policy` 控制操作并留审计。
7. 所有跨组件动作携带幂等键;所有 Hermes/人工决策落 `decisions` 表;所有操作落 `audit_log`。

## 4. 技术选型(已定稿)

| 领域 | 选型 | 备注 |
|---|---|---|
| Workflow Runtime | **Temporal 自托管** + 自研 Worker | Workflow/Activity 用 Go SDK |
| 服务端语言 | Go(Runtime Worker、Trigger 服务、API) | |
| Hermes | **复用现有 hermes-agent 平台**(q-uat 上 nousresearch/hermes-agent) | 2026-07-21 决策变更:不自研 Agent 循环;§3 硬性边界不变,工具白名单 + JSON Schema 输出校验仍必须落地 |
| LLM 模型 | 由 hermes-agent 平台配置(原 Sonnet/Haiku 分工不再适用) | prompt 版本化,进 Git |
| Client Agent | **Go**,Windows Service(kardianos/service) | 单二进制分发 |
| 服务器↔Client 通信 | **REST + JSON over HTTPS + mTLS** | 不用 gRPC(留给后期日志流) |
| 数据库 | PostgreSQL 15+(与 Temporal 共实例分库) | Client 本地用 SQLite(WAL) |
| 附件/日志存储 | MinIO(S3 兼容),预签名 URL 直传 | 大文件不过 Runtime |
| 产物仓库 | GitLab Generic Package Registry(现状沿用) | |
| 通知 | 飞书机器人 + 交互卡片(按钮回调 → Runtime signal) | |
| 部署 | Docker Compose(服务器全套);Client 手动安装 MSI/exe | |
| 日志 | 结构化日志(zerolog),UTC + 毫秒,全组件 NTP | |

## 5. 建议仓库结构(monorepo)

```text
hermes-devops/
├── CLAUDE.md                  # 本文件
├── contracts/                 # 契约优先,四方共同依赖,只加字段不删字段
│   ├── plan.schema.json       # Plan DSL v1
│   ├── manifest.schema.json   # Manifest v1
│   ├── result.schema.json     # result.json v1
│   ├── client-agent-api.openapi.yaml
│   └── callbacks-api.openapi.yaml
├── ci/                        # 给业务仓库(algo-super-sdk)使用的 CI 脚本
│   ├── gen_manifest.py
│   ├── variants.yaml          # 8 个变体 → Manifest 参数映射表
│   ├── write_meta.py
│   └── gen_bundle.py
├── runtime/                   # Go: Temporal Worker + Trigger + REST API
│   ├── cmd/{worker,trigger,api}/
│   ├── internal/workflow/     # DeviceTestWorkflow 等
│   ├── internal/activity/     # dispatch/collect/extract_evidence 等
│   ├── internal/store/        # Postgres 访问层
│   ├── internal/evidence/     # 确定性证据提取器
│   └── internal/rules/        # 规则引擎(LLM 的保底裁决)
├── agent/                     # Go: Windows Client Agent
│   ├── cmd/agent/             # 服务模式
│   ├── cmd/agent-cli/         # CLI 模式(先做这个!见 §12 Phase 1)
│   └── internal/{server,executor,artifact,adb,reporter,store}/
├── hermes/                    # Go 或 Python: Planner/Analyzer/Notifier/IM 网关
│   ├── internal/{planner,analyzer,notifier,gateway,tools}/
│   └── prompts/               # 版本化 prompt
├── deploy/docker-compose.yml  # Temporal + Postgres + MinIO + runtime + hermes
└── docs/
```

## 6. 现状:业务仓库的 CI(已存在,只做增量改造)

业务仓库 `algo-super-sdk` 已有 `.gitlab-ci.yml`:

- 触发:MR / tag / master push;8 个构建变体:
  `aarch64_{Linux,Android}_SNPE_1.68`、`aarch64_{Linux,Android}_SNPE_2.21`、
  `aarch64_{Linux,Android}_RKNN_2.3.2`、`aarch64_{Linux,Android}_TFLite_2.21.0`
- 每变体执行 `./rebuild.sh $variant` + `./release_pack.sh --platform $variant --output-dir dist`,产出 `*.tar.gz + *.sha256`,上传 Generic Registry(版本号取 CMakeLists 的 X.Y.Z;tag 时取 tag)。
- 已有 candidate(master, `rc.{CI_PIPELINE_IID}`)/ release(tag)双轨与手动打 tag job。
- 上传对 400 already-exists 做 skip(幂等)。

**待改造三件事(Phase 1 的 CI 部分):**

1. **文件名编码唯一性**:master 构建的包文件名改为
   `${RELEASE_PACKAGE_NAME}-${variant}-g${CI_COMMIT_SHORT_SHA}-p${CI_PIPELINE_IID}.tar.gz`,
   解决"版本号不变导致新构建被 skip 静默丢弃"的阻塞问题。URL 保持确定性可寻址:
   `{CI_API_V4_URL}/projects/{id}/packages/generic/{name}/{X.Y.Z}/{上述文件名}`。
2. **包内注入契约文件**:`gen_manifest.py` 在打包后解包 → 写入 `manifest.yaml`(按 `ci/variants.yaml` 渲染)+ `files.sha256` → 重打包重命名。生成后立即用 `manifest.schema.json` 校验,不合法 fail pipeline。
3. **bundle 聚合**:每个 build job 输出 `dist/meta/{variant}.json`(job artifact);新增 `publish:bundle` job 聚合为 `bundle-g{sha}.json` 上传 Registry。**8 个 meta 不齐全则不发 bundle**(挡住被 interruptible 打断的残缺构建)。
   **2026-07-22 演进:变体级触发(kick)**。build job 上传成功后直发
   `POST /kick`(meta 原样透传,复用 webhook 密钥),Trigger 校验(形态 +
   URL 归属 + Registry 探活)后起**单变体 workflow**(ID 含 variant,重复 kick
   由 Temporal 去重)——一个包编好即测,不等全部 8 个包与 pipeline success。
   bundle 保留为发布完整性断言;`TRIGGER_PIPELINE_WEBHOOK=false` 后 pipeline
   success webhook 仅记录不再起完整 workflow(防双跑)。触发与设备解耦:
   fleet 无匹配设备的变体由 SelectTestSpecs 秒级跳过(任意 OS/板型,CI 不改)。
4. Linux 变体第一阶段不进设备测试链路(SSH Adapter 属 Phase 4),但 Manifest 照常生成。

## 7. 核心契约(v1,放入 contracts/,以 JSON Schema 为准)

### 7.1 Plan DSL(Hermes → Runtime)

```json
{
  "plan_version": 1,
  "plan_id": "pln_20260716_001",
  "origin": { "type": "manual_nl|gitlab_event|template", "user": "...", "raw_text": "..." },
  "goal_summary": "一句话目标",
  "build": { "project": "algo-super-sdk", "ref": "master", "targets": ["aarch64_Android_SNPE_2.21"], "build_type": "Release" },
  "tests": [
    { "test_id": "t1", "suite": "snpe-smoke",
      "device_selector": { "soc": "QCM6125", "capabilities": ["hexagon"] },
      "depends_on": [] }
  ],
  "policies": {
    "on_infra_error": { "retry": { "max_attempts": 2, "backoff_sec": 30 } },
    "on_signature": [ { "signature_id": "cpu_fallback", "retry": false, "actions": ["analyze","notify"] } ],
    "on_perf_regression": { "baseline": "branch_last_n_passed:5", "threshold_pct": 10, "action": "block_and_notify" },
    "on_test_failed": { "retry": false, "actions": ["analyze","notify"] }
  },
  "notify": { "channels": ["feishu:oc_xxx"], "on": ["terminal_state"] },
  "constraints": { "deadline_min": 60, "max_devices": 1 }
}
```

### 7.2 Manifest(打包期生成,包内 manifest.yaml)

```yaml
manifest_version: 1
artifact: { project, commit, pipeline_id, platform: <variant>, build_type }
requirements: { os: android, abi: arm64-v8a, soc: [QCM6125], capabilities: [hexagon], min_free_storage_mb: 512 }
deploy:
  workdir: /data/local/tmp/algo-super-sdk
  files:                      # src 为包内相对路径,dst 为 workdir 相对路径
    - { src: bin/xxx, dst: bin/xxx, mode: "0755", sha256: "..." }
  env: { LD_LIBRARY_PATH: "{workdir}/lib", ADSP_LIBRARY_PATH: "{workdir}/lib/dsp;/system/lib/rfsa/adsp" }  # SNPE 变体
test:
  entry: ./run.sh
  args: ["--suite","snpe-smoke","--output","results/"]
  timeout_sec: 900
  success: { exit_code: 0, require_files: [results/result.json] }
  failure_signatures:         # 与 Plan 的 on_signature 通过 id 关联
    - { id: cpu_fallback,  where: logcat, pattern: "Falling back to CPU", classify: MODEL }
    - { id: native_crash,  where: logcat, pattern: "Fatal signal|tombstone", classify: CODE }
collect: [results/result.json, results/junit.xml, logs/*.log, dumps/**]
cleanup: { remove_workdir: true, keep_on_failure: true }
```

### 7.3 result.json(测试脚本产出,Client 回传)

```json
{
  "result_version": 1, "task_id": "...", "attempt": 1,
  "status": "COMPLETED", "exit_code": 0, "duration_sec": 412,
  "cases": { "total": 38, "passed": 38, "failed": 0, "skipped": 0, "failures": [] },
  "signatures_hit": [],
  "metrics": { "latency_ms_p50": 12.4, "latency_ms_p99": 18.9, "peak_rss_mb": 214, "model_load_ms": 830 },
  "environment": { "serial": "...", "soc": "QCM6125", "android": "12" },
  "artifact": { "commit": "...", "pipeline_id": 0 },
  "attachments": [ { "name": "logcat.txt", "object_key": "runs/.../logcat.txt", "sha256": "...", "size": 0 } ]
}
```

## 8. API 契约

### 8.1 Runtime → Client Agent(局域网 HTTPS + mTLS)

```text
POST   /api/v1/tasks          派单 → 202 Accepted(仅确认"已入本地队列",后台执行)
       body: { task_id, idempotency_key, attempt,
               artifact: { url, sha256, auth: bearer|job_token },
               manifest_digest, device_serial, callback_base_url, presigned_uploads[] }
DELETE /api/v1/tasks/{id}     取消(尽力而为:kill 设备进程 + 清理)
GET    /api/v1/tasks/{id}     查询本地状态
GET    /api/v1/devices        设备列表: [{serial, state, props(soc/abi/android), workdir_free_mb}]
POST   /api/v1/diagnostics    白名单探测: { probe: adb_devices|logcat_tail|df|getprop, args_limited }
GET    /healthz
```

幂等:收到重复 `idempotency_key` → 返回已有任务状态,不重复执行。

### 8.2 Client → Runtime 回调(HTTPS)

```text
POST /callbacks/v1/heartbeat    每 10s:client 信息 + 设备状态 + 进行中 task_id 列表(即租约续期)
POST /callbacks/v1/task-events  状态迁移事件: { task_id, idempotency_key, seq, from, to, ts, detail }
POST /callbacks/v1/results      终态: result.json 内容 + attachments 对象键清单(附件已直传 MinIO)
```

回调可能重发,Runtime 按 `idempotency_key + seq` 去重;Temporal signal 只投递一次。

## 9. 状态模型(status 与 verdict 正交,不要合并)

```text
status (生命周期):
  CREATED → QUEUED → DISPATCHING → ACCEPTED
  → PREPARING → DOWNLOADING → DEPLOYING → RUNNING → COLLECTING
  → COMPLETED | FAILED | TIMEOUT | CANCELED          (终态)

verdict (终态后判定):
  PASSED | TEST_FAILED | PERF_REGRESSION | INFRA_ERROR | INCONCLUSIVE
  规则: TIMEOUT → INFRA_ERROR(除非签名命中更具体类别); CANCELED → INCONCLUSIVE
```

错误分类(判定优先由确定性规则完成,LLM 只补充解释):

| category | 判定来源 | 缺省对策(Plan 可覆盖) |
|---|---|---|
| INFRA | Runtime 判定(下载失败/ADB 断连/离线/租约过期) | 机械重试 ≤2;设备连续 3 次 → QUARANTINED |
| BUILD | GitLab pipeline 状态 | 不进设备测试;通知附编译错误摘要 |
| CODE | 签名 native_crash / junit 失败 | 不重试;分析 + 通知;MR 场景阻断 |
| MODEL/DELEGATE | 签名 cpu_fallback 等 | 不重试;分析 Delegate 分区;通知 |
| DEVICE | 签名 + 设备属性预检失败 | 换设备重试 1 次;仍失败 → 隔离 + 通知 |
| PERF | Runtime 基线比较(metrics 表,分支最近 5 次 PASSED 中位数) | 不重试;按 Plan 决定是否阻断 |
| UNKNOWN | 兜底 | 不重试;附件全保留;通知标"需人工" |

## 10. 关键参数缺省值(可配置,写入 config)

心跳 10s;离线判定 3 次丢失;任务租约 120s(心跳续期);ADB 命令级重试 2 次间隔 3s(仅幂等命令);任务级机械重试 max 2(仅 INFRA);设备隔离阈值连续 3 次 INFRA;产物下载超时 10min;私有 adb server 端口 **5137**(`ANDROID_ADB_SERVER_PORT`,Agent 内置固定版本 adb,自管生命周期,永不使用系统 5037)。

## 11. 数据模型(PostgreSQL,Temporal 自身表之外)

```text
plans(plan_id PK, plan_json JSONB, origin, created_by, created_at)
workflows(workflow_id PK, plan_id FK, status, started_at, ended_at)
tasks(task_id PK, workflow_id FK, test_id, attempt, idempotency_key UNIQUE,
      client_id, device_id, status, verdict, error_category, created_at, ended_at)
clients(client_id PK, host, version, last_heartbeat, status)
devices(device_id PK, serial UNIQUE, client_id FK, soc, abi, capabilities TEXT[],
        status: IDLE|BUSY|OFFLINE|QUARANTINED, fail_streak INT)
device_leases(device_id FK, task_id FK, lease_expires_at)   -- 行锁保证独占
artifacts(artifact_id PK, project, commit_sha, pipeline_id, variant, build_type,
          url, sha256, size, created_at)
results(task_id FK, exit_code, duration_sec, counts JSONB, metrics JSONB, attachments JSONB)
metrics(project, variant, device_model, suite, metric_name, value, task_id, ts)  -- 基线来源
decisions(decision_id PK, task_id, actor: hermes|human|rule, input_digest,
          model, prompt_version, output JSONB, created_at)   -- 一切裁决可回放
audit_log(actor, action, target, payload_digest, ts)
```

Client 本地 SQLite:`tasks(task_id, idempotency_key, state, manifest_path, started_at, ...)` + `events(seq, ...)`,每次状态迁移单事务落盘,崩溃重启后据此恢复。

## 12. 实施阶段(当前所处:Phase 2)

### Phase 0 — 契约(已随本文件给出草案,首个任务是把 §7/§8 物化为 contracts/ 下的正式 Schema/OpenAPI 并写校验测试)

### Phase 1 — 无 LLM 最小闭环(当前阶段,按此顺序做)

1. `contracts/` 三个 JSON Schema + 两个 OpenAPI,附正反例测试。
2. `ci/` 四个脚本(gen_manifest / variants.yaml / write_meta / gen_bundle)+ 业务仓库 `.gitlab-ci.yml` 增量改造(见 §6)。
   **Algo_Super_SDK 适配门禁**：按
   `docs/assessments/algo-super-sdk-packaging.md` 关闭全部 P0 整改项；代表性 Android
   测试包必须通过静态契约检查和原生 Windows 实机验收，之后 Trigger 才能将该业务
   仓库产物派发给 Client Agent。SDK 发布包不得未经裁剪和测试入口适配直接进入设备
   测试链路。
3. **`agent-cli` 先行**:不做 RPC Server,先做命令行
   `agent-cli run --package-url ... --sha256 ... --serial ...`,
   完整实现:下载 → 整包 sha256 校验 → 解压 → Manifest Schema 校验 → 设备预检(getprop 属性/df 空间)→ 清理旧现场 → adb push → chmod/env → 执行(超时控制,超时 kill 但仍收集)→ adb pull collect 列表 → 本地产出结果目录。
   目的:Windows+USB+ADB 是最大不确定段,用 CLI 手动踩完所有坑再套服务壳。
4. Temporal spike(1 周内 go/no-go):signal 接收、Activity 重试、杀进程后重放恢复,三个最小示例。
5. Trigger 服务:GitLab pipeline webhook(验签、去重)→ 拉 bundle-g{sha}.json → 登记 artifacts 表 → 启动 DeviceTestWorkflow。
6. DeviceTestWorkflow 主干:resolve_artifact → acquire_device → dispatch(POST Client,幂等键={workflow_id}:{task_id}:{attempt})→ await_result(signal,租约由心跳续期,过期按 on_infra_error)→ extract_evidence → **规则引擎**判 verdict(不接 LLM)→ release_device → 飞书纯文本通知。
7. agent-cli 套 RPC 壳(§8.1 API)+ 心跳/事件/结果回调 + MinIO 预签名直传 + Windows Service 化。

**Phase 1 DoD**:push 一次代码 → 15 分钟内飞书收到含 verdict 与日志链接的通知;三项故障注入(拔 USB / 杀 Agent 进程 / 重启 Runtime)均收敛到正确终态,零重复执行。

### Phase 2 — Hermes 接入

> 2026-07-21 决策变更:Hermes 层**复用 q-uat 现有 hermes-agent 服务**(见 §4),不再自研
> Agent 循环。Phase 2 设计需先明确:专用实例还是复用现有实例(albinsu/eason/rocklin
> 为个人实例,宜另起专用实例)、工具白名单与 Schema 输出校验在该平台上的落地方式。
> §3 硬性边界(Hermes 只经 Runtime、结构化输入、不在执行关键路径)不因复用而放宽。

Evidence Extractor 完整化(签名匹配 + 匹配处 ±50 行上下文 + junit 失败 + 指标差值 → evidence.json,几十 KB 级);Analyzer(LLM 分析 evidence → 结构化结论 → decisions 落库;Hermes 超时/不可用 → 规则引擎保底);飞书交互卡片(重试/忽略/隔离按钮 → Runtime signal);Planner v1(自然语言 → Plan DSL,服务端 Schema 校验不过打回重试 ≤3 次)。
**严禁把原始日志全量灌入 LLM;Hermes 按需通过 `fetch_log_range(attachment, start, end)` 工具取片段。**

### Phase 3 — 硬化
mTLS 双向 + Client 身份签发;全链路幂等键核验;≥10 场景故障注入矩阵(断电/网络分区/adb 僵死/下载中断/回调丢失/重复回调);审计完备;MinIO 生命周期(PASSED 日志 7 天,失败 90 天);Agent 版本上报 + 最低版本门禁。

### Phase 4 — 扩展
多设备并发调度;性能基线 MR 门禁;Linux 变体 SSH Adapter;Grafana 看板。

## 13. 工程约定

- Go 1.22+;错误处理用 wrapped errors;所有跨网络调用带 context 超时。
- 每个包含状态迁移的模块必须有表驱动的状态机单测;恢复路径必须有故障注入测试。
- 契约变更规则:只加字段不删字段;`*_version` 字段递增;Agent/Runtime 双向版本协商,版本过低拒绝派单。
- 配置:环境变量 + 配置文件;秘钥不落 Git。
- 提交信息用英文,注释中文英文皆可;公共 API 必须有 godoc。
- 时间一律 UTC 存储,展示层再本地化。

## 14. 明确禁止(实现时的红线)

- ❌ 给 Client Agent 加任意 shell/exec 接口("方便调试"也不行,调试走 diagnostics 白名单)。
- ❌ Hermes 直连 Client、直连 ADB、直连数据库写操作。
- ❌ 在 Runtime 中用轮询等待 Client 结果(必须用 Temporal signal)。
- ❌ 把 status 和 verdict 合并为一个枚举。
- ❌ 用 CMake 版本号作为产物唯一标识(必须文件名含 commit + pipeline iid)。
- ❌ 使用系统全局 adb server(5037);必须私有端口 5137。
- ❌ 附件经 Runtime 中转(必须预签名直传 MinIO)。
- ❌ 未经 Schema 校验就消费 Plan / Manifest / result.json。
