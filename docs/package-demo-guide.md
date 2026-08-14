# 测试包打包指南(Packaging Guide)

> 目标读者:业务侧/算法工程师,想把自己的 demo 包装成 hermes-devops 可执行的测试包。
> 本文对应 `aarch64_Linux_QCS6490_SNPE_2.21` 真实包(p124)的结构,已实测通过。

一个可执行的测试包 = **一个 tar.gz**,内部包含:

```text
your-package.tar.gz
├── manifest.yaml        # 契约文件:声明要求/部署/测试/收集(必须有)
├── files.sha256         # 契约文件:载荷文件清单 + 校验和(必须有)
└── <载荷目录>/           # 你的实际内容(名字随意,manifest 里用相对路径引用)
    ├── run.sh           # 测试驱动入口(必须有,manifest.test.entry 指向它)
    ├── bin/             # 可执行文件(你的 demo 二进制)
    ├── lib/             # 依赖库(.so 等)
    ├── config/          # 配置(json 等)
    ├── models/          # 模型文件
    └── testdata/        # 测试输入数据
```

---

## 1. 决定目标设备与 requirements

先想清楚你的包跑在哪类设备上,这是 `manifest.yaml` 的 `requirements`:

| 字段 | 取值 | 说明 |
|---|---|---|
| `os` | `linux` / `android` | **必填** |
| `abi` | `arm64-v8a` 等 | **必填** |
| `soc` | `["QCS6490"]` | 允许运行的 SoC 白名单;**空 = 不限制**(不推荐,会误配到不兼容设备) |
| `capabilities` | `["hexagon"]` | 设备能力要求(与 Runtime 服务端 `DEVICE_CAPABILITIES_MAP` 匹配) |
| `min_free_storage_mb` | `512` | 部署前设备最小剩余空间检查 |

> ⚠️ **明确指定 SoC 时,不允许其它型号设备代替**(2026-08-12 决定)。
> 写了 `soc: [QCS6490]` 就只会派给 QCS6490,不会找 QCM6125 顶替。

---

## 2. 编写 run.sh(测试驱动)

`run.sh` 是设备端执行的入口,由 agent 在 `workdir` 下以 `./run.sh <args>` 运行。

**契约要求:**
1. 在**当前目录**下创建 `results/result.json`(绝对要求,见 §5)
2. 可选的 `logs/`(会被 collect 收走,便于排查)
3. 退出码 0 = 成功;非 0 = 失败(会被判为 CODE 类失败)

**最小模板**(Linux 板可用;Android 把 `/system/bin/sh` 换成 `#!/system/bin/sh`):

```sh
#!/bin/sh
set -u
mkdir -p results logs
start="$(date +%s)"

# ===== 你的测试逻辑 =====
./bin/your_demo --input testdata/demo.jpg > logs/demo.log 2>&1
exit_code=$?
# =========================

# 提取性能指标(可选):从日志里 grep 出 "xxx_ms=12.3" 之类的行
avg_ms=$(grep -oE '[0-9.]+ ms' logs/demo.log | head -1 | sed 's/ ms//')
metrics=""
if [ -n "${avg_ms}" ]; then
  metrics=", \"demo.inference_ms_avg\": ${avg_ms}"
fi

dur=$(( $(date +%s) - start ))
cat > results/result.json <<EOF
{
  "result_version": 1,
  "task_id": "${HERMES_TASK_ID:-demo}",
  "attempt": ${HERMES_ATTEMPT:-1},
  "status": "COMPLETED",
  "exit_code": ${exit_code},
  "duration_sec": ${dur},
  "metrics": {${metrics#,\ }},
  "cases": { "total": 1, "passed": $([ ${exit_code} -eq 0 ] && echo 1 || echo 0), "failed": $([ ${exit_code} -eq 0 ] && echo 0 || echo 1), "skipped": 0, "failures": [] }
}
EOF
exit ${exit_code}
```

> 💡 真实包用 `config/test-cases.conf` + 循环驱动多个二进制,并为每个产出
> `<binary>.inference_ms_avg` 指标。单 demo 不必照抄,一个 case 即可。

---

## 3. 编写 manifest.yaml

对照真实包 p124 的结构(必填字段以 schema `contracts/manifest.schema.json` 为准):

```yaml
manifest_version: 1
artifact:
  project: your-namespace/your-demo   # 项目名(显示用)
  commit: 5c885dbb                    # 你的 commit(8位hex即可)
  pipeline_id: 124                    # 你的 pipeline 号
  platform: aarch64_Linux_smoke       # 变体名(显示用,无约束格式)
  build_type: Release
requirements:
  os: linux
  abi: arm64-v8a
  soc: [QCS6490]
  capabilities: [hexagon]
  min_free_storage_mb: 512
deploy:
  workdir: /tmp/your-demo             # 设备端部署目录
  files:                              # 包内相对路径 → 部署相对路径
    - { src: bin/your_demo, dst: bin/your_demo, mode: "0755", sha256: "<hex64>" }
    - { src: run.sh, dst: run.sh, mode: "0755", sha256: "<hex64>" }
    - { src: testdata/demo.jpg, dst: testdata/demo.jpg, mode: "0644", sha256: "<hex64>" }
  env:
    LD_LIBRARY_PATH: "{workdir}/lib"   # {workdir} 会被 agent 替换为真实部署路径
test:
  entry: ./run.sh
  args: []
  timeout_sec: 900
  success:
    exit_code: 0
    require_files: [results/result.json]
  failure_signatures:                 # 可选;命中的 signature 决定 verdict 分类
    - { id: native_crash, where: stderr, pattern: "Segmentation fault|core dumped", classify: CODE }
collect:
  - results/result.json
  - results/junit.xml
  - logs/*.log
  - dumps/**
cleanup:
  remove_workdir: true
  keep_on_failure: true
```

