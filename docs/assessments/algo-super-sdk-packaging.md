# Algo_Super_SDK 打包适配评估

日期：2026-07-17

## 结论

本次评估将两个维度分开判定：

- **SDK 发布完整性：通过。** 当前 `release_pack.sh` 会生成归档和 SHA-256 文件，
  并检查发布信息及规定目录；本次样包也通过其 SHA-256 校验。
- **Hermes 测试包适配性：不通过。** 当前包缺少可执行测试入口和结构化结果生产
  能力，运行库路径与 Manifest 配置不一致，目标 CI 也未接入 Manifest、meta 和
  bundle 生成。因此在本报告 P0 项全部关闭前，禁止将该业务仓库产物直接派发给
  Client Agent。

当前包可以继续作为 SDK 人工交付物，但不能直接作为 Hermes 自动设备测试包。

## 评估范围与基线

- 目标项目本地路径：`/home/maxin/Code/560D/Algo_Super_SDK`
- 目标项目 commit：`57d3ca01eca9b3860ae3c861a66b282b16261525`
- 评估时 `.gitlab-ci.yml`、`rebuild.sh`、`release_pack.sh`、
  `scripts/release_pack/` 和 `ci/resolve_build_env.sh` 没有已跟踪改动
- 本地评估样包：
  `Algo_Super_SDK_v1.0.2_aarch64_Android_TFLite_2.21.0.tar.gz`
- 样包 SHA-256：
  `73cf87bce40381c0cb50bdf5e2572ea9ff4fb32a22df2b95a006e7400fde91d7`
- 样包常规文件数：455
- 样包解压后常规文件总大小：98,367,108 字节

样包是目标项目工作区中的本地产物，不是 Hermes 仓库的测试夹具，也不复制到本
仓库。路径只用于复核本次评估，不是生产配置。

## 已验证事实

1. 样包的外部 `.sha256` 校验通过。
2. 样包只有一个顶层目录，包含 SDK 二进制、动态库、模型、头文件、示例源码和发布
   说明。
3. 样包不存在 `run.sh`、`manifest.yaml`、`files.sha256` 或
   `results/result.json`。
4. 样包只有一个带执行位的文件：`bin/aarch64_Android/ocr_test`。
5. 样包包含 Android 运行时依赖 `3rd_party/opencv/lib/libc++_shared.so`。
6. 使用当前 `ci/gen_manifest.py` 和 Manifest Schema 处理样包时，路径
   `3rd_party/opencv/lib/libc++_shared.so` 因含 `+` 被拒绝；这意味着现有契约无法
   表达 Android 标准 C++ 运行库文件名。
7. `ci/variants.yaml` 为 TFLite Android 变体设置的 `LD_LIBRARY_PATH` 只有
   `{workdir}/lib`，但实际库位于 `lib/aarch64_Android` 和多个
   `3rd_party/*/lib` 目录。
8. 当前 Manifest 模板要求运行 `./run.sh --suite tflite-smoke --output results/`，
   而包内没有 `run.sh`，`ocr_test` 的实际接口也不接受该组参数。
9. 目标项目 `.gitlab-ci.yml` 没有调用 `gen_manifest.py`、`write_meta.py` 或
   `gen_bundle.py`。
10. `gen_manifest.py` 当前会扫描归档内全部常规文件；若直接处理该 SDK 包，头文件、
    示例源码和发布说明也会进入 `deploy.files`。

## 规则对照

