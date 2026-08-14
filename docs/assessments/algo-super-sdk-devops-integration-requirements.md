# Algo_Super_SDK 接入 Hermes DevOps 设备测试 — 修改需求(RFC)

> 版本: v1.0(2026-08-14)
> 状态: 待业务仓库评估
> 评审人: tobias(DevOps 平台侧);业务仓库负责人
> 对应平台文档: `hermes_ai_devops/CLAUDE.md`、`docs/package-demo-guide.md`、
> `docs/assessments/algo-super-sdk-packaging.md`

---

## 1. 背景与目标

Hermes AI DevOps 已具备自动设备测试能力(编译产物 → 派发 → 板卡执行 → 结果回传)。
`Algo_Super_SDK` 作为主要业务仓库,其产物需要**成为可被 Hermes 自动测试的测试包**。

当前 SDK 发布包(`Algo_Super_SDK_v1.0.2_*.tar.gz`)是**人工交付物**,缺少 Hermes
测试包所需的受控测试入口、结构化结果与调度契约。本 RFC 提出业务仓库侧需要完成的
修改,供评估排期。

**判定原则**:所有 P0 项关闭前,Hermes 不会把该仓库产物派发给板卡执行。

---

## 2. 修改需求清单

### A. 设备调度约束(variants.yaml 变更)

**背景**:Runtime 侧已于 2026-08-11 将 SNPE 变体的 SoC 从"留空(靠 capability 匹配)"
改为**显式 SoC 白名单**。业务仓库 `ci/variants.yaml` 必须与本仓库
`hermes_ai_devops/ci/variants.yaml` **保持一致**,否则包内 manifest 的 requirements
与 Runtime 调度不一致,导致误派设备或变体被跳过。

| 变体名 | requirements(目标值) | 变更点 |
|---|---|---|
| `aarch64_Linux_QCS6125_SNPE_1.68` | `os: linux, soc: [QCS6125], capabilities: [hexagon]` | soc 从空 → `[QCS6125]` |
| `aarch64_Linux_QCS6490_SNPE_2.21` | `os: linux, soc: [QCS6490], capabilities: [hexagon]` | soc 从空 → `[QCS6490]` |
| `aarch64_Android_QCM6125_SNPE_1.68` | `os: android, soc: [QCM6125], capabilities: [hexagon]` | soc 从空 → `[QCM6125]` |
| `aarch64_Android_QCM6490_SNPE_2.21` | `os: android, soc: [QCM6490], capabilities: [hexagon]` | 已定(2026-08-06 起) |
| RKNN 各变体 | `soc: [RK3562/RK3568/RK3576], capabilities: [rknpu]` | 已定 |
| TFLite 变体 | `soc: [QCM6125,QCM6490,SM6225]` / `[QCS6125,QCS6490]` | 已定 |

> **注意**:QCS6125 与 QCS6490 的 SNPE 变体**必须显式区分**,不可互相替代。
> 此前 QCS6125 变体因只声明 `capabilities: [hexagon]`,被 QCS6490 替代执行,
> 但 QCS6490 的 DSP 对 SNPE 1.68 不可用(`RUNTIME_NOT_AVAILABLE`),导致误测失败。
> 显式 soc 后,无匹配设备时变体 `SKIPPED`(不误测、不阻塞其他变体)。

**同步方式**:业务仓库 `ci/variants.yaml` 是运行时副本,由 `gen_manifest.py`
在打包期渲染进包内 `manifest.yaml`。**改业务仓库 → 重新打包 → Runtime 自动读取包内
requirements,无需改 Runtime。**

### B. 测试包内容(打包期必须注入)

每个进入设备测试的变体,其测试包(非 SDK 发布包)必须包含:

| 项 | 要求 | 对应 P0 |
|---|---|---|
| `run.sh` | 受控测试入口,`manifest.test.entry` 指向它;在 workdir 下可执行 | P0-2 |
| `results/result.json` | 成功/失败路径都按 `result.schema.json` v1 输出 | P0-3 |
| `manifest.yaml` | 由 `gen_manifest.py` 生成,过 `manifest.schema.json` 校验 | P0-6 |
| `files.sha256` | 包内每个部署文件摘要 | P0-6 |
| 运行库闭包 | `LD_LIBRARY_PATH`/`ADSP_LIBRARY_PATH` 覆盖实际用到的库 | P0-4 |
| 最小文件集 | 不含 `include/`、`example/`、纯发布文档 | P0-7 |

