# 预签名 URL 按需签发设计（差距 #8）

日期：2026-07-29

状态：待批准

## 1. 背景

今天预签名在**派单时**一次性签发（`activity/presign.go`）：worker 为固定的 5 个键
（`runs/{task_id}/{result.json,junit.xml,logcat.txt,stdout.log,stderr.log}`）各签一个
PUT URL，TTL 取 `MINIO_PRESIGN_TTL`（缺省 1h），随 dispatch 载荷下发；Agent 收集完成后
按 `presigned_uploads[]` 逐个 PUT。

这个形态有两个已知缺陷：

1. **长任务的 URL 会过期**（差距 #8）。签名在派单那一刻生成，而任务时长上限由 Manifest 的
   `test.timeout_sec` 决定（`ci/variants.yaml` 里有 900s 的，加上下载、部署、收集，逼近
   1h 并非不可能）。URL 过期后附件静默留在本地——`deploy/README.md` 已经把这条写成
   运维注意事项，说明它是被容忍而非被解决的。
2. **glob 命中的文件从来没被上传过**。`ci/variants.yaml` 的 `collect` 声明是
   `results/result.json`、`results/junit.xml`、`logs/*.log`、`dumps/**`——后两条是 glob，
   派单期无法预知文件名，所以固定键集之外的一切都进不了 MinIO。注意受影响的不只是
   `dumps/**`：`logs/*.log` 同样一个都没上传过（固定键集里的 `stdout.log`/`stderr.log`
   是执行器自己的输出，不是 `logs/` 目录下的）。`presign.go:14` 与
   `docs/superpowers/specs/2026-07-21-agent-service-design.md:20` 都以 `CONTRACT-ISSUE`
   记录了这一点；Agent 侧的表现是 `tasks.go` 的一行日志"object_key 不在固定键集映射内,
   跳过上传"。

差距清单 #8 给出的方向是"callbacks 加 upload-requests 端点，派单载荷改 endpoint"。

## 2. 关键决策

1. **端点用租约凭据鉴权**（已与负责人确认，2026-07-29）。这一条不是可选的加固：callbacks
   今天**完全没有鉴权**（`handler.go` 的三个端点无 token、无 mTLS，靠 LAN 隔离，mTLS 属
   Phase 3）。现有端点是"接收数据"，而 upload-requests 是"**签发写入凭据**"——预签名
   PUT URL 就是一张往证据桶写东西的通行证。放在无鉴权入站面上，同网段任何人都能拿猜到的
   task_id 换取写入能力，而 task_id 是有规律的
   （`device-test-{project}-g{sha}-p{iid}:{variant}:a{n}`）。
   复用差距 #15 引入的租约所有权凭据（`lease_id` + `lease_generation`，派单时下发、
   心跳续租时已在校验），不新增任何秘密。
2. **顺带修掉 glob 洞**（已确认）。Agent 在收集时已经知道 `collect` 实际匹配到了哪些文件，
   把相对路径清单报给 Runtime 换 URL 即可。这是按需签发的自然结果，不做反而浪费。
3. **派单载荷保留 `presigned_uploads[]`，新增 `upload_request_url`**。契约只加不删；
   且保留它有第二个用处（见决策 4）。
4. **端点不可达时回退到派单时下发的固定键集 URL**。Runtime 在任务执行期间重启并非罕见，
   而改造前"URL 已在手"意味着 Runtime 短暂不可用不影响上传。若改成纯按需，Runtime 一抖
   就丢证据——那是拿一个新的失败模式换掉旧的。既然 `presigned_uploads[]` 因兼容而保留，
   回退是免费的。
5. **Runtime 逐个校验请求的 key**，只签发 `runs/{task_id}/` 前缀下的键，并限制数量与
   路径形态（见 §4.3）。

不采用的方案：

- 共享密钥 token（如 `/kick` 的 webhook 密钥）：简单，但全局密钥拿到就能给**任意** task
  签 URL，粒度远粗于租约。
- 不鉴权、等 Phase 3 mTLS：与现有端点一致，但把一个"收数据"的面变成"发凭据"的面，
  风险级别不同，不能沿用旧结论。
- 只修 TTL、键集仍固定：改动小，但 CONTRACT-ISSUE 继续挂着，而它正是本轮顺手能关掉的。
- 派单时不再签发、纯按需：见决策 4。

## 3. 范围

交付：

- `contracts/callbacks-api.openapi.yaml`：新增 `POST /callbacks/v1/upload-requests`
- `contracts/client-agent-api.openapi.yaml`：dispatch 载荷新增 `upload_request_url`
- Runtime：callbacks handler 新增端点 + 租约校验 + key 校验 + 签发
- Agent：收集后按实际文件请求 URL，失败回退派单时的固定键集
- 文档：差距 #8 状态、`deploy/README.md` 的 TTL 注意事项更新、两处 CONTRACT-ISSUE 关闭