| 评估项 | 当前观察 | Hermes 预期 | 判定 | 整改项 |
|---|---|---|---|---|
| 唯一包名 | 发布包名只包含 SDK 版本和变体 | master 包名包含 commit 与 pipeline IID | 不通过 | P0-6 |
| `manifest.yaml` | 包内不存在 | 打包期生成并通过 Schema 校验 | 不通过 | P0-6 |
| `files.sha256` | 包内不存在，仅有整包外部摘要 | 包内记录每个部署文件摘要 | 不通过 | P0-6 |
| 安全相对路径 | `libc++_shared.so` 无法通过当前 Schema | 允许安全的 Android 运行库名，仍拒绝路径穿越 | 不通过 | P0-1 |
| 测试入口 | 包内只有 `ocr_test`，没有 `run.sh` | Manifest 声明的受控入口必须存在 | 不通过 | P0-2 |
| 测试参数 | `ocr_test` 使用配置、输入和迭代次数参数 | 入口适配 Manifest 参数并确定性执行 | 不通过 | P0-5 |
| 结构化结果 | 不生成 `results/result.json` | 入口在成功和失败路径都生成 result v1 | 不通过 | P0-3 |
| 运行库路径 | 库分散在平台目录和 `3rd_party` 目录 | `LD_LIBRARY_PATH` 覆盖实际运行库闭包 | 不通过 | P0-4 |
| 部署文件集合 | SDK 包含 455 个常规文件 | 仅部署测试运行所需文件 | 不通过 | P0-7 |
| 变体 meta | 构建 job 不生成 meta | 每个变体生成一个经过约束的 meta | 不通过 | P0-6 |
| bundle 完整性 | 没有 bundle 聚合 job | 8 个 meta 齐全才发布 bundle | 不通过 | P0-6 |
| 设备能力选择 | 变体存在，但当前 CI 不产生调度契约 | 派发前按 SoC 和 capability 过滤 | 部分具备 | P1-2 |
| SDK 自校验 | 校验整包摘要、发布信息和规定目录 | SDK 发布用途保持可验证 | 通过 | 无 |

## P0 整改项

以下项目全部关闭前，适配门禁保持关闭：

- [ ] **P0-1 路径契约兼容：** 允许相对路径中的安全 `+` 字符，使
  `libc++_shared.so` 可进入 Manifest；保留绝对路径、`..` 和路径穿越反例测试。
- [ ] **P0-2 受控测试入口：** 为每个进入设备测试的 Android 变体提供包内
  `run.sh`，并确保 `test.entry` 指向实际存在且具有执行权限的文件。
- [ ] **P0-3 结构化结果：** 由入口包装器在成功、测试失败和启动失败路径写出符合
  `contracts/result.schema.json` 的 `results/result.json`。
- [ ] **P0-4 运行库搜索路径：** 按变体列出实际 `lib/aarch64_Android` 和所需
  `3rd_party/*/lib` 路径，并通过动态库闭包检查验证。
- [ ] **P0-5 参数适配：** 将每个 suite 映射到真实测试程序、配置、输入样本和迭代
  参数；不得把统一的 `--suite/--output` 参数直接传给不支持它的业务程序。
- [ ] **P0-6 CI 契约链路：** 接入 commit/pipeline 唯一包名、Manifest 注入、
  `files.sha256`、每变体 meta 和 8 变体完整 bundle；Schema 校验失败必须使
  pipeline 失败。
- [ ] **P0-7 最小部署集合：** 从 SDK 发布包派生裁剪后的 Hermes 测试包，只保留
  测试二进制、运行库、模型、配置、输入数据和受控入口，不部署 `include/`、
  `example/` 或纯发布文档。

## P1 整改项

- [ ] **P1-1 变体运行闭包：** 分别验证 SNPE、RKNN、TFLite 的二进制、动态库、
  模型、配置和输入样本，不从一个变体外推其他变体。
- [ ] **P1-2 设备能力矩阵：** 明确每个 Android 变体的 SoC 和 capability；例如
  QCM6125 不得调度 RKNN 变体。
- [ ] **P1-3 代表性契约测试：** 增加包含 `libc++_shared.so` 和单顶层目录的正例，
  以及绝对路径、`..`、符号链接和路径穿越反例。
- [ ] **P1-4 上传前静态检查：** 在 Registry 上传前验证入口存在、文件摘要、运行库
  目录、结果契约和 collect 规则。
