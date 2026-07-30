# DevOps → PM 升级通道设计(kanban_bridge)

**日期:** 2026-07-30
**状态:** 待批准

## 1. 背景与决策

测试链路的诊断能力已经闭环(规则引擎判 verdict + Hermes 给根因 + evidence 快照可回放),
但「诊断 → 修复」仍靠人肉搬运:像「gesture DSP 不支持 unsigned PD」「seg 模型缺量化参数」
这类结论,需要有人去 algo_super_sdk 改代码。用户的 hermes 四人组(PM/开发/架构/测试)
恰好接得住这一段,缺的只是从 devops 到 `tobias_pm` 的结构化通道。

已验证的事实(2026-07-30 侦察):

- `tobias_pm` 在 q-uat 的 `hermes-rocklin` 容器(WebUI :9221,飞书 gateway 在线);
- `algo_super_sdk` board 已存在且 PM 活跃;
- `hermes kanban --board algo_super_sdk create --assignee tobias_pm --idempotency-key …`
  全链路实测成功(试点任务 `t_01343ec8`),重复提交返回同一 task id(去重内建);
- WebUI REST API 走 cookie 会话,api_server 平台未启用,webhook 平台无订阅——
  均不作为投递通道。

关键决策:

1. **投递通道 = kanban(docker exec hermes-rocklin),不是 WebUI API / webhook / api_server。**
   kanban 的语义(持久、原子认领、assignee、idempotency-key)就是为跨 profile 派活造的;
   其余通道要么要发明会话管理,要么语义不符。
2. **worker 不直接持 docker 权限**,经宿主侧薄桥 `kanban_bridge` 中转(与 analyze_bridge
   同构:FastAPI + Bearer + 无 LLM)。桥只做信封 → CLI 翻译,无状态。
3. **升级时机在 Hermes 分析之后**,且只升级「有稳定诊断的非 INFRA 失败」。INFRA 类
   (设备/网络/Runtime 抖动)由机械重试和归因计数负责,不打扰 PM。
4. **升级是 fire-and-forget 的旁路**:任何环节失败(bridge 不可达、建单失败)只记日志,
   主链路(测试、判定、通知)不受任何影响(§3:agent 不在执行关键路径)。
5. **审计落 decisions 表**(actor='escalation'):失败任务的 task_id 天然存在,
   满足 decisions 的外键约束,不需要新表。

不采用的方案:

- worker 容器挂 docker socket 直接 exec:权限面过大,违反最小权限。
- WebUI cookie 会话:需要发明登录态管理,且 WebUI 属第三方(升级即碎)。
- 自由文本任务体:信封必须结构化自包含(沙箱隔离,PM 侧够不到我们的 DB/MinIO)。

## 2. 架构

```
DeviceTestWorkflow(非 PASSED 终态)
  → 规则引擎 verdict + Hermes 分析(既有)
  → Escalate activity(新增,旁路)
      │  门槛:category ∈ {CODE,MODEL,DELEGATE,DEVICE} 且 hermes.confidence ≥ 阈值
      ├─ 组信封(envelope JSON,§3)
      ├─ POST kanban_bridge /escalations(Bearer)
      │     → docker exec hermes-rocklin hermes kanban --board algo_super_sdk create
      │        --assignee tobias_pm --idempotency-key <派生,§4>
      ├─ 幂等:重复升级返回同一 kanban task id(kanban 内建)
      └─ 落 decisions(actor='escalation',output={kanban_task_id, idempotency_key})
  → 主链路继续(通知等),永不阻塞
```

## 3. 任务信封(Schema v1)

`contracts/escalation.schema.json`,bridge 端校验(与 command.schema.json 同形态):

```json
{
  "escalation_version": 1,
  "source": {
    "project": "aios/algo_super_sdk",
    "commit": "def41bec",
    "pipeline_iid": 53,
    "variant": "aarch64_Android_SNPE_2.21",
    "task_id": "device-test-…-p53:aarch64_Android_SNPE_2.21:a1"
  },
  "rule": { "verdict": "TEST_FAILED", "category": "DELEGATE", "reason": "signature hit: dsp_unavailable" },
  "hermes": { "summary": "…", "root_cause": "…", "suggested_category": "DELEGATE",
              "confidence": 0.92, "next_actions": ["…"] },
  "evidence": { "snapshot_id": "device-test-…:a1", "object_key": "evidence/…/evidence.json",
                "sha256": "…", "extractor_version": "2" },
  "ruled_out": ["测试链路 INFRA(outbox/租约/设备调度验证正常)"],
  "reproduce": "push 任意 master commit 触发 pipeline,该变体必现"
}
```

约束:`escalation_version`/`source`/`rule` 必填;`hermes` 可空(分析降级时仍允许升级,
但门槛改为仅 category + 无 confidence 要求时不升级——见 §5);正文渲染(title/body)
由 bridge 从信封生成,devops 不直接写自由文本。