不交付（明确排除）：

- 移除 `presigned_uploads[]`（契约只加不删；下线条件见 §7）
- callbacks 的通用鉴权/mTLS（Phase 3）
- 附件生命周期与保留策略（上一轮已做）
- Runtime 侧对上传内容的校验（sha256 由 Agent 在 `result.json` 的 attachments 里自报，
  与今天一致）

## 4. 契约

### 4.1 `POST /callbacks/v1/upload-requests`

请求：

```json
{
  "task_id": "device-test-algo-super-sdk-g9da3b9d9-p56-aarch64_Android_SNPE_2.21:aarch64_Android_SNPE_2.21:a1",
  "client_id": "c1",
  "device_id": "513cd3de",
  "attempt": 1,
  "lease_id": "device-test-...:a1",
  "lease_generation": 3,
  "files": ["results/result.json", "logs/run.log", "dumps/0001.bin"]
}
```

响应 200：

```json
{
  "uploads": [
    { "path": "results/result.json",
      "object_key": "runs/{task_id}/results/result.json",
      "url": "https://minio.lan:9000/...",
      "expires_at": "2026-07-29T09:12:00Z" }
  ],
  "rejected": [
    { "path": "../../etc/passwd", "reason": "path escapes task prefix" }
  ]
}
```

- `401`：租约凭据校验不通过（非当前持有者/租约已易主/已释放）
- `400`：请求形态非法（缺字段、`files` 为空或超上限）
- `503`：MinIO 未配置或签名失败（Agent 据此回退，见 §5.2）

**部分拒绝不是错误**：合法 key 照签，非法 key 进 `rejected` 并附原因。理由与
`presigned_uploads` 的降级语义一致——证据能传多少传多少，一个坏路径不该让整批失败。

### 4.2 dispatch 载荷新增字段

`contracts/client-agent-api.openapi.yaml` 的 `TaskDispatchRequest` 增加：

```yaml
        upload_request_url:
          type: string
          description: |
            按需签发端点的完整 URL(差距 #8),形如
            {callback_base_url}/callbacks/v1/upload-requests。
            Agent 收集完成后用它换取本次实际收集到的文件的预签名 PUT URL。
            为空 = Runtime 未启用按需签发,Agent 沿用 presigned_uploads[]。
```

`presigned_uploads[]` **保留**，其 description 加一句：新 Agent 仅在按需签发不可用时用它兜底。

### 4.3 Key 校验规则（Runtime 侧）

对 `files[]` 的每一项，依次：

1. 归一化：拒绝绝对路径、拒绝含 `..` 的路径段、拒绝空串、统一 `/` 分隔符
2. 拼成 `runs/{task_id}/{path}`，再次确认结果仍以 `runs/{task_id}/` 开头
   （防御归一化本身被绕过）
3. 数量上限 `UPLOAD_REQUEST_MAX_FILES`（缺省 64）——超出部分整体 400，不做截断
   （截断会让 Agent 以为传全了）
4. 单次请求只针对一个 `task_id`；`task_id` 与凭据里的必须一致

不校验文件是否真的存在于 Manifest 的 `collect` 声明内：Runtime 手里没有 Manifest
（它只有 `manifest_digest`），要校验就得把 Manifest 内容也传上来或存下来，成本与收益
不成比例。前缀约束已经保证写入不出 `runs/{task_id}/`，而那个前缀本来就是这个任务自己的
地盘。

## 5. 数据流

### 5.1 正常路径

```text
派单:  Runtime → Agent   presigned_uploads[](5 个固定键,兼容用)
                        + upload_request_url
                        + lease_id / lease_generation(已有)

执行:  Agent 跑测试,按 Manifest 的 collect 收集文件到 out_dir

收集后:Agent → Runtime   POST upload-requests
                        {task_id, client_id, device_id, attempt,
                         lease_id, lease_generation, files: [实际相对路径...]}
       Runtime          校验租约 → 校验每个 key → 逐个签发(TTL 同 MINIO_PRESIGN_TTL)
       Runtime → Agent  {uploads: [...], rejected: [...]}

上传:  Agent 按 uploads[] 逐个 PUT → 汇总 attachments → POST /callbacks/v1/results
```

关键点：URL 在**收集完成之后**才签发，此时距离上传只有秒级，TTL 过期问题从根上消失。

### 5.2 端点不可达的回退

Agent 请求 upload-requests 失败（连接失败 / 5xx / 超时）时：

1. 记日志，**不重试到天荒地老**：最多 2 次、间隔 3s（与 §10 的 ADB 命令级重试同量级）
2. 回退到派单时的 `presigned_uploads[]`，按今天的固定键集逻辑上传
3. 回退路径下 glob 文件依旧传不了——这是降级，不是新缺陷

`401`（租约失效）**不回退**：租约都不是自己的了，说明这个任务已经易主或被回收，
继续上传只会污染别人的证据。记日志并跳过上传。

