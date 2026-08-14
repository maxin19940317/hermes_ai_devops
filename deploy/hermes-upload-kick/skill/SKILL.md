---
name: devops-test-package
description: 测试本地 tar.gz 测试包(经 hermes-upload-kick 上传 MinIO 并触发设备测试)。当用户给出测试包路径或要求"测这个包/打包测一下"时使用。
---

# DevOps Test Package(上传 tar 包并触发设备测试)

把本地 tar.gz 测试包上传到 MinIO 并触发 QCS6490 等设备测试。不需要预先在
artifacts 表登记——本流程自动完成 上传 + 解析 manifest + kick。

## 何时使用

- 用户给出一个 tar.gz 包路径,要求"测一下这个包"
- 用户要求对某个本地构建产物做设备测试
- 明确这是 **devops 设备测试**(adb 设备,如 QCS6490/QCM6125/RK3576);
  不是 ls26 串口板卡(那是另一个 skill ls26-lab-operations)

## 用法

```bash
# 上传并触发。variant/pipeline_iid/pipeline_global_id 必填;commit/project 可选
# (缺省取包内 manifest.artifact)。
curl -s -m 600 -X POST \
  --data-binary @<包路径> \
  "http://172.17.0.1:18686/upload-kick?variant=<variant>&pipeline_iid=<n>&pipeline_global_id=<n>&commit=<sha>&project=<proj>"
```

参数说明:
- `<包路径>`:tar.gz 在 Hermes 内的路径,如 `/opt/data/workspace/gene_pm-tmp/xxx.tar.gz`
- `variant`:变体名(如 `aarch64_Linux_QCS6490_SNPE_2.21`),决定派到哪块板
- `pipeline_iid` / `pipeline_global_id`:任意正整数(本地手动触发用递增数字即可)
- `commit` / `project`:可选,缺省取包内 manifest.yaml 的 artifact 字段

## 响应

- `{"ok": true, "reply": "202 {\"started\":true,\"workflow_id\":\"...\"}", "url": "..."}`
  → 成功,workflow 已启动
- `{"ok": false, "error": "..."}` → 失败,按 error 排查

## 常见变体名(当前设备)

- `aarch64_Linux_QCS6490_SNPE_2.21`(QCS6490 Ubuntu 板)
- `aarch64_Android_QCM6125_SNPE_1.68`(QCM6125 Android 板)
- `aarch64_Android_RK3576_RKNN_2.3.2`(RK3576 Android 板)

## 注意

- 包必须含 `manifest.yaml`(打包规范见仓库 docs/package-demo-guide.md)
- 明确 SoC 的变体只派到对应板,不会替代
- 触发后可用 devops_wait_result 等待结果
