# Hermes DevOps Runtime 独立 Compose 部署设计

日期：2026-07-21

状态：已批准，待实施计划

## 1. 背景与决策

`/opt/hermes/docker-compose.yml` 运行的是 Hermes Agent 的 `gateway` 和
`dashboard`，使用 host 网络。它与本仓库的 DevOps Runtime 没有代码级依赖，且
宿主机 `8090` 已被现有 `gitlab_bridge` 占用。

本次采用独立 Compose 项目部署 DevOps Runtime，不修改 `/opt/hermes`：

```text
GitLab Pipeline Hook
        |
        v
Trigger (:8090/container, :18090/host)
        |--- GitLab Generic Package Registry
        |--- PostgreSQL: hermes_runtime
        `--- Temporal (:7233/internal)
                    |
                    v
              Worker (device-test queue)
                    |--- PostgreSQL: hermes_runtime
                    `--- callbacks (:8091/container)
```

选择独立 Compose 的原因：

- 不干扰现有 Hermes Agent 的启动、升级和端口；
- Runtime 服务使用独立 bridge 网络，以服务名访问 PostgreSQL 和 Temporal；
- Trigger 与 Worker 共用同一构建镜像，但保持独立进程和重启边界；
- 后续加入 Client Agent、MinIO 或 HTTPS/mTLS 时无需改造现有 Agent Compose。

不采用的方案：

- 把 Runtime 服务加入 `/opt/hermes/docker-compose.yml`：会耦合两个无直接依赖的系统，
  并与现有 host 网络和端口布局混杂；
- Trigger 运行在宿主机、其余组件运行在 Docker：适合临时调试，但不利于一致部署和
  回滚。

## 2. 范围

本次交付：

- `runtime/Dockerfile`：多阶段构建 `trigger` 与 `worker` 两个 Go 二进制；
- `deploy/docker-compose.yml`：PostgreSQL、Temporal、Temporal UI、Trigger、Worker；
- `deploy/.env.example`：只包含非秘密示例和必填变量说明；
- PostgreSQL 初始化脚本：在同一 PostgreSQL 实例中隔离 Temporal 与 Runtime 数据库；
- Worker `GET /healthz`：补齐容器健康检查端点；
- 部署、升级、验证、日志查看和回滚说明；
- Compose 静态校验、镜像构建测试、Go 回归测试和部署冒烟测试。

本次不交付：

- 修改 `/opt/hermes/docker-compose.yml` 或停止其现有容器；
- Windows Client Agent RPC/心跳服务；
- 真机测试闭环；
- MinIO、飞书交互卡片或 LLM；
- 面向生产公网的 TLS/mTLS 终止与高可用 Temporal。

因此本次是 q-uat 的 Phase 1 集成部署，不宣称生产就绪。

## 3. 服务与数据边界

### 3.1 PostgreSQL

使用 PostgreSQL 15+ 单实例、独立数据库：

- `temporal`：Temporal 主存储；
- `temporal_visibility`：Temporal visibility 存储；
- `hermes_runtime`：artifacts、clients、devices、tasks、events、results。

初始化必须幂等。Temporal schema 由官方的 schema/setup 流程管理；Runtime schema
继续由 `store.OpenPG` 中的嵌入式 `schema.sql` 管理。Trigger 完成健康检查后再启动
Worker，避免两个进程首次启动时并发执行 Runtime DDL。

数据库不发布到宿主机网络。数据放在命名卷中，常规停止和回滚禁止删除卷。

### 3.2 Temporal

q-uat 使用单节点 Temporal，持久化到上述 PostgreSQL。镜像必须固定到明确版本和
不可变 digest，禁止 `latest`。初始实现参考 Temporal 官方 `samples-server` 的
PostgreSQL Compose 配置；`auto-setup` 仅用于本次 UAT 集成环境，不作为生产部署
模板。生产化时切换到 `temporalio/server` 和显式 schema migration。

Temporal gRPC 只在 Compose 网络内提供给 Trigger/Worker。为了运维诊断，可以把
Temporal UI 绑定到宿主机 `127.0.0.1:18080`，通过 SSH tunnel 访问；不对 LAN 或公网
直接暴露。