### 5.3 Agent 侧的路径映射

今天 Agent 有一张 `wellKnownFiles` 映射（`tasks.go:271`），把 5 个固定键名映射到
`out_dir` 内的相对路径。按需签发后，Agent 报的就是 `out_dir` 内的相对路径本身，
`object_key` 由 Runtime 拼，映射表在新路径上**不再需要**。

保留 `wellKnownFiles` 仅供回退路径使用，并加注释说明它只服务于 §5.2。

## 6. 错误处理

| 情形 | 行为 |
|---|---|
| `upload_request_url` 为空（旧 Runtime） | Agent 直接走 `presigned_uploads[]`，与今天一致 |
| 端点连接失败 / 5xx / 超时 | 重试 ≤2 次后回退固定键集（§5.2） |
| 端点返回 401 | 不回退，不上传，记日志 —— 租约已非己有 |
| 部分 key 被 `rejected` | 已签发的照传；被拒的记日志，不影响其余 |
| MinIO 未配置（Runtime 侧） | 端点返回 503；Agent 回退，而 `presigned_uploads[]` 此时也是空的 → 无附件，与今天的降级一致 |
| Agent 收集到 0 个文件 | 不发请求（空 `files` 会被 400） |
| 上传单个文件失败 | 与今天一致：该文件不进 attachments，其余继续 |

## 7. 兼容与下线

滚动升级四种组合都必须可用：

| Runtime | Agent | 行为 |
|---|---|---|
| 旧 | 旧 | 今天的行为 |
| 新 | 旧 | Agent 不认识 `upload_request_url`，用 `presigned_uploads[]` —— 与今天一致 |
| 旧 | 新 | `upload_request_url` 为空，Agent 走 `presigned_uploads[]` |
| 新 | 新 | 按需签发，glob 文件也能上传 |

`presigned_uploads[]` 的下线条件：**全部 Agent 升级完，且不再有依赖回退路径的运行**。
与心跳字符串格式（`callbacks-api.openapi.yaml`）一样，标 `deprecated` 但保留，
删除属破坏性变更。本轮不标 deprecated——它还是回退路径的载体，现在标会误导。

## 8. 配置

| 变量 | 缺省 | 说明 |
|---|---|---|
| `UPLOAD_REQUEST_MAX_FILES` | `64` | 单次请求的文件数上限 |
| `MINIO_PRESIGN_TTL` | `1h` | 复用现有变量；按需签发后这个值不再是长任务的风险点 |

不新增开关：端点始终启用（受租约凭据保护）。`upload_request_url` 是否下发，由
`CALLBACK_BASE_URL` 是否配置决定——与现有回调地址同一来源，不引入第二处配置。

## 9. 测试

**契约**：`callbacks-api.openapi.yaml` 与 `client-agent-api.openapi.yaml` 的正反例，
沿用 `contracts/tests/` 既有形态。

**Runtime handler（表驱动）**：
- 租约凭据正确 → 200，`uploads` 数量与合法 `files` 一致
- 凭据失配（错 `lease_generation` / 错 `client_id` / 租约已释放）→ 401，**且不签发任何 URL**
- `task_id` 与凭据里的不一致 → 401
- 路径逃逸（`../`、绝对路径、空串）→ 进 `rejected`，其余照签
- 超 `UPLOAD_REQUEST_MAX_FILES` → 400（整体拒绝，不截断）
- MinIO 未配置 → 503
- **签发出的 object_key 一定以 `runs/{task_id}/` 开头**（对全部用例断言，这是安全性质）

**Agent（fake Runtime）**：
- 正常路径：按实际收集到的文件请求，上传数量与之匹配
- 端点 5xx → 重试 2 次后回退固定键集，且回退确实上传了
- 端点 401 → 不回退、不上传
- `upload_request_url` 为空 → 直接走固定键集（旧 Runtime 兼容）
- glob 文件（如 `dumps/0001.bin`）在新路径下确实被上传——这是关闭 CONTRACT-ISSUE 的证据

**端到端**：现有的 dispatch → 执行 → 收集 → 回调链路测试补一条断言：
`dumps/**` 命中的文件出现在 `results` 回调的 attachments 里。

## 10. 验收标准

- 长任务（执行时长超过 `MINIO_PRESIGN_TTL`）的附件能成功上传（有测试或实机验证）
- `dumps/**` 这类 glob 命中的文件能进 MinIO（有测试）
- 无有效租约凭据的请求拿不到任何 URL（有测试）
- 任何签发出的 key 都在 `runs/{task_id}/` 前缀内（有测试，全用例断言）
- 四种滚动升级组合都不丢附件（新×旧、旧×新有测试覆盖）
- Runtime 在任务执行期间重启，附件仍能经回退路径上传
- `presign.go` 与 `2026-07-21-agent-service-design.md` 的两处 CONTRACT-ISSUE 关闭并注明去向
