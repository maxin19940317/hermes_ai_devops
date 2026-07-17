# ci/ — 业务仓库(algo-super-sdk)CI 集成脚本

四个脚本对应 CLAUDE.md §6 的三件待改造事项,在 GitLab Runner 上运行。

| 文件 | 职责 |
|---|---|
| `variants.yaml` | 8 个构建变体 → Manifest 参数映射(调度约束/env/签名/超时) |
| `gen_manifest.py` | 解包 → 注入 `manifest.yaml` + `files.sha256` → Schema 校验 → 重打包为唯一文件名 |
| `write_meta.py` | 生成 `dist/meta/{variant}.json`(job artifact) |
| `gen_bundle.py` | 聚合 8 个 meta → `bundle-g{sha}.json`,不齐全拒绝发布 |

集成方式见 `gitlab-ci.example.yml`。Runner 依赖:`python3 >= 3.9`,`pip install pyyaml jsonschema`。

## 数据流

```
release_pack.sh → *.tar.gz
  → gen_manifest.py  (注入契约,重命名为 {name}-{variant}-g{sha}-p{iid}.tar.gz,输出 info JSON)
  → write_meta.py    (info + CI 变量 → dist/meta/{variant}.json)
  → curl 上传包到 Generic Registry
  ... 8 个变体 job 并行 ...
  → gen_bundle.py    (8 个 meta → bundle-g{sha}.json,Schema 校验后上传)
  → Trigger 服务只认 bundle
```

## 关键决定

- **唯一性靠文件名**:GitLab 13.8 Generic Registry 版本号强制 strict `X.Y.Z`,
  故 commit + pipeline iid 编码进文件名,版本号不变也不会互相覆盖/被 skip。
  同名上传只会发生在同 job 重跑,此时 400 already-exists → skip 是安全幂等。
- **manifest 校验失败 = pipeline 失败**:契约不合法的包不允许进 Registry。
- **bundle 是发布原子单位**:任何变体缺 meta(如被 interruptible 打断)则整个
  bundle 不发,Trigger 永远不会看到残缺构建。
- 包内单一顶层目录布局时,`deploy.files[].src` 保留实际路径,`dst` 剥掉顶层目录。

## 测试

```bash
~/anaconda3/envs/hermes-devops/bin/python -m pytest ci/tests
```