**字段要点:**
- `deploy.files[].src` 是包内**相对路径**(不含顶层目录),`dst` 是 `workdir` 下相对路径
- 每个文件必须有 `sha256`(64 hex)。`mode` 用字符串 `"0755"`
- `env` 里 `{workdir}` 占位符 agent 会替换;`LD_LIBRARY_PATH`/`ADSP_LIBRARY_PATH` 按需
- `test.failure_signatures` 是"失败签名":日志匹配到 → 按 `classify` 分类,同时**阻止机械重试**。分类:`CODE`/`MODEL`/`DELEGATE`/`DEVICE`/`UNKNOWN`
- `workdir` **不能用 /opt**(RK3568 rootfs 只读),**Linux 用 /tmp**(QCS6490 的 /data 是 noexec)

---

## 4. 生成 files.sha256 并打包

`files.sha256` 是载荷文件(manifest 引用到的那些,不含 manifest 自身)的校验清单:

```bash
cd your-package-dir/
sha256sum bin/your_demo run.sh testdata/demo.jpg > files.sha256
# 格式:每行 "<sha256>  <相对路径>"
```

打包(注意**顶层不要套一层目录**,manifest/files.sha256 就在包根):

```bash
tar -czf your-demo.tar.gz manifest.yaml files.sha256 bin run.sh testdata
```

---

## 5. 用 schema 校验(强烈建议先过这关)

```bash
python3 - <<'EOF'
import json, yaml
from jsonschema import Draft202012Validator
manifest = yaml.safe_load(open('manifest.yaml'))
schema = json.load(open('contracts/manifest.schema.json'))
Draft202012Validator(schema).validate(manifest)
print("manifest OK")
EOF
```

校验不过 = agent 在设备端也会拒绝,直接在这里改掉最省事。

---

## 6. 上传到 MinIO 并触发测试

包就绪后,上传到 MinIO 的 `hermes-packages` 桶(**公开读**),然后 POST `/kick`:

```bash
# 1) 上传(在服务器上,用 mc 或 minio 客户端)
docker exec hermes-runtime-minio-1 mc cp your-demo.tar.gz \
  hermes/hermes-packages/your-demo-g5c885dbb-p124.tar.gz

# 2) 构造 kick 载荷并发送
SHA256=$(sha256sum your-demo.tar.gz | cut -d' ' -f1)
MANIFEST_DIGEST=$(sha256sum manifest.yaml | cut -d' ' -f1)
SIZE=$(stat -c%s your-demo.tar.gz)

curl -s -X POST http://127.0.0.1:18090/kick \
  -H "X-Gitlab-Token: $TRIGGER_WEBHOOK_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "variant": "aarch64_Linux_smoke",
    "package_file": "your-demo-g5c885dbb-p124.tar.gz",
    "url": "http://10.88.118.251:9000/hermes-packages/your-demo-g5c885dbb-p124.tar.gz",
    "sha256": "'$SHA256'",
    "size": '$SIZE',
    "manifest_digest": "'$MANIFEST_DIGEST'",
    "version": "1.0.2",
    "project": "your-namespace/your-demo",
    "commit": "5c885dbb",
    "pipeline_id": 124,
    "pipeline_global_id": 88882,
    "requirements": {"os":"linux","abi":"arm64-v8a","soc":["QCS6490"],"capabilities":["hexagon"],"min_free_storage_mb":512},
    "failure_signatures": []
  }'
```

**kick 载荷要点(Trigger 校验规则):**
- `url` 必须以 `PACKAGE_URL_BASES` 白名单开头(MinIO = `http://10.88.118.251:9000`)
- `sha256`(整包)、`manifest_digest`(manifest.yaml 的 sha256)都必须是 64 位 hex
- `size` > 0
- `requirements`/`failure_signatures` 现在由 kick 载荷自带(2026-08-12 解耦),Runtime 不再依赖镜像内 variants.yaml

---

## 7. 查看结果

- 测试触发后,任务进入 Runtime,结果会发飞书卡片(含 verdict 与各 case 耗时)
- 附件(日志/result.json)直传 MinIO `hermes-evidence` 桶,路径含 task_id
- verdict 判定:规则引擎确定性完成(PASSED / TEST_FAILED / PERF_REGRESSION / INFRA_ERROR / INCONCLUSIVE)

---

## 常见坑

| 现象 | 原因 |
|---|---|
| `failure_stage: download` | MinIO 上传后未公开读,或 URL 不在白名单 |
| `schema_violation: /artifact/auth/type` | Agent 版本太旧,缺少 `none` auth 支持(≥ 6a5e7e6) |
| 设备端 `Permission denied` 执行 run.sh | workdir 挂在 noexec(如 QCS6490 的 /data);改用 /tmp |
| `device not found via adb` | soc 白名单没匹配到,或设备离线 |
| 测试秒失败但无日志 | `require_files` 里结果文件没产出;先本地跑 run.sh 验证 |
| manifest 校验不过 | 最常见:files[].sha256 与实际不符,或 requirements 缺 os/abi |
