# wf-devops-device-test — DevOps 设备测试 workflow 资产

Hermes workflow_runtime 的合规封装:在 hermes-agent 里以 workflow 形式触发
DevOps 设备测试系统。

## 架构合规性(为什么这样设计)

```
用户(Hermes WebUI/飞书)
   → wf-devops-device-test(workflow.yaml,LLM 编排)
       → devops_test / devops_rerun / devops_status / devops_runs(hermes_devops MCP)
           → Runtime /api/v1/cmd(确定性执行)
               → Temporal workflow → Windows Agent → 设备测试
```

- **执行全部走 Runtime**:本 workflow 只编排 LLM 调用 `devops_*` 工具,不直接
  操作 ADB/设备(CLAUDE.md §3.3/§14:LLM 禁止直连 ADB、禁止任意命令)。
- **环境用 `hermes_devops` MCP**(合规受控接口),**不用 `local_device`**
  (那是 18765 端口直连 ADB 的违规 MCP)。
- LLM 不在执行关键路径(§3 规则 5):workflow 编排是辅助,真正的测试由
  Runtime 确定性完成。

## 部署(在 hermes-rocklin 容器内)

```bash
# 1. 拷贝 workflow 目录
docker cp hermes/workflow_runtime/wf-devops-device-test hermes-rocklin:/opt/data/profiles/workflow_runtime/workspace/workflows/

# 2. 注册到 WORKFLOW_REGISTRY.yaml(见 registry 条目;或用 hermes workflow 管理命令)
# 3. 重启 gateway 或新会话生效
```

## Registry 条目

```yaml
- id: wf-devops-device-test
  name: DevOps 设备测试
  version: 1.0.0
  owner: pm
  status: stable
  metadata:
    hermes:
      workflow: true
      tags:
      - devops
      - device-test
      - automated
  execution:
    primary: yaml
    targets:
    - id: device-test
      label: DevOps 设备测试
      match:
        required_term_groups:
        - - 设备测试
          - devops
          - 测试
      inputs: {}
  auto_start:
    phrases:
    - 设备测试
    - 触发设备测试
```

## 测试

```bash
# 在 tobias_pm 会话里(新会话加载)
hermes -z "运行 DevOps 设备测试 workflow,变体 aarch64_Android_QCM6125_SNPE_1.68"
```

预期:LLM 走 01-validate → 02-trigger(devops_test)→ 03-report,
返回 workflow_id 与运行状态,全程不出现 ADB/终端命令。
