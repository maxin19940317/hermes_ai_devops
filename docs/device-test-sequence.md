# 设备测试链路时序图(目标设计基线 v1.0,已冻结 2026-07-24)

本图是**目标设计基线 v1.0**,用于指导 Trigger API、Temporal Workflow、Activity
Worker、Client Agent 四条开发线并行实施,并作为后续故障注入、接口验收和架构
审计的基准。与 2026-07-24 现状的差距见文末清单——实施时必须先分清
「照图新建」与「与现状对齐」。

## 设计原则

1. Client 回调先进入 Trigger/Callback API,不直接进入 Workflow。
2. Workflow 不直接访问数据库、MinIO、Client 或 Hermes,一切 I/O 经 Activity
   (唯一例外:规则引擎是纯函数,刻意留在 Workflow 内调用以保证重放确定性,
   且按 rule_version 路由,历史版本实现不得在有历史 Workflow 时删除)。
3. 保存结果与唤醒 Workflow 必须避免双写不一致:业务数据与 outbox 事件单事务写入,
   由独立 Outbox Relay 投递 Signal(至少一次,接收端幂等)。
   **关键事件(Result、Cancel、Human Decision)必须走事务性 Outbox;
   进度事件(TaskEvent)是非关键事件,允许 best-effort 直接 Signal**——
   它仅用于进度展示与 Workflow Query,不参与结果判定、租约判断或流程正确性,
   丢失不影响任务最终收敛;一旦未来需要根据进度事件做超时/跳转决策,
   必须先把 task-event 纳入 Outbox。
4. 产物地址应尽量精确(kick 载荷携带精确 URL/sha256),不长期依赖版本探测。
5. Client 不长期持有高权限凭据(只读 Deploy Token 替代 PRIVATE-TOKEN);
   预签名上传 URL 按需短期签发,不在派单时一次性生成。
6. Temporal History 保持精简:心跳只续数据库租约,不向 Workflow 发高频 Signal;
   Workflow 以租约到期 Durable Timer + CheckLease Activity 做低频检查(非轮询)。

## 时序图

