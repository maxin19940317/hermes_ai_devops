# 设备测试链路时序图(修正版,对齐 CLAUDE.md §6/§8/§9/§10/§14 与当前实现)

与早期设想的差异要点:Runtime 不触发 pipeline(由 push/webhook 驱动)、
日志附件经 MinIO 预签名 URL 直传(不过 Runtime)、结果等待是 Temporal
signal 驱动(禁止轮询)、verdict 判定权在规则引擎(Hermes 只补充解释)、
飞书通知由 Runtime 组文案发送(Hermes summary 只是其中一行)。

```mermaid
sequenceDiagram
    autonumber
    participant U as 用户
    participant G as GitLab CI
    participant P as Package Registry
    participant T as Trigger(W)
    participant W as DeviceTestWorkflow<br/>(Temporal)
    participant D as Postgres
    participant M as MinIO
    participant A as Client Agent
    participant B as 开发板
    participant H as Hermes<br/>(analyze_bridge)
    participant F as 飞书

    Note over A,D: 常驻:启动注册(设备属性/能力),心跳 10s;<br/>心跳即租约续期(先续 DB 租约,再 signal workflow)

    U->>G: push 代码
    G->>G: 编译、打包、gen_manifest/gen_bundle
    G->>P: 上传产物(tar.gz + bundle.json)
    alt 变体级触发(§6.3,CI 接 kick.py 后)
        G->>T: POST /kick(单包 meta,一编好即测)
    else 整线触发(当前)
        G-->>T: pipeline success webhook(Secret Token 验签)
    end
    T->>P: 拉 bundle(按 package_name 探测版本)
    T->>T: Schema 校验 + 登记 artifacts(幂等 upsert)
    T->>W: 启动 DeviceTestWorkflow<br/>(ID=device-test-{project}-g{sha}-p{iid},重复投递去重)

    W->>W: SelectTestSpecs:fleet 无匹配设备 /<br/>OS 未接入(Linux,Phase 4)→ 直接 SKIPPED

    loop 每个待测 Android 变体(INFRA 机械重试 ≤2,§9/§10)
        W->>D: AcquireDevice(FOR UPDATE SKIP LOCKED;<br/>BUSY+租约过期 → 懒回收)
        D-->>W: 租约(120s,心跳续期)
        W->>D: CreateTask(幂等键 task_id)
        W->>A: POST /tasks(manifest + 预签名上传 URL)
        A-->>W: 202 Accepted

        A->>P: 下载产物(bearer,PRIVATE-TOKEN)
        P-->>A: 测试包(sha256 校验)
        A->>W: 回调 PREPARING(task-events)
        A->>B: ADB Preflight(私有 adb server 5137)
        A->>W: 回调 DEPLOYING
        A->>B: ADB Push
        A->>W: 回调 RUNNING
        A->>B: ADB Shell 执行测试(timeout 900s)

        par 执行期间
            A->>W: 心跳(active_task_ids)→ 续租 + 进度事件
        and
            W->>W: await_result:signal 驱动,禁轮询(§14);<br/>租约过期/硬超时 → INFRA 重试
        end

        B-->>A: 退出码 + 结果文件
        A->>B: ADB Pull
        A->>M: 附件直传(logcat/stdout/stderr/junit,预签名 PUT)
        A->>W: POST /callbacks/v1/results(result.json)
        W->>D: Schema 校验 → SaveResult(去重)→ signal 唤醒 workflow

        W->>W: 规则引擎判 verdict(§9,判定权永远在此)
        W->>D: SaveDecision(actor=rule,可回放 §11)
        alt verdict ≠ PASSED(Phase 2)
            W->>M: ExtractEvidence(拉日志,签名命中 ±50 行,<br/>96KB 预算,严禁全量灌 LLM)
            W->>H: POST /analyze(evidence.json + 规则类别)
            H->>H: hermes -z(工具全禁)→ Schema 校验(打回 ≤3 次)
            H-->>W: analysis(summary/root_cause/next_actions)
            W->>D: SaveDecision(actor=hermes,evidence 摘要+prompt 版本)
        end
        W->>D: FinishTask + ReleaseDevice<br/>(INFRA → fail_streak+1,连续 3 次 QUARANTINED)
    end

    W->>F: Notify:精练文案(verdict + 耗时用例 +<br/>非 PASSED 附 hermes summary 行)
```

## 关键不变量(图中未展开的红线)

- **§3 边界**:Hermes 与 Client Agent 无直接通信,一切经 Runtime 中转;设备操作由
  Manifest 白名单声明,Client 不提供任意 Shell。
- **§9 正交**:status(生命周期)与 verdict(终态判定)分离;verdict 由规则引擎判定,
  Hermes 仅补充解释,Analyzer 不可用 → 规则引擎保底。
- **§10 租约**:120s,心跳续期(DB + workflow signal 双侧);过期 = 持有者失联,
  由 AcquireDevice 懒回收,无后台清扫。
- **§14 禁轮询**:workflow 等结果只收 signal;重复 webhook/重复回调全部幂等去重。
