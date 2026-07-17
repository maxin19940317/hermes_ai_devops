# Algo_Super_SDK Hermes 测试打包设计

日期：2026-07-17

## 目标与范围

为独立仓库 `Algo_Super_SDK` 增加一条可被 Hermes Client Agent 消费的测试打包
链路，同时保留现有 SDK 发布包的结构和用途。新链路覆盖 8 个构建变体，为 4 个
Android 变体生成可执行 smoke 测试包；4 个 Linux 变体生成同契约包，但 Phase 1
不进入设备派发。

本设计同时修改两个独立仓库：

- `hermes_ai_devops`：修正契约路径兼容性、更新变体 Manifest 配置和测试，并提供
  可复制的权威 CI 工具。
- `Algo_Super_SDK`：保存 Hermes 工具的固定版本快照，新增精简测试打包脚本并改造
  GitLab CI。

两个仓库分别提交，不引入 Git submodule，也不要求 Algo 的内网 GitLab Runner 在
构建时访问 GitHub。

## 方案选择

采用独立 `hermes_test_pack.sh`，不为现有 `release_pack.sh` 增加 profile，也不在
SDK 归档完成后通过删除目录的方式改造成测试包。

独立脚本直接从本次构建输出、模型目录和测试数据中收集明确的运行闭包。这样 SDK
发布包继续面向人工交付和开发集成，Hermes 测试包只面向自动设备测试；两种产物的
内容变化互不影响。

## 产物与数据流

```text
rebuild.sh
   ├─ release_pack.sh       → SDK 发布包及外部 SHA-256
   └─ hermes_test_pack.sh   → 精简测试基础包
                                  ↓
ci/hermes/gen_manifest.py   → manifest.yaml + files.sha256 + 唯一包名
                                  ↓
scripts/hermes_test_pack/verify_package.py → 静态运行闭包与结果契约检查
                                  ↓
ci/hermes/write_meta.py     → 每变体 meta
                                  ↓
8 个 meta ────────────────→ gen_bundle.py → bundle → Registry
```

master 和 tag pipeline 上传测试包及 bundle。Merge Request pipeline 完成构建、打包、
契约与静态检查，但不上传 Registry。

测试包最终文件名沿用 Hermes 唯一性规则：

```text
algo-super-sdk-<variant>-g<CI_COMMIT_SHORT_SHA>-p<CI_PIPELINE_IID>.tar.gz
```

GitLab Generic Package 的版本目录继续使用严格 `X.Y.Z`，不把 commit 或 pipeline
编码进版本号。

## Algo 仓库结构

新增以下受控文件：

```text
hermes_test_pack.sh
scripts/hermes_test_pack/
├── lib/package.sh
├── lib/variants.sh
├── templates/run.sh.template
└── verify_package.py

ci/hermes/
├── REVISION
├── README.md
├── gen_manifest.py
├── write_meta.py
├── gen_bundle.py
├── variants.yaml
└── contracts/
    ├── manifest.schema.json
    ├── bundle.schema.json
    └── result.schema.json
```

`ci/hermes/REVISION` 只包含来源 `hermes_ai_devops` commit。`README.md` 说明快照来源、
复制文件清单、更新步骤和漂移检查命令。Algo CI 只使用仓库内快照，不在运行时下载
Hermes 文件。

`verify_package.py` 属于 Algo 测试包检查器，不从 Hermes 快照复制；它了解 Algo
测试包的固定布局和变体 smoke 映射。

## 测试包布局

所有变体使用相同布局：

```text
<single-top-directory>/
├── run.sh
├── bin/
│   └── <smoke executable>
├── lib/
│   ├── *.so
│   └── dsp/                 # 仅需要 DSP 运行时的 SNPE 变体
├── models/
│   └── <suite-specific files>
├── config/
│   └── smoke.json
├── testdata/
│   └── <fixed input image>
├── manifest.yaml            # gen_manifest.py 注入
└── files.sha256             # gen_manifest.py 注入
```

