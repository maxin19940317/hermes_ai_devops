---
name: devops-test-package
description: 测试本地 tar.gz 测试包(经 hermes-upload-kick 上传 MinIO 并触发设备测试)。当用户给出测试包路径或要求"测一下 <路径>/测试这个包"时使用。
---

# DevOps Test Package(上传 tar 包并触发设备测试)

把本地 tar.gz 测试包上传到 MinIO 并触发设备测试(QCS6490 等 adb 板)。
不需要预先在 artifacts 表登记——自动完成 上传 + 解析 manifest + kick。

## 何时使用

- 用户给出一个 tar.gz 包路径要求测试,如"测一下 /opt/data/workspace/gene_pm-tmp/xxx.tar.gz"
- 这是 **devops 设备测试**(adb 板:QCS6490/QCM6125/RK3576);不是 ls26
  串口板卡(那是另一个 skill ls26-lab-operations)

## 用法(推荐:文件名即变体)

把包命名为 `<variant>.tar.gz` 放到 workspace(如
`/opt/data/workspace/gene_pm-tmp/aarch64_Linux_QCS6490_SNPE_2.21.tar.gz`),
然后:

```bash
VARIANT=$(basename /opt/data/workspace/gene_pm-tmp/aarch64_Linux_QCS6490_SNPE_2.21.tar.gz .tar.gz)
curl -s -m 600 -X POST \
  --data-binary @/opt/data/workspace/gene_pm-tmp/aarch64_Linux_QCS6490_SNPE_2.21.tar.gz \
  "http://172.17.0.1:18686/upload-kick?variant=${VARIANT}&pipeline_iid=<n>&pipeline_global_id=<n>"
```

## 用法(文件名不含变体)

文件名不是 `<variant>.tar.gz` 时,需显式传 variant:

```bash
curl -s -m 600 -X POST \
  --data-binary @<包路径> \
  "http://172.17.0.1:18686/upload-kick?variant=<variant>&pipeline_iid=<n>&pipeline_global_id=<n>"
```

## 参数

- `<包路径>`:tar.gz 在 Hermes 内的路径
- `variant`:变体名(推荐从文件名推导,即 `basename <path> .tar.gz`);
  若文件名不含变体则必须显式给出
- `pipeline_iid` / `pipeline_global_id`:任意正整数(手动触发用递增数字即可,
  如时间戳/当前分钟数;脚本会自动避免重复)
- `commit` / `project`:可选,缺省取包内 manifest.yaml 的 artifact 字段

## 响应

- `{"ok": true, "reply": "202 {\"started\":true,\"workflow_id\":\"...\"}", "url": "..."}`
- `{"ok": false, "error": "..."}` → 失败,按 error 排查

## 常见变体名(当前设备)

- `aarch64_Linux_QCS6490_SNPE_2.21`(QCS6490 Ubuntu 板)
- `aarch64_Android_QCM6125_SNPE_1.68`(QCM6125 Android 板)
- `aarch64_Android_RK3576_RKNN_2.3.2`(RK3576 Android 板)

## 注意

- 包必须含 `manifest.yaml`(打包规范见 docs/package-demo-guide.md)
- 明确 SoC 的变体只派到对应板,不会替代
- 触发后可用 devops_wait_result 等待结果