## 4. 幂等键与防风暴

```text
idempotency-key = "devops-escalation:{project}:{commit}:{variant}:{signature_or_category}"
```

- `signature_or_category`:有签名命中取签名 id(如 dsp_unavailable),否则取 rule category。
- 同根因在不同 pipeline 重复失败 → 同 key → kanban 返回已有任务,不新建;
  bridge 此时对已有任务追加一条 comment(新 pipeline 信息),保持单一事实源。
- 不同 commit 同根因:key 含 commit 会各建一条——刻意如此(回归 vs 旧病未愈,
  语义不同,由 PM 归并)。

## 5. 升级门槛

同时满足才升级:

1. rule verdict ≠ PASSED 且 category ∈ {CODE, MODEL, DELEGATE, DEVICE};
2. hermes 分析成功且 `confidence ≥ ESCALATION_MIN_CONFIDENCE`(缺省 0.7);
3. 该 workflow 此前未对同一 task 升级过(decisions 查 actor='escalation' 判重)。

Hermes 不可用/低置信/INFRA 类:不升级,现状行为逐字节不变。

## 6. kanban_bridge 薄桥

`hermes/kanban_bridge/`(Python FastAPI,仿 analyze_bridge 目录结构):

- `POST /escalations`:Bearer 校验 → escalation.schema.json 校验 → 渲染 title/body →
  `docker exec hermes-rocklin hermes kanban --board algo_super_sdk create …` →
  返回 `{kanban_task_id, created|existing}`。
- `POST /escalations/comment`(同 key 已存在时由 create 路径内部调用,不对外)。
- 配置:`KANBAN_BRIDGE_TOKEN`(必填)、`KANBAN_CONTAINER`(缺省 hermes-rocklin)、
  `KANBAN_BOARD`(缺省 algo_super_sdk)、`KANBAN_ASSIGNEE`(缺省 tobias_pm)、
  `KANBAN_TIMEOUT_SEC`(缺省 30)。
- 启动方式照搬 `start-analyze-bridge`(env 文件 + pidfile + nohup uvicorn),
  跑在 q-uat 宿主 tobias 账号(docker 组),不在任何容器内。
- 安全:桥只接受契约内的信封,命令行全部由 bridge 用参数数组(非 shell 拼接)构造;
  title/body 长度上限(title ≤ 200,body ≤ 8KB,截断标记)。

## 7. 审计与红线

- 每次升级尝试在 decisions 落行(actor='escalation'):output 含 kanban_task_id、
  idempotency_key、created/existing、bridge 响应码;失败落 error 字段。可回放
  「这个失败有没有派给 PM、派的是哪条」。
- **红线(与全系统一致):**
  - PM/agent 的产出(代码、建议)必须走 MR + 人 review,devops 不提供任何直接合入路径;
  - devops 不执行 agent 发来的任意指令——反向通道(若有)只能走飞书指令层同一套
    封闭枚举 + 白名单;
  - 升级失败不阻断、不重试(主链路优先);风暴由幂等键 + 门槛双重约束。

## 8. 配置

| 变量 | 缺省 | 说明 |
|---|---|---|
| `ESCALATION_ENDPOINT` | 空 | kanban_bridge URL;空 = 升级禁用(现状) |
| `ESCALATION_TOKEN` | 空 | Bearer 共享密钥 |
| `ESCALATION_MIN_CONFIDENCE` | `0.7` | hermes 置信度门槛 |
| `ESCALATION_BOARD` | `algo_super_sdk` | 目标 board(桥侧可再覆盖) |

compose 透传(worker environment),`.env.example` 加注释示例。

## 9. 测试

- **契约**:escalation.schema.json 正反例(缺 source、category 非法、超长 body)。
- **bridge pytest**(假 docker exec):信封校验失败 400、鉴权 401、create 成功渲染、
  重复 key → existing + comment、CLI 非零 → 502、body 截断。
- **Escalate activity / workflow 表驱动**:四类 category 升级/不升级矩阵、confidence 门槛、
  同 task 判重、bridge 失败只记日志不影响 verdict/通知、decisions 落行内容。
- **验收**:重放一个已知失败变体(gesture DSP)→ kanban 出现任务且 assigned tobias_pm;
  再重放一次 → 不新建,只有 comment;decisions 两行 actor='escalation' 可查。

## 10. 后续(不在本轮)

- PM 完成态回流(kanban complete → devops 标记「已修复待验证」→ 下个 pipeline 自动验收
  后回写结论)——需要先定义回流通道,属路线 B 的一部分。
- 升级对象从 tobias_pm 泛化为按 category 路由(CODE→开发,MODEL→架构评审)。
- workflow_runs 落地后,信封的 source 段切换到权威 run 索引。