不允许测试包部署 `include/`、`example/`、SDK README、`CONTENTS.txt` 或其他只服务于
发布的内容。

所有普通动态库归一到 `lib/`。复制时若出现同名文件：摘要一致则只保留一份；摘要不
一致则打包失败，禁止后复制的文件静默覆盖前一个文件。SNPE DSP 搜索所需文件保留在
`lib/dsp/`。

## Smoke 映射

| 变体 | suite | 可执行文件 | 配置 | 输入 | 设备约束 |
|---|---|---|---|---|---|
| Android/Linux SNPE 1.68 | `snpe-smoke` | `seg_crowd_test` | 人体分割 `model.json` | `human.jpg` | Android: QCM6125 + hexagon |
| Android/Linux SNPE 2.21 | `snpe-smoke` | `seg_crowd_test` | 人体分割 `model.json` | `human.jpg` | Android: QCM6125 + hexagon |
| Android/Linux RKNN 2.3.2 | `rknn-smoke` | `ocr_test` | OCR v6 small `model.json` | `111.png` | Android: RK3588/RK3566 + rknpu |
| Android/Linux TFLite 2.21.0 | `tflite-smoke` | `ocr_test` | OCR v6 small `model.json` | `111.png` | Android: arm64-v8a |

实际执行命令固定为：

```text
SNPE:   ./bin/seg_crowd_test ./config/smoke.json ./testdata/human.jpg 1
RKNN:   ./bin/ocr_test       ./config/smoke.json ./testdata/111.png 1
TFLite: ./bin/ocr_test       ./config/smoke.json ./testdata/111.png 1
```

打包时将选定配置复制为 `config/smoke.json`，并把其中的 `model_dir` 重写成测试包
内的 `./models/.../` 路径。选定模型、配置、输入或可执行文件任一缺失即失败。

RKNN 即使当前没有 RK SoC，也必须生成可执行测试包并通过所有静态检查。Runtime 按
Manifest 的 SoC/capability 约束阻止它被派发给 QCM6125；接入 RK SoC 后补齐 RKNN
原生 Windows 实机验收。

## `run.sh` 语义

每个包生成一个只执行固定 smoke case 的 `run.sh`，不接受任意命令。Manifest 的
`test.entry` 为 `./run.sh`，`test.args` 为空列表。

运行流程：

1. 创建 `results/` 和 `logs/`。
2. 记录开始时间。
3. 执行固定业务程序，将 stdout/stderr 保存到 `logs/smoke.log`。
4. 保留业务程序的原始退出码。
5. 写入 `results/result.json`。
6. 将日志输出到标准输出，便于 Agent 同时采集。
7. 以业务程序原始退出码退出。

结果文件至少包含 result v1 必填字段。CLI 模式使用
`HERMES_TASK_ID=agent-cli`、`HERMES_ATTEMPT=1` 的缺省值；未来服务模式可由 Agent
注入同名环境变量。业务程序退出 0 时 cases 为 1 passed；非零时为 1 failed，并在
failures 中记录 suite 名称和退出码。

业务程序完成但返回非零时，结果生命周期状态仍为 `COMPLETED`，测试结论由退出码、
cases 和 Runtime verdict 判定。找不到入口或执行权限错误由 Client/静态检查在更早
阶段阻止，不由包装器伪装成测试失败。

## Manifest 与运行环境

Hermes `ci/variants.yaml` 进行以下调整：

- 所有变体的 `test.entry` 保持 `./run.sh`，args 改为空。
- Android 动态库路径使用 `{workdir}/lib`。
- SNPE 的 `ADSP_LIBRARY_PATH` 包含 `{workdir}/lib/dsp` 和设备系统 DSP 路径。
- RKNN Android 保留 `rknpu` capability 和 RK SoC 白名单。
- TFLite Android 不要求特定 SoC。
- Linux 变体生成 Manifest，但 Phase 1 不进入 ADB 调度。