### C. 唯一包名

`Algo_Super_SDK_v1.0.2_aarch64_Android_*.tar.gz`(仅版本号+变体)会与 GitLab
Registry 的 `X.Y.Z` 版本号冲突——**版本号不变时,新构建会被 skip 静默丢弃**。

改为(CLAUDE §6.1):

```text
{RELEASE_PACKAGE_NAME}-{variant}-g{CI_COMMIT_SHORT_SHA}-p{CI_PIPELINE_IID}.tar.gz
例:algo-super-sdk-aarch64_Linux_QCS6490_SNPE_2.21-g5c885dbb-p124.tar.gz
```

包名用 commit + pipeline 唯一化;Registry 版本号 `X.Y.Z` 仍用于 GitLab 13.8 的
Generic Package 寻址约束(不变)。

### D. CI 契约链路(gitlab-ci.yml 增量改造)

业务仓库 `.gitlab-ci.yml` 的 build job 需接入:

1. `gen_manifest.py` 打包后注入 `manifest.yaml` + `files.sha256`,过 schema 校验;
   不合法 → **pipeline 失败**
2. `write_meta.py` 输出 `dist/meta/{variant}.json`
3. `gen_bundle.py` 聚合 12 变体 meta → `bundle-g{sha}.json` 上传 Registry
   (**12 个 meta 不齐全不发 bundle**,挡住残缺构建)
4. 每个 build job 上传成功后直发 `POST /kick`(meta 原样透传),一个包编好即测

对应脚本(平台侧已就绪,业务仓库直接引用):
`hermes_ai_devops/ci/{gen_manifest.py, write_meta.py, gen_bundle.py, kick.py}`。

### E. Hermes 上传路径约定(配合 Hermes 手动触发,可选)

Hermes 现在支持直接对本地 tar.gz 发起测试。为让 Hermes 自动推导变体:

- 测试包放到 Hermes workspace 时,文件名 = `<variant>.tar.gz`
  (例:`aarch64_Linux_QCS6490_SNPE_2.21.tar.gz`)
- Hermes 指令:`测一下 /opt/data/workspace/gene_pm-tmp/<variant>.tar.gz`

此为**运维约定**,不是代码改动;不配合则 Hermes 需要显式指定 variant。

### F. 失败签名(manifest.test.failure_signatures)

为使错误分类准确(CODE/MODEL/DELEGATE),包内 manifest 应声明每变体的失败签名。
平台侧已内置默认(见 `hermes_ai_devops/ci/variants.yaml` defaults),业务仓库可
按实际日志追加/覆盖。

---

## 3. 建议排期

| 优先级 | 项 | 预估工作量 | 依赖 |
|---|---|---|---|
| P0 | B(C 包结构)+ D(CI 链路) | 2-3 人日 | gen_manifest 等脚本平台侧已就绪 |
| P0 | A(variants.yaml soc pin) | 0.5 人日 | 需与平台侧 variants.yaml 对齐 |
| P0 | C(唯一包名) | 0.5 人日 | — |
| P1 | F(失败签名逐变体核对) | 1 人日 | 需真实日志 |
| P2 | E(Hermes 上传约定) | 0(约定) | 配合即可 |

---

## 4. 验收标准(业务侧自查)

- [ ] 每个受支持变体的测试包:整包 sha256 + manifest schema + files.sha256 均过
- [ ] 包内 `run.sh` 存在、可执行、引用路径全部在包内
- [ ] 成功/失败路径都产出 `results/result.json`(符合 result.schema v1)
- [ ] `deploy.files` 不含 `include/`、`example/`、纯发布文档
- [ ] 12 变体 meta 齐全才发布 bundle;缺变体时 bundle 不发
- [ ] 每个 SNPE 变体 soc 显式、与平台侧 variants.yaml 一致
- [ ] 至少一个 Android 变体经原生 Windows `agent-cli` 实机跑通
  (`status=COMPLETED, exit_code=0, criteria_met=true`)

---

## 5. 平台侧已就绪(无需业务仓库操作)

- MinIO 第二产物源(hermes-packages 公开读桶 + kick URL 白名单)
- Hermes 上传即测(hermes-upload-kick 服务 + gene_pm skill)
- 变体级触发(kick)、Temporal 去重、设备调度解耦(包内 requirements 权威)
- 失败签名匹配 + 规则引擎 verdict(LLM 仅补充解释)
- 飞书通知卡片(含 verdict 与日志链接)
