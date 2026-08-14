# ci/ — 业务仓库(algo-super-sdk)CI 集成脚本

脚本对应 CLAUDE.md §6 的待改造事项,在 GitLab Runner 上运行。

| 文件 | 职责 |
|---|---|
| `variants.yaml` | 12 个构建变体 → Manifest 参数映射(调度约束/env/签名/超时) |
| `gen_manifest.py` | 解包 → 注入 `manifest.yaml` + `files.sha256` → Schema 校验 → 重打包为唯一文件名 |
| `write_meta.py` | 生成 `dist/meta/{variant}.json`(job artifact) |
| `gen_bundle.py` | 聚合 12 个 meta → `bundle-g{sha}-p{global_id}.json`,不齐全拒绝发布 |

12 个变体(2026-08-06 pipeline 1109 起,SNPE/TFLite 变体名编码目标 SoC):
`aarch64_{Linux,Android}_QCS{QCM}6125_SNPE_1.68`、
`aarch64_{Linux,Android}_QCS{QCM}6490_SNPE_2.21`、
`aarch64_{Linux,Android}_RK3562_RKNN_2.3.2`、
`aarch64_{Linux,Android}_RK3568_RKNN_2.3.2`、
`aarch64_{Linux,Android}_RK3576_RKNN_2.3.2`、
`aarch64_{Linux,Android}_Qualcomm_TFLite_2.21.0`

集成方式见 `gitlab-ci.example.yml`。Runner 依赖:`python3 >= 3.9`,`pip install pyyaml jsonschema`。

## 数据流

```
release_pack.sh → *.tar.gz
  → gen_manifest.py  (注入契约,重命名为 {name}-{variant}-g{sha}-p{iid}.tar.gz,输出 info JSON)
  → write_meta.py    (info + CI 变量 → dist/meta/{variant}.json)
  → curl 上传包到 Generic Registry + POST /kick(meta 原样透传)
  ... 12 个变体 job 并行 ...
  → gen_bundle.py    (12 个 meta + CI_COMMIT_TIMESTAMP
                       → bundle-g{sha}-p{global_id}.json,Schema 校验后上传)
  → Trigger:kick 起单变体 workflow / bundle 作为发布完整性断言
```

## 关键决定

- **唯一性靠文件名**:GitLab 13.8 Generic Registry 版本号强制 strict `X.Y.Z`,
  故 commit + pipeline iid 编码进文件名,版本号不变也不会互相覆盖/被 skip。
  同名上传只会发生在同 job 重跑,此时 400 already-exists → skip 是安全幂等。
- **manifest 校验失败 = pipeline 失败**:契约不合法的包不允许进 Registry。
- **bundle 是发布原子单位**:任何变体缺 meta(如被 interruptible 打断)则整个
  bundle 不发,Trigger 永远不会看到残缺构建。
- **bundle 身份全局唯一且可重现**:`pipeline_global_id` 来自 GitLab
  `CI_PIPELINE_ID`,而现有 `pipeline_id` 仍是项目内的 `CI_PIPELINE_IID`。bundle
  文件名使用全局 ID,避免同 commit 的不同 pipeline 相互覆盖。
  `created_at` 传入 `CI_COMMIT_TIMESTAMP`,不读取 job 墙钟;这使 GitLab 13.8
  上同一 pipeline 的 retry 产生字节完全一致的 bundle。
- **变体级触发(kick,2026-07-22 演进)**:build job 上传成功后直发
  `POST /kick`,Trigger 校验(形态 + URL 归属 + Registry 探活)后起单变体
  workflow(ID 含 variant,重复 kick 由 Temporal 去重)——一个包编好即测,
  不等全部 12 个包与 pipeline success。bundle 保留为发布完整性断言;
  `TRIGGER_PIPELINE_WEBHOOK=false` 后 pipeline success webhook 仅记录不再起
  完整 workflow(防双跑)。触发与设备解耦:fleet 无匹配设备的变体由
  SelectTestSpecs 秒级跳过(任意 OS/板型,CI 不改)。
- **产物来源白名单(2026-08-14)**:`/kick` 的 URL 默认必须指向本 GitLab
  Registry;`PACKAGE_URL_BASES`(逗号分隔,compose 缺省取 `MINIO_PUBLIC_ENDPOINT`)
  可额外放行本机 MinIO。MinIO 产物桶 `hermes-packages` 公开读(匿名探活/下载,
  URL 稳定不随签名过期),生命周期默认 30 天。GitLab URL 探活带 token,
  其余来源匿名 Range 探活。
- **本地包一条龙(2026-08-14,`ci/upload_kick.py`)**:服务器上的 tar.gz
  未登记时,一条命令完成 上传 MinIO → 解析 manifest(requirements/
  failure_signatures,原始字节算 manifest_digest)→ schema 校验 → kick 登记
  并启动单变体 workflow。之后 Hermes `devops_test <variant>` 即命中该包:
  ```bash
  TRIGGER_WEBHOOK_SECRET=xxx python3 ci/upload_kick.py \
    --package /tmp/xxx.tar.gz --variant aarch64_Linux_QCS6490_SNPE_2.21 \
    --commit 5c885dbb --pipeline-iid 9991 --pipeline-global-id 88880
  ```
  变体名须与该变体的已登记名称一致(requirements.soc 决定派哪块板)。
  实测:264MB 真实 QCS6490 包经此脚本触发 → PASSED。
- 包内单一顶层目录布局时,`deploy.files[].src` 保留实际路径,`dst` 剥掉顶层目录。

## 测试

```bash
~/anaconda3/envs/hermes-devops/bin/python -m pytest ci/tests
```
