# Algo_Super_SDK 打包适配评估设计

日期：2026-07-17

## 目标

在 `hermes_ai_devops` 中建立一项可追踪的业务项目适配评估，判断
`/home/maxin/Code/560D/Algo_Super_SDK` 当前发布包能否被 Hermes Client Agent
安全、确定地部署并执行。评估必须给出证据、优先级、整改责任边界和可验证的完成
标准，而不是只记录笼统结论。

本项只修改 `hermes_ai_devops`。不修改 `Algo_Super_SDK` 的源码、CI 或本地未跟踪
文件；业务仓库的实际改造在评估项通过评审后另行实施。

## 评估基线

评估报告固定记录以下基线，避免目标仓库后续变化导致结论无法复现：

- 目标仓库：`/home/maxin/Code/560D/Algo_Super_SDK`
- 已评估 commit：`57d3ca01eca9b3860ae3c861a66b282b16261525`
- 打包相关受控文件在评估时无已跟踪改动
- 样包：`Algo_Super_SDK_v1.0.2_aarch64_Android_TFLite_2.21.0.tar.gz`
- 样包 SHA-256：`73cf87bce40381c0cb50bdf5e2572ea9ff4fb32a22df2b95a006e7400fde91d7`
- 样包规模：455 个常规文件，解压后 98,367,108 字节

样包属于目标仓库的本地产物，不复制到 Hermes 仓库。报告只保存可复核的元数据、
检查命令和观察结果。

## 交付内容

### 1. 适配评估报告

新增 `docs/assessments/algo-super-sdk-packaging.md`，按以下结构组织：

1. 评估范围与基线。
2. 总体结论：区分“SDK 发布包完整性”和“Hermes 设备测试包适配性”。
3. 当前规则与 Hermes 契约逐项对照表。
4. 已确认的阻塞证据。
5. P0/P1 整改清单。
6. 适配验收矩阵。
7. 非目标与后续业务仓库改造边界。

结论应明确：当前样包通过自身 SHA-256 校验，具备 SDK 发布内容，但不满足 Hermes
设备测试包预期，不能直接进入自动设备测试链路。

### 2. Phase 1 适配门禁

在 `CLAUDE.md` Phase 1 的 CI 改造步骤下增加 `Algo_Super_SDK` 适配门禁，引用评估
报告，并规定只有 P0 项全部关闭、代表性 Android 包通过静态与实机验收后，Trigger
才能把该业务仓库产物派发给 Client Agent。

门禁只补充现有 Phase 1 的验收条件，不改变三层架构、Manifest 契约边界或既定的
Runtime/Agent 职责。

## 已确认问题与优先级

### P0：阻塞设备测试

1. **路径契约不兼容**：`contracts/manifest.schema.json` 的相对路径规则不允许 `+`，
   因而 Android 必需库 `libc++_shared.so` 无法进入合法 Manifest。整改时必须允许
   安全文件名中的 `+`，同时保持对绝对路径、`..` 和路径穿越的拒绝。
2. **缺少测试入口**：样包内没有 Manifest 当前声明的 `./run.sh`。
3. **缺少结构化结果生产者**：现有测试程序和打包脚本不生成
   `results/result.json`，无法满足 `test.success.require_files`。
4. **运行库搜索路径错误**：`ci/variants.yaml` 声明的
   `{workdir}/lib` 与包内 `lib/aarch64_Android`、`3rd_party/*/lib` 不一致。
5. **测试参数不匹配**：统一的 `--suite ... --output ...` 参数不是当前
   `ocr_test` 等业务程序的实际命令行接口。
6. **CI 集成缺失**：目标仓库 `.gitlab-ci.yml` 尚未调用 `gen_manifest.py`、
   `write_meta.py` 或 `gen_bundle.py`，也未发布 Hermes 唯一命名包和完整 bundle。
7. **部署集合过宽**：`gen_manifest.py` 当前扫描包内全部常规文件。直接处理 SDK
   发布包会把头文件、示例源码和文档也列入 `deploy.files`，造成不必要的逐文件
   ADB 部署。

### P1：兼容性与质量

1. 对 SNPE、RKNN、TFLite 分别验证测试二进制、动态库闭包、模型、配置和输入样本。
2. 建立设备能力矩阵；例如 QCM6125 设备不得调度 RKNN 变体。
3. 增加代表性包契约测试，覆盖 `libc++_shared.so`、单顶层目录及路径穿越反例。
4. 增加 Registry 上传前的静态包检查，验证入口、依赖目录、结果契约和收集规则。
5. 在原生 Windows 上使用 `agent-cli.exe` 完成支持变体的实机验收。

## 推荐整改边界

SDK 交付包和设备测试包承担不同职责，不应强制使用完全相同的文件集合。后续实施
推荐保留 `release_pack.sh` 的 SDK 发布产物，再从该产物生成裁剪后的 Hermes 测试包：

- 只保留测试所需二进制、运行库、模型、配置和输入数据；
- 注入受控的 `run.sh`、`manifest.yaml` 和 `files.sha256`；
- 由 `run.sh` 适配各业务程序参数，并在成功、失败时都写出结构化结果；
- 保持原 SDK 包可供人工交付，Hermes 测试包用于自动设备测试。

这种边界避免破坏已有 SDK 发布用途，也减少设备部署时间和不必要文件暴露。

## 验收标准

适配门禁至少包含以下检查：

1. 代表性包通过整包 SHA-256 和 Manifest Schema 校验。
2. 包内存在 `run.sh`、`manifest.yaml`、`files.sha256`，且 Manifest 声明的每个文件
   摘要匹配。
3. 部署集合不包含 `include/`、`example/` 和发布说明等非运行时内容。
4. 测试入口引用的二进制、配置、模型、输入数据和动态库均存在。
5. Android 动态库路径与 Manifest 环境变量一致。
6. 8 个变体的 meta 齐全时才生成 bundle；Android 与 Linux 变体按阶段边界调度。
7. 支持的 Android 变体通过原生 Windows 实机执行，结果满足：
   `status=COMPLETED`、`exit_code=0`、`criteria_met=true`，并成功回收
   `results/result.json` 和约定日志。
8. 不兼容设备在派发前由 requirements/capabilities 拒绝，而不是运行后失败。

## 验证方式

- 检查评估报告中的 commit、样包摘要、文件数量和失败证据可由列出的命令复现。
- 检查 `CLAUDE.md` 门禁引用有效且未改变现有架构约束。
- 扫描文档，确保没有 `TODO`、`TBD`、凭据或把本地绝对路径误写为生产配置。
- 文档改动使用 Markdown，提交信息遵循仓库英文约定。
