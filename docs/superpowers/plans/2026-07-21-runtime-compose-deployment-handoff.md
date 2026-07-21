# Runtime Compose deployment handoff

日期：2026-07-21

## Git 状态

- 工作树：`/home/maxin/Code/hermes_ai_devops/.worktrees/runtime-compose-deployment`
- 分支：`feature/runtime-compose-deployment`
- 基线：`a6d09cb`（已合并 Runtime Worker 的 master）
- 交接前实现 HEAD：`b2d43a4`
- 设计：`docs/superpowers/specs/2026-07-21-runtime-compose-deployment-design.md`
- 实施计划：`docs/superpowers/plans/2026-07-21-runtime-compose-deployment.md`

## 已完成

1. `db21145 docs: design runtime compose deployment`
   - PostgreSQL、Temporal、Trigger、Worker 独立 Compose 设计已批准。
   - 明确不修改 `/opt/hermes/docker-compose.yml`，不占用宿主机 8090。
2. `ff59d65 docs: plan runtime compose deployment`
   - 七个实施任务和 q-uat 验收步骤已写明。
3. `07e913f feat(runtime): add worker health endpoint`
   - Worker callback mux 新增 `GET /healthz`。
   - callbacks 包测试通过，规格与质量审查通过。
4. `c990066 test(deploy): define runtime compose contracts`
   - `deploy/.env` 与 `deploy/images.lock.env` 已加入根级精确忽略规则。
   - 测试验证真实秘密路径被 Git 忽略，而对应 `.example` 路径不被忽略。
   - 规格与质量审查通过。
5. `b2d43a4 build(runtime): add container image`
   - 新增根构建上下文的 `.dockerignore`。
   - 新增 `runtime/Dockerfile`，同一非 root Alpine 镜像包含 Trigger、Worker 和
     `/etc/hermes/variants.yaml`。
   - 当前静态部署契约测试通过，规格审查通过。

## 尚未关闭的问题

Task 3 的最终代码质量审查仍有一个 **Important**，因此 Task 3 不能标记完成：

- `deploy/tests/test_deploy_contracts.py` 目前按物理行读取 Dockerfile；
- 两条 Go build 命令仍通过原始文本 `assertIn` 校验；
- 把相同文本移入注释、错误 stage，或追加第三个 `FROM`，测试可能误通过；
- 本机没有 Docker，所以这个静态门禁必须足够严格，不能依赖尚未执行的镜像构建。

接手人应先提交一个独立修复：

1. 增加 Dockerfile logical-instruction parser：
   - 跳过空行和注释；
   - 合并末尾反斜杠续行；
   - 用单空格归一化逻辑指令；
   - 未终止续行必须报错。
2. 精确断言 `FROM` 列表只有：
   - `FROM ${GO_IMAGE} AS build`
   - `FROM ${RUNTIME_BASE_IMAGE}`
3. 精确断言唯一的双 build `RUN` 位于 build stage。
4. 分别校验 build stage、runtime stage 和最终 `USER/WORKDIR/CMD`。
5. 重新运行 focused 和完整 `unittest`，再请求质量复审。

建议提交信息：

```text
test(deploy): harden Dockerfile contract parser
```

## 尚未开始

- Task 4：`deploy/.env.example`、PostgreSQL 初始化、镜像 digest lock、
  环境预检、PostgreSQL + Temporal + Trigger + Worker Compose。
- Task 5：Pipeline 656 端到端验证脚本。
- Task 6：q-uat 运维文档和最终本地/远端验收。
- Task 7：整体代码审查、分支交付和集成选择。

## 未执行的外部验收

当前开发环境没有 `docker` 命令，因此以下项目尚未验证，禁止声称通过：

- Runtime Docker 镜像真实构建；
- 第三方镜像 tag 到 digest 的锁定；
- `docker compose config`；
- PostgreSQL、Temporal、Trigger、Worker 容器健康；
- Pipeline 656 的 Registry → artifacts → Temporal → Worker 链路；
- q-uat 的端口、网络和持久卷验证。

这些步骤必须在 q-uat 上执行，并且不能停止或修改：

- `/opt/hermes` 的 `gateway`、`dashboard`；
- 占用宿主机 8090 的现有 `gitlab_bridge`。

Trigger 的宿主机端口按设计使用 18090；Worker callback 默认只绑定
`127.0.0.1:18091`，在 HTTPS/mTLS 或测试网段限制完成前不得暴露到 LAN。

## 环境注意事项

本机是 WSL1。内置 `apply_patch` 沙箱可能报：

```text
bubblewrap is not supported on WSL1
```

仍应使用 `apply_patch` 编辑文件，但通过 `which apply_patch` 得到完整路径，再由
`exec_command` 以 `require_escalated` 运行；不要改用 `cat > file` 或其他直接写入
方式。

## 接手续跑命令

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/runtime-compose-deployment
git status --short --branch
git log --oneline --decorate a6d09cb..HEAD
python3 -m unittest discover -s deploy/tests -v
cd runtime
/home/maxin/.local/go/bin/go test ./...
```

先关闭 Task 3 的 Important，再按实施计划从 Task 4 继续。真实秘密只保存在 q-uat 的
`deploy/.env`，不得提交、输出或粘贴到评审记录。