### 3.3 Runtime 镜像

同一个镜像包含：

- `/app/hermes-trigger`；
- `/app/hermes-worker`；
- `/etc/hermes/variants.yaml`。

构建阶段使用与 `runtime/go.mod` 一致的 Go 1.26.5，运行阶段使用非 root 用户和精简
基础镜像，并保留访问 GitLab HTTPS 所需的 CA 证书。两个 Compose service 通过不同
command 启动对应二进制。

### 3.4 Trigger

容器内监听 `:8090`，默认映射宿主机 `18090:8090`，不使用已被占用的宿主机 8090。
环境变量：

- `TRIGGER_WEBHOOK_SECRET`：GitLab Webhook Secret Token；
- `GITLAB_BASE_URL=https://gitlab2.quectel.com`；
- `GITLAB_TOKEN`：Hermes 用户 PAT，至少 `read_api`；
- `GITLAB_TOKEN_HEADER=PRIVATE-TOKEN`；
- `PACKAGE_NAME=algo-super-sdk`；
- `TRIGGER_REFS=master`；
- `TEMPORAL_ADDRESS=temporal:7233`；
- `TEMPORAL_TASK_QUEUE=device-test`；
- `DATABASE_URL`：连接 `hermes_runtime` 数据库。

`/healthz` 作为容器 liveness。进程在初始数据库或 Temporal 连接失败时 fail fast；运行
期间 `/healthz` 不代表所有下游依赖仍然健康，端到端冒烟负责验证依赖链路。

### 3.5 Worker 与回调服务

Worker 使用与 Trigger 完全相同的 `TEMPORAL_TASK_QUEUE=device-test` 和
`DATABASE_URL`，并设置：

- `VARIANTS_CONFIG=/etc/hermes/variants.yaml`；
- `WORKER_CALLBACKS_ADDR=:8091`；
- `CALLBACK_BASE_URL`：Client 收到的 Runtime 回调基地址；
- `ARTIFACT_AUTH_TYPE`、`ARTIFACT_AUTH_TOKEN`：后续 Client 下载私有包使用；
- 可选的 `FEISHU_WEBHOOK_URL`。

当前 Worker 回调 mux 没有健康检查。本次新增无副作用的 `GET /healthz`，只报告 HTTP
进程存活，不修改 `/callbacks/v1/*` 的业务语义。

`contracts/callbacks-api.openapi.yaml` 要求 mTLS，但当前 handler 尚未执行 mTLS
鉴权。为避免形成未受保护的 LAN 接口，本次默认只把 Worker 回调端口映射到
`127.0.0.1:18091`。在 Windows Client 联调前，必须先完成 HTTPS/mTLS 终止或由防火墙
严格限制到测试网段，之后再把 `CALLBACK_BASE_URL` 改为 Client 可访问的地址。

## 4. Compose 启动与恢复顺序

启动依赖为：

```text
postgres healthy
      |
      v
temporal schema/setup + temporal healthy
      |
      v
trigger healthy
      |
      v
worker healthy
```

所有长期运行服务使用 `restart: unless-stopped`。健康检查使用容器内可用的原生命令或
专用探针，不依赖宿主机工具。启动不能依赖固定 `sleep`。

Trigger 和 Worker 都允许在依赖恢复后由 Compose 重启。Temporal 历史与 Runtime
业务表持久化在 PostgreSQL 卷中；重建应用容器不会丢失 Workflow 或业务状态。

## 5. 网络与端口

| 组件 | 容器端口 | 宿主机默认 | 暴露策略 |
|---|---:|---:|---|
| Trigger | 8090 | 18090 | q-uat 联调时供 GitLab Webhook 访问；生产需 HTTPS 反代 |
| Worker callbacks | 8091 | 127.0.0.1:18091 | 默认仅本机；mTLS/网段限制完成后才供 Client 访问 |
| Temporal gRPC | 7233 | 不发布 | 仅 Compose 网络 |
| Temporal UI | 8080 | 127.0.0.1:18080 | 仅 SSH tunnel |
| PostgreSQL | 5432 | 不发布 | 仅 Compose 网络 |