- [ ] **P1-5 Windows 实机验收：** 使用原生 Windows `agent-cli.exe` 对每个受支持
  Android 变体执行至少一次完整部署、运行和收集。

## 推荐包边界

保留两类产物，避免破坏现有 SDK 交付用途：

1. **SDK 发布包：** 继续由 `release_pack.sh` 生成，包含库、头文件、示例、模型和
   说明，面向人工或下游开发集成。
2. **Hermes 测试包：** 从 SDK 发布包或同一 staging 内容派生，仅包含设备测试运行
   闭包，并注入 `run.sh`、`manifest.yaml` 和 `files.sha256`。

入口包装器负责把 Manifest 的 suite 语义转换成实际业务程序参数，并在所有可控终态
写出结构化结果。Hermes 测试包使用包含 commit 和 pipeline IID 的唯一文件名；SDK
包继续保持现有版本化命名，两者不互相替代。

## 验收矩阵

| 验收维度 | 通过条件 | 证据 |
|---|---|---|
| 静态契约 | 整包 SHA、Manifest Schema、每个 `deploy.files` 摘要均通过 | CI 静态检查日志 |
| 包内入口 | `run.sh` 存在、可执行，引用路径全部在包内 | 包清单和入口检查 |
| 运行闭包 | 二进制、模型、配置、输入和所有动态库存在 | 变体闭包报告 |
| 最小部署 | `deploy.files` 不含 `include/`、`example/` 和纯发布说明 | Manifest 审计 |
| CI 聚合 | 8 个变体 meta 齐全后才发布 bundle | bundle 生成日志及 Schema 校验 |
| 调度约束 | 不支持的设备在派发前被 requirements/capabilities 拒绝 | Runtime 调度测试 |
| Windows 实机 | `status=COMPLETED`、`exit_code=0`、`criteria_met=true` | `agent-cli.exe` 输出及 run summary |
| 结果收集 | `results/result.json` 和规定日志成功回收 | Agent 结果目录及结果 Schema 校验 |

只有上述所有 P0 相关验收通过，Trigger 才能将 Algo_Super_SDK 产物派发给 Client
Agent。P1 项可以分阶段关闭，但不得削弱设备选择、安全路径或结果契约。

## 非目标

- 本项不修改 Algo_Super_SDK 的源码、构建脚本或 `.gitlab-ci.yml`。
- 本项不把本地样包提交到 Hermes 仓库。
- 本项不为 Linux 变体提前实现 SSH Adapter；Linux 设备测试仍属于 Phase 4。
- 本项不改变 Hermes、Runtime 和 Client Agent 的三层职责边界。
- 本项不以“样包能解压”代替原生 Windows 加真实设备的最终验收。

## 复核命令

以下命令只读取目标仓库或在 Hermes 临时输出目录中工作，不会修改目标仓库：

```bash
git -C /home/maxin/Code/560D/Algo_Super_SDK rev-parse HEAD
git -C /home/maxin/Code/560D/Algo_Super_SDK diff --quiet -- \
  .gitlab-ci.yml rebuild.sh release_pack.sh scripts/release_pack ci/resolve_build_env.sh

cd /home/maxin/Code/560D/Algo_Super_SDK/dist
sha256sum -c Algo_Super_SDK_v1.0.2_aarch64_Android_TFLite_2.21.0.sha256
tar -tzf Algo_Super_SDK_v1.0.2_aarch64_Android_TFLite_2.21.0.tar.gz
```

Manifest 兼容性可从 Hermes 仓库执行 `ci/gen_manifest.py` 复核。当前基线会在
`deploy.files` 中的
`Algo_Super_SDK_v1.0.2_aarch64_Android_TFLite_2.21.0/3rd_party/opencv/lib/libc++_shared.so`
处失败，因为 `contracts/manifest.schema.json` 的相对路径正则不接受 `+`。