```mermaid
sequenceDiagram
    autonumber

    actor U as 用户
    participant G as GitLab CI
    participant P as Package Registry
    participant T as Trigger / Callback API<br/>(两个进程 :18090/:18091)
    participant O as Outbox Relay
    participant W as DeviceTestWorkflow<br/>(Temporal)
    participant R as Runtime Worker<br/>(Activities)
    participant D as PostgreSQL
    participant M as MinIO
    participant A as Client Agent
    participant B as 开发板
    participant H as Hermes<br/>(analyze_bridge)
    participant F as 飞书

    Note over A,D: 常驻链路:Client 启动注册设备属性/能力;每 10s 心跳,<br/>携带租约所有权凭据只续 PostgreSQL 租约(原则 6),<br/>不向 Workflow 发 Signal。

    %% =========================
    %% 1. 构建与触发
    %% =========================

    U->>G: Push 代码
    G->>G: 编译、打包<br/>gen_manifest / gen_bundle
    G->>P: 上传 tar.gz + bundle.json

    alt 变体级触发(原则 4,主路径)
        G->>T: POST /kick<br/>精确 package/version/url/sha256
    else 整线触发(兜底;13.8 webhook 不带版本号,<br/>只能按 package_name 探测)
        G-->>T: Pipeline success webhook<br/>Secret Token 验签
    end

    T->>P: 获取 Bundle(kick 路径为精确地址)
    P-->>T: bundle.json
    T->>T: JSON Schema 校验
    T->>D: Upsert Artifact(幂等)
    T->>W: StartWorkflow<br/>ID=device-test-{project}-g{sha}-p{iid}

    Note over T,W: Workflow ID 冲突策略:运行中 → 拒绝重复;<br/>上次 COMPLETED → 拒绝重复;上次 FAILED/TERMINATED →<br/>只允许显式 retry(新 Run ID,workflow_attempt+1),<br/>普通 webhook 重放不得自动重启。

    %% =========================
    %% 2. 规格解析
    %% =========================

    W->>R: SelectTestSpecs Activity
    R-->>W: 可执行 specs + skipped(fleet 无匹配 /<br/>OS 未接入,如 Linux 留 Phase 4)
    W->>R: RecordSkipped Activity
    R->>D: 保存 SKIPPED 结论(审计)

    loop 每个可执行 Android 变体

        %% =========================
        %% 3. 获取设备(task_id 由 Workflow 确定性生成)
        %% =========================

        W->>W: task_id = {workflow_id}:{test_spec_id}:a{attempt}<br/>(重放不变,全链路聚合键)
        W->>R: AcquireDevice Activity
        R->>D: SELECT ... FOR UPDATE SKIP LOCKED<br/>懒回收已过期 BUSY 租约
        D-->>R: device + task_id + lease_id +<br/>lease_generation + expires_at=now+120s
        R-->>W: DeviceLease

        W->>R: CreateTask Activity
        R->>D: Upsert Task(幂等键 task_id 含 attempt)
        R-->>W: TaskRecord

        %% =========================
        %% 4. 派发任务(原则 5:不预签上传 URL)
        %% =========================

        W->>R: DispatchTask Activity
        R->>A: POST /api/v1/tasks<br/>Idempotency-Key<br/>Manifest + 产物精确地址 + lease_id/<br/>generation + upload_request_endpoint
        A->>A: 查询本地 SQLite 幂等记录

        alt 相同 Idempotency-Key 已存在
            A-->>R: 返回原 task_id 和当前状态
        else 新任务
            A->>A: SQLite 保存 ACCEPTED
            A-->>R: 202 Accepted
        end

        R-->>W: Dispatch accepted

        %% =========================
        %% 5. Client 准备与部署
        %% =========================

        A->>P: 下载产物(精确 URL +<br/>只读 Deploy Token,原则 5)
        P-->>A: 测试包
        A->>A: Manifest Schema 校验<br/>SHA-256 校验和缓存

        Note over A,W: 进度 Signal 只在状态迁移时发送,且为 best-effort<br/>非关键事件(原则 3/6):PREPARING/DEPLOYING/RUNNING/COLLECTING

        A->>T: POST task-events(PREPARING + seq)
        T->>D: 保存事件(task_id+seq 去重)
        T->>W: SignalTaskEvent(PREPARING,best-effort)

        A->>B: ADB Preflight(私有 ADB Server :5137)
        B-->>A: state / getprop / df / capabilities

        A->>T: POST task-events(DEPLOYING + seq)
        T->>D: 保存事件
        T->>W: SignalTaskEvent(DEPLOYING,best-effort)

        A->>B: adb -P 5137 -s serial push<br/>chmod / 准备工作目录

        %% =========================
        %% 6. 执行测试
        %% =========================

        A->>T: POST task-events(RUNNING + seq)
        T->>D: 保存事件
        T->>W: SignalTaskEvent(RUNNING,best-effort)

        A->>B: adb shell 执行测试<br/>设备侧 timeout=900s

        par Client 心跳(只续 DB 租约,校验所有权)
            loop 每 10 秒
                A->>T: heartbeat([{task_id, attempt,<br/>lease_id, lease_generation}])
                T->>D: UPDATE device_leases …<br/>WHERE lease_id AND task_id AND attempt<br/>AND client_id AND lease_generation<br/>AND released_at IS NULL
                alt 影响行数 = 1
                    D-->>T: 续租成功(不发 Workflow Signal)
                else 影响行数 = 0
                    D-->>T: LEASE_NOT_OWNED
                    T-->>A: Client 停止操作该任务
                end
            end
        and Workflow 低频租约检查(非轮询)
            W->>W: Durable Timer 至 lease_expires_at
            W->>R: CheckLease Activity(Timer 到期才触发)
            R->>D: 读 lease_expires_at
            alt 租约已续期
                R-->>W: 新 expires_at → 重设 Timer
            else 租约已过期
                R-->>W: 进入 INFRA_ERROR 处理
            end
        end

        %% =========================
        %% 7. 收集与上传(原则 3:Outbox 防双写;原则 5:按需预签)
        %% =========================

        B-->>A: 退出码 + result.json<br/>junit / stdout / stderr / logcat
        A->>B: ADB Pull

        A->>T: POST upload-requests(待传附件清单)
        T-->>A: 短期预签名 PUT URL(10–30min)
        A->>M: 预签名 PUT 直传附件
        M-->>A: Object Key + ETag

        A->>T: POST /callbacks/v1/results<br/>result.json + attachment refs

        T->>T: Result Schema 校验 + 去重
        T->>D: 单事务:Upsert Result +<br/>Insert Outbox(UNIQUE event_key)
        D-->>T: Commit

        O->>D: Claim unpublished outbox rows
        O->>W: SignalTaskResult(task_id)<br/>(至少一次,接收端幂等)
        O->>D: Mark published(失败则 attempts+1<br/>记 last_error 后重试)

        W->>R: LoadResult Activity
        R->>D: 读取权威 Result
        D-->>R: ResultRecord
        R-->>W: ResultRecord

        %% =========================
        %% 8. 规则判定(原则 2:版本化纯函数)
        %% =========================

        W->>W: rules.Decide(plan.rule_version 路由,<br/>workflow 内纯函数,§9)
        W->>R: SaveDecision Activity
        R->>D: actor=rule

        Note over W,R: verdict 最终判定权属规则引擎,<br/>Hermes 不可覆盖;历史 rule_version 实现保留。

        %% =========================
        %% 9. 非通过结果分析(Evidence 可回放)
        %% =========================

        alt verdict != PASSED
            W->>R: ExtractEvidence Activity
            R->>M: 流式扫描日志全文(不整载入内存),保留<br/>签名命中 ±50 行 + 首个 error + 尾部 N 行
            M-->>R: 日志内容
            R->>M: evidence.json 上传 MinIO(≤96KB,<br/>随 Decision 保留周期)
            W->>R: SaveEvidenceSnapshot Activity
            R->>D: evidence_snapshots(object_key + sha256 +<br/>extractor_version + task_id/attempt)

            W->>R: Analyze Activity
            R->>H: POST /analyze<br/>evidence.json + rule category
            H->>H: hermes -z(工具全禁)+ Schema 校验,<br/>不合法打回 ≤3 次(bridge 内部)

            H-->>R: summary / root_cause /<br/>next_actions / confidence
            R->>D: SaveDecision(actor=hermes,<br/>prompt_version + evidence_snapshot_id)
            R-->>W: HermesAnalysis
            Note over R,D: decisions 只记录真正发生的决策;<br/>evidence 快照独立存 evidence_snapshots。
        end

        %% =========================
        %% 10. 重试与释放(失败归因)
        %% =========================

        alt INFRA_ERROR 且 attempt < 上限(机械重试 ≤2)
            W->>R: FinishTask + ReleaseDevice Activity
            R->>D: 按归因计数:设备级 INFRA →<br/>device_fail_streak+1;<br/>Client/网络级 → client_fail_streak+1
            W->>W: 下一 attempt(回到获取设备)
        else 终态
            W->>R: FinishTask + ReleaseDevice Activity
            R->>D: PASSED / TEST_FAILED / PERF_REGRESSION<br/>→ fail_streak 清零;<br/>device_fail_streak 连续 3 → QUARANTINED
        end
    end

    %% =========================
    %% 11. 通知
    %% =========================

    W->>R: Notify Activity
    R->>F: verdict + 耗时 + 用例统计,<br/>非 PASSED 附 Hermes summary 行
    F-->>U: 飞书通知
```