Hermes Manifest Schema 的安全相对路径字符集加入 `+`，使
`libc++_shared.so` 合法。绝对路径、空路径、`..`、路径穿越和符号链接仍必须被
拒绝。嵌入 Agent 的 Manifest Schema 与顶层契约同步更新。

## CI 改造

Algo `.gitlab-ci.yml` 的每个变体 job 执行：

1. `rebuild.sh <variant>`。
2. `release_pack.sh` 生成并自校验 SDK 包。
3. `hermes_test_pack.sh` 生成精简基础包。
4. 使用 `ci/hermes/gen_manifest.py` 注入契约并生成唯一命名测试包。
5. 使用 `verify_package.py` 验证固定布局、入口、运行闭包、结果 Schema 和部署白名单。
6. 使用 `write_meta.py` 生成 `dist/meta/<variant>.json`。
7. master/tag 上传最终测试包；MR 不上传。
8. 将 meta 作为 GitLab job artifact 交给聚合 job。

新增不可中断的 `publish:bundle`。它显式 needs 8 个 build job，只有 8 个 meta 全部
存在且 bundle Schema 校验通过时才上传。任何缺失变体都不得发布部分 bundle。

现有 SDK 包上传和候选/正式 tag 流程保留。测试包上传对同一 job 重跑产生的 400
already-exists 可做幂等 skip；其他 HTTP 状态必须失败，不能用无条件 `|| echo` 吞掉。

## 错误处理与安全边界

以下情况必须使打包或 CI 失败：

- 未知 variant 或 variant 与构建输出平台不一致；
- smoke 二进制、配置、模型、输入、动态库或 `run.sh` 缺失；
- 同名动态库内容不同；
- 配置仍引用包外模型路径；
- Manifest 或 result 示例未通过对应 Schema；
- `deploy.files` 包含禁止目录或摘要不匹配；
- 归档含绝对路径、`..`、符号链接或非单一顶层目录；
- meta 数量不是 8，或 variant 集合与权威 `variants.yaml` 不一致；
- Registry 上传返回非成功且不是确认的同文件重复。

Client Agent 仍只执行 Manifest 声明的入口，不增加任意 shell 接口。业务包不能改变
Hermes、Runtime、Client Agent 的既有职责边界。

## 测试策略

### Hermes 仓库

- Manifest Schema 正例覆盖 `libc++_shared.so`。
- 反例覆盖绝对路径、`..` 和路径穿越。
- 检查 Agent 嵌入 Schema 与顶层契约完全一致。
- 更新 8 个 variant 的 Manifest 渲染期望。
- 保持 contracts/CI Python 测试和 Agent Go 测试通过。

### Algo 仓库

- 使用最小临时 fixture 验证 SNPE、RKNN、TFLite 文件选择和固定命令。
- 验证缺少二进制、模型、配置、输入时失败。
- 验证同名同摘要动态库去重、不同摘要动态库失败。
- 验证测试包不含禁止目录，且 `run.sh`/配置/模型引用闭合。
- 执行生成的 `run.sh` fake fixture，验证成功和非零退出均生成合法 result v1，并
  保留原始退出码。
- 验证 8 个 meta 才能生成 bundle。
- 对当前完整样包执行静态检查，但不把大体积产物提交仓库。

### 实机验收

- QCM6125：SNPE 1.68、SNPE 2.21 和 TFLite 2.21.0。
- RK SoC：RKNN 2.3.2，在设备接入后执行。
- 每个支持变体要求原生 Windows `agent-cli.exe` 输出
  `status=COMPLETED`、`exit_code=0`、`criteria_met=true`，并回收合法
  `results/result.json` 与日志。

## 提交与交付边界

实施时使用两个隔离 worktree，分别提交：

1. Hermes 契约、工具配置、测试和快照来源 commit。
2. Algo 打包脚本、Hermes 快照、CI 和测试。

先完成 Hermes commit，再将该 commit 对应文件复制到 Algo 的 `ci/hermes/`，确保
`REVISION` 可复现。两个仓库分别运行测试、合并和推送；不得把 Algo 工作区中现有的
未跟踪文件带入提交。