Compose 项目和网络使用独立名称 `hermes-runtime`，不使用 host 网络，不声明会与现有
容器冲突的固定 `container_name`。

## 6. 秘密与权限

- 真实配置存放在部署主机的 `deploy/.env`，权限 `0600`，不提交 Git；
- `.env.example` 不包含可用 Token、密码或 DSN；
- `GITLAB_TOKEN` 与 `TRIGGER_WEBHOOK_SECRET` 是两个不同秘密；
- GitLab API 调试只输出筛选字段，禁止再次输出完整项目对象；
- 已暴露的 GitLab Runner registration token 必须在部署前完成轮换；
- 容器日志不得打印 Token、数据库密码或完整授权头；
- Runtime 容器以非 root 用户运行，文件系统按可行范围设为只读。

## 7. 验证与验收

### 7.1 构建前验证

- `go test ./...` 全部通过；
- Dockerfile 能分别启动 Trigger 和 Worker；
- `docker compose config` 成功，且渲染结果不含空的必填配置；
- 镜像与第三方服务均固定版本，不使用 `latest`。

### 7.2 服务冒烟

- PostgreSQL healthcheck 通过；
- Temporal cluster health 通过；
- `curl http://127.0.0.1:18090/healthz` 返回 `200 ok`；
- `curl http://127.0.0.1:18091/healthz` 返回 `200 ok`；
- Trigger/Worker 日志分别出现监听和 Temporal worker 启动信息；
- Runtime 数据库包含预期表。

### 7.3 Pipeline 656 链路

使用项目 ID `651`、全局 Pipeline ID `656` 和 commit `0f3b2fe1`：

1. Hermes PAT 能通过 GitLab API 读取私有项目；
2. Trigger 能下载并校验 `bundle-g0f3b2fe1-p656.json`；
3. 模拟成功 Pipeline Hook 返回 `202` 和确定性 `workflow_id`；
4. `artifacts` 表登记 bundle 中全部 8 个变体；
5. Temporal 中可查询到 `DeviceTestWorkflow`，且 Worker 已消费 task queue；
6. 重复发送同一 webhook 返回 `started:false`，数据库记录不重复。

没有 Client 心跳和可用设备时，Workflow 等待设备并最终按规则收敛为无设备，而不是
完成真机测试。这是当前代码阶段的预期行为，不作为部署失败。真机闭环属于后续 Client
RPC/心跳部署。

## 8. 日志、升级与回滚

- `docker compose logs` 是本次统一日志入口；日志使用 UTC 和结构化 JSON；
- 升级前记录当前应用镜像 digest，构建新镜像后逐个重建 Trigger/Worker；
- 回滚只恢复应用镜像版本并重新创建应用容器，不执行 `down -v`；
- 数据库 schema 当前只做向前兼容的 `CREATE TABLE IF NOT EXISTS`，任何后续破坏性迁移
  必须另行设计备份和回滚；
- 部署和回滚不得操作 `/opt/hermes` 的 `gateway`、`dashboard` 或占用 8090 的
  `gitlab_bridge`。

## 9. 实施完成判据

本次实施完成必须同时满足：

- 独立 Compose 可在 q-uat 重复启动，现有 Hermes Agent 不受影响；
- 四个核心服务 PostgreSQL、Temporal、Trigger、Worker 均健康；
- Pipeline 656 手工 webhook 完成 Registry、数据库和 Temporal 的端到端验证；
- 重复 webhook 幂等验证通过；
- 所有秘密留在部署主机，仓库和构建产物中无真实凭据；
- 文档明确当前不是生产部署，且真机闭环仍依赖后续 Client Agent。

## 10. 参考

- Temporal 官方最新 Compose 示例已迁移到
  <https://github.com/temporalio/samples-server>；
- Temporal 官方说明 `auto-setup` 适合开发/集成环境，生产应使用显式配置的
  `temporalio/server`：<https://github.com/temporalio/docker-compose>；
- 本仓库总体架构与阶段约束见 `CLAUDE.md` 第 4、11、12、14 节。