## 组件职责

| 组件 | 核心职责 |
|---|---|
| Trigger / Callback API | 接收外部 HTTP、验签、Schema 校验、去重、租约所有权校验、单事务落库(业务数据 + outbox)(部署为两个进程 :18090/:18091) |
| Outbox Relay | 独立进程:claim 未投递 outbox 行 → 发 Temporal Signal(至少一次)→ 标记已投;失败重试并记录 last_error,可监控 |
| Temporal Workflow | 维护流程、等待 Signal、Durable Timer 租约检查、控制重试与步骤顺序;不直接访问任何外部系统 |
| Runtime Worker | 以 Activity 执行所有数据库、网络、MinIO、Client、Hermes、飞书操作 |
| PostgreSQL | Artifact、Task、Result、Lease、Decision、Evidence Snapshot、Outbox 的业务权威数据源 |
| Client Agent | 确定性完成下载、ADB、测试和结果上传(本地 SQLite 幂等) |
| 规则引擎 | 版本化纯函数,决定 status、verdict 和错误类别;在 Workflow 内按 rule_version 路由调用 |
| Hermes | 对已确定的事实进行根因解释和后续建议,不可覆盖 verdict |
| MinIO | 保存大日志/Dump 附件与 evidence.json 快照(后者随 Decision 保留周期) |

## 与现状的差距清单(2026-07-24)

| # | 目标设计 | 现状 | 行动 |
|---|---|---|---|
| 1 | Signal Outbox 单事务 + 独立 Relay(原则 3) | 写库成功后直接 signal,失败靠租约/硬超时兜底 | **新建**:outbox 表(UNIQUE event_key)+ callbacks 单事务 + Relay 进程(重试/监控) |
| 2 | LoadResult 权威读 | 结果本体随 signal 载荷传递,不二次查库 | 配合 #1 引入 |
| 3 | 心跳只续 DB,Workflow 用 Timer + CheckLease(原则 6) | 每 10s 对 active task 发 SignalTaskHeartbeat,长任务污染 History | **改造**:callbacks 停发心跳 signal;workflow 改为到期 Timer + CheckLease |
| 4 | SignalTaskEvent 仅状态迁移时发送,best-effort | task-events 仅落库,workflow 不消费 | 配合 #3 引入(进度可视) |
| 5 | RecordSkipped 落库 | SKIPPED 只在 workflow 输出与通知中 | 可选(审计) |
| 6 | Evidence 快照独立存 evidence_snapshots + MinIO | evidence 瞬态,decisions 只存 sha256 | **新建**:evidence_snapshots 表 + ExtractEvidence 上传 MinIO + SaveEvidenceSnapshot |
| 7 | Rule Engine 版本化(plan.rule_version 路由) | rules.Decide 无版本,升级即破坏重放确定性 | **新建**:plan/契约加 rule_version,版本路由,历史实现保留 |
| 8 | 预签名 URL 按需签发(收集时请求) | 派单时一次性签发(1h TTL),长任务可能过期 | **改造**:callbacks 加 upload-requests 端点,派单载荷改 endpoint |
| 9 | 日志流式全扫(不只尾部 8MB) | readWindow 只保留尾部 8MB,前部错误可能丢失 | 改造 evidence 提取:流式扫描,只留命中上下文/首 error/尾部 |
| 10 | 失败归因 device/client 分离 + 明确重置规则 | 单一 fail_streak,网络问题可能误隔离设备 | store 扩 client_fail_streak,定义归因与清零语义 |
| 11 | Workflow ID 冲突策略精细化(失败仅显式 retry) | AllowDuplicateFailedOnly:失败 workflow 可被 webhook 重放自动重启 | Trigger 侧区分显式 retry 与普通重放 |
| 12 | Client 只读 Deploy Token(原则 5) | bearer PAT(高权限) | 配置变更:read_package_registry Deploy Token 替换 `ARTIFACT_AUTH_TOKEN` |
| 13 | kick 精确产物地址(原则 4) | `/kick` 与 `ci/kick.py` 已实现;业务仓库 CI 未接线;webhook 兜底保留 | 业务仓库 CI 接 `kick.py` + 配 `TRIGGER_KICK_URL/TOKEN` |
| 14 | task_id 由 Workflow 确定性生成 | **已对齐**(devicetest.go:`{workflow_id}:{test_id}:a{attempt}`) | 无 |
| 15 | 心跳续租校验所有权(lease_id/task/attempt/client/generation) | RenewLease 仅按 device_id+task_id 匹配 | **改造**:device_leases 扩 lease_id/generation/released_at,心跳载荷与 WHERE 条件对齐,失配返回 LEASE_NOT_OWNED |

## 实施优先级(四批,按依赖排序)

**第一批:可靠事件链路**(全系统可靠性基础,决定后续共同底座)

1. outbox 表
2. Callback API 单事务写入 Result + Outbox
3. 独立 Outbox Relay
4. Workflow LoadResult 权威读取
5. Result Signal 接收端幂等

**第二批:Temporal History 与重放安全**

1. 停止 Heartbeat Signal(含租约所有权校验,#15)
2. Durable Timer + CheckLease Activity
3. plan.rule_version
4. rules/v1、rules/v2 版本路由
5. Workflow ID 显式重试策略

**第三批:Evidence 和凭据硬化**

1. Evidence 全日志流式扫描
2. Evidence Snapshot 持久化(evidence_snapshots + MinIO)
3. 上传前按需申请 Presigned URL
4. Deploy Token 替换高权限 PAT
5. device/client failure streak 分离

**第四批:非关键审计增强**

1. TaskEvent 状态迁移 Signal(best-effort)
2. RecordSkipped 落库
3. 进度可视化
4. Outbox backlog 和失败监控

## 关键不变量(§3/§9/§10/§14,开发与评审红线)

- **§3 边界**:Hermes 与 Client Agent 无直接通信,一切经 Runtime 中转;设备操作由
  Manifest 白名单声明,Client 不提供任意 Shell。
- **§9 正交**:status(生命周期)与 verdict(终态判定)分离;verdict 由版本化规则引擎
  判定,Hermes 仅补充解释,Analyzer 不可用 → 规则引擎保底。
- **§10 租约**:120s,心跳携带所有权凭据(task_id/attempt/lease_id/generation)只续
  数据库租约,失配返回 LEASE_NOT_OWNED;Workflow 以到期 Timer + CheckLease 低频检查;
  过期 = 持有者失联,由 AcquireDevice 懒回收,无后台清扫。
- **§14 禁轮询**:workflow 等结果只收 signal;CheckLease 由租约到期时间驱动,非高频
  轮询;重复 webhook/重复回调/outbox 重复投递全部幂等去重。
- **事件分级**:关键事件(Result/Cancel/Human Decision)必须走事务性 Outbox;
  进度事件(TaskEvent)允许 best-effort Signal,丢失不影响收敛。
- **决策可回放**:evidence 快照独立持久化(evidence_snapshots + MinIO),
  decisions 只记录真正发生的决策,引用 evidence_snapshot_id。

## Outbox 表结构(实施参考)

```text
id              BIGSERIAL PRIMARY KEY
aggregate_type  TEXT        -- task / device / ...
aggregate_id    TEXT
event_type      TEXT        -- task-result / cancel / ...
event_key       TEXT        -- 幂等键,如 {task_id}:result
payload         JSONB
created_at      TIMESTAMPTZ
published_at    TIMESTAMPTZ -- NULL = 未投递
attempts        INT
last_error      TEXT
UNIQUE(event_key)
```

Relay 必须允许重复发送(Temporal Signal 接收端幂等);published_at 标记
与 Signal 成功之间允许崩溃,重投由接收端去重兜底。

## evidence_snapshots 表结构(实施参考)

```text
evidence_id       TEXT PRIMARY KEY
task_id           TEXT        -- 含 attempt 的全链路聚合键
attempt           INT
object_key        TEXT        -- MinIO 对象键
sha256            TEXT
extractor_version TEXT
created_at        TIMESTAMPTZ
```

原始大日志可按生命周期清理;evidence 快照(≤96KB)保留周期与 Decision 一致。
