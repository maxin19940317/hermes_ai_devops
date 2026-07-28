# 飞书指令层自然语言翻译 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让白名单用户在飞书用自然语言下达既有的四条指令，翻译结果经 Schema 与参数校验后走完全相同的执行路径，副作用指令需二次确认。

**Architecture:** 自然语言 → Hermes 翻译成**一行合法指令文本** → 回灌既有 `Parse()` → 既有 `execute()`。翻译层的值域因此等于用户手打的值域。翻译只在 `Parse` 返回 `help` 时触发；失败一律退化为今天的 usage 回复。

**Tech Stack:** Go 1.22（runtime）、Python 3.10 + FastAPI（analyze_bridge）、PostgreSQL 15、JSON Schema Draft 2020-12（santhosh-tekuri/jsonschema v5 / jsonschema for py）。

**设计文档:** `docs/superpowers/specs/2026-07-28-feishu-cmd-nl-translate-design.md`（已批准）

## Global Constraints

- 指令面严格保持 `status | devices | rerun | unquarantine` + help，**不得新增指令**。
- `execute()` 的行为不得有任何变更；翻译层禁用或失败时行为与改动前逐字节一致。
- 白名单判定永远在最前：非白名单 open_id 静默忽略，**不得触发任何 LLM 调用**。
- `command.schema.json` 的 `args` item pattern 固定为 `^[A-Za-z0-9._-]{1,64}$`（禁空白，保证渲染-解析可逆）。
- 置信度门限 `0.75`；翻译超时环境变量 `FEISHU_CMD_NL_TIMEOUT_SEC` 缺省 `60`。
- 待确认 TTL `120s`，单槽（同一 open_id 新翻译覆盖旧待确认）。
- 应答闭集：`y`/`yes` 确认、`n`/`no` 取消，均为 `strings.TrimSpace` + 小写**精确匹配**。
- bridge 侧校验用哪份 schema 由服务端写死，**不接受请求方指定**。
- 契约变更只加字段不删字段；`contracts/` 与 bridge 部署副本必须有防漂移测试。
- 时间一律 UTC；Go 错误用 wrapped errors；跨网络调用必须带 context 超时。
- 提交信息用英文，注释中文。

**命令速查：**

```bash
# Go 测试(仓库根目录)
cd runtime && go test ./... 
# Python 契约测试
.venv/bin/python -m pytest contracts/tests -q
# bridge 测试
.venv/bin/python -m pytest hermes/analyze_bridge -q
```

---

### Task 1: `command.schema.json` 契约与正反例

**Files:**
- Create: `contracts/command.schema.json`
- Create: `contracts/tests/test_command_schema.py`
- Create: `contracts/tests/examples/command/valid/{status,rerun_full,rerun_no_variant,none,confidence_zero,confidence_one}.json`
- Create: `contracts/tests/examples/command/invalid/{unknown_command,arg_with_space,arg_with_newline,too_many_args,arg_too_long,confidence_above_one,extra_field,missing_version}.json`

**Interfaces:**
- Consumes: 无（本任务是链路起点）
- Produces: `contracts/command.schema.json`，被 Task 2（bridge 部署副本）与 Task 3（Go 内嵌）消费。字段：`translation_version`(int, const 1)、`command`(enum)、`args`(array[string])、`confidence`(number)、`reason`(string)

- [ ] **Step 1: 写 schema**

创建 `contracts/command.schema.json`：

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "command.schema.json",
  "title": "command v1",
  "description": "飞书指令层 LLM 意图翻译输出(CLAUDE.md §12 Phase 2)。command 为封闭枚举,args 禁空白字符以保证渲染成一行指令文本后可被 strings.Fields 无损切回。",
  "type": "object",
  "additionalProperties": false,
  "required": ["translation_version", "command", "confidence"],
  "properties": {
    "translation_version": { "const": 1 },
    "command": {
      "type": "string",
      "enum": ["status", "devices", "rerun", "unquarantine", "none"],
      "description": "none = 信息不足或输入根本不是指令"
    },
    "args": {
      "type": "array",
      "maxItems": 3,
      "items": { "type": "string", "pattern": "^[A-Za-z0-9._-]{1,64}$" }
    },
    "confidence": { "type": "number", "minimum": 0, "maximum": 1 },
    "reason": { "type": "string", "maxLength": 200 }
  }
}
```

- [ ] **Step 2: 写正例**

`contracts/tests/examples/command/valid/status.json`：
```json
{ "translation_version": 1, "command": "status", "args": [], "confidence": 0.95, "reason": "询问整体状态" }
```

`valid/rerun_full.json`：
```json
{ "translation_version": 1, "command": "rerun",
  "args": ["9da3b9d9", "56", "aarch64_Android_SNPE_1.68"],
  "confidence": 0.92, "reason": "指代最近一次 SNPE 1.68 的失败运行" }
```

`valid/rerun_no_variant.json`：
```json
{ "translation_version": 1, "command": "rerun", "args": ["9da3b9d9", "56"], "confidence": 0.8 }
```

`valid/none.json`：
```json
{ "translation_version": 1, "command": "none", "confidence": 0.1, "reason": "与设备测试无关" }
```

`valid/confidence_zero.json`：
```json
{ "translation_version": 1, "command": "none", "confidence": 0 }
```

`valid/confidence_one.json`：
```json
{ "translation_version": 1, "command": "devices", "args": [], "confidence": 1 }
```

- [ ] **Step 3: 写反例**

`contracts/tests/examples/command/invalid/unknown_command.json`：
```json
{ "translation_version": 1, "command": "reboot", "confidence": 0.9 }
```

`invalid/arg_with_space.json`（**核心反例**：含空格会破坏渲染-解析可逆性）：
```json
{ "translation_version": 1, "command": "rerun", "args": ["9da3b9d9 56"], "confidence": 0.9 }
```

`invalid/arg_with_newline.json`：
```json
{ "translation_version": 1, "command": "rerun", "args": ["9da3b9d9\nunquarantine"], "confidence": 0.9 }
```

`invalid/too_many_args.json`：
```json
{ "translation_version": 1, "command": "rerun", "args": ["a", "b", "c", "d"], "confidence": 0.9 }
```

`invalid/arg_too_long.json`：
```json
{ "translation_version": 1, "command": "rerun",
  "args": ["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"],
  "confidence": 0.9 }
```

`invalid/confidence_above_one.json`：
```json
{ "translation_version": 1, "command": "status", "confidence": 1.5 }
```

`invalid/extra_field.json`：
```json
{ "translation_version": 1, "command": "status", "confidence": 0.9, "raw_sql": "DROP TABLE tasks" }
```

`invalid/missing_version.json`：
```json
{ "command": "status", "confidence": 0.9 }
```

- [ ] **Step 4: 写测试**

创建 `contracts/tests/test_command_schema.py`（照搬 `test_analysis_schema.py` 的形态）：

```python
"""command.json v1 (CLAUDE.md §12 Phase 2, 飞书指令层 LLM 翻译输出) 的正反例校验测试。"""
import pytest
from jsonschema import ValidationError

from contract_helpers import load_example


class TestCommandSchema:
    contract = "command"

    def test_valid_examples_pass(self, validators, valid_case):
        validators["command"].validate(load_example(valid_case))

    def test_invalid_examples_rejected(self, validators, invalid_case):
        with pytest.raises(ValidationError):
            validators["command"].validate(load_example(invalid_case))
```

- [ ] **Step 5: 把 command 挂进 conftest 的 validators**

打开 `contracts/tests/conftest.py`，找到 `validators` fixture 里罗列契约名的地方，加入 `"command"`（与 `"analysis"` 并列）。若 fixture 是按目录自动发现的，则无需改动——先读该文件确认。

- [ ] **Step 6: 跑测试**

Run: `.venv/bin/python -m pytest contracts/tests/test_command_schema.py -q`
Expected: 全部 PASS（6 正例 + 8 反例）

- [ ] **Step 7: Commit**

```bash
git add contracts/command.schema.json contracts/tests/test_command_schema.py contracts/tests/examples/command contracts/tests/conftest.py
git commit -m "feat(contracts): add command.schema.json for NL command translation"
```

---

### Task 2: `analyze_bridge` 增加 `POST /translate`

**Files:**
- Modify: `hermes/analyze_bridge/analyze_bridge.py`
- Create: `hermes/analyze_bridge/command.schema.json`（`contracts/command.schema.json` 的部署副本，逐字节相同）
- Modify: `hermes/analyze_bridge/test_analyze_bridge.py`
- Modify: `hermes/analyze_bridge/README.md`

**Interfaces:**
- Consumes: Task 1 的 `contracts/command.schema.json`
- Produces: `POST /translate`，请求体 `{prompt, raw_text, context, model?}`，2xx 返回符合 `command.schema.json` 的 JSON；缺字段 400、鉴权失败 401、重试耗尽/CLI 失败 502。被 Task 3 的 `hermesclient.Translate` 消费

- [ ] **Step 1: 复制 schema 部署副本**

```bash
cp contracts/command.schema.json hermes/analyze_bridge/command.schema.json
```

- [ ] **Step 2: 写失败的测试**

在 `hermes/analyze_bridge/test_analyze_bridge.py` 末尾追加（先读文件头部，复用既有的 `client` fixture 与假 hermes CLI 驱动方式；下面的 `fake_hermes` / `client` 名称按该文件既有 fixture 名对齐）：

```python
TRANSLATE_OK = json.dumps({
    "translation_version": 1, "command": "devices", "args": [],
    "confidence": 0.95, "reason": "询问设备状态",
})

TRANSLATE_BODY = {
    "prompt": "把用户输入翻译成一条指令",
    "raw_text": "看下设备都什么状态",
    "context": {"now": "2026-07-28T09:12:00Z", "variants": [], "recent_runs": [], "devices": []},
}


def test_translate_ok(client, fake_hermes):
    fake_hermes(stdout=TRANSLATE_OK)
    r = client.post("/translate", json=TRANSLATE_BODY, headers=AUTH)
    assert r.status_code == 200
    assert r.json()["command"] == "devices"


def test_translate_missing_field(client):
    r = client.post("/translate", json={"prompt": "x"}, headers=AUTH)
    assert r.status_code == 400


def test_translate_unauthorized(client):
    r = client.post("/translate", json=TRANSLATE_BODY, headers={"authorization": "Bearer wrong"})
    assert r.status_code == 401


def test_translate_retries_then_succeeds(client, fake_hermes):
    fake_hermes(stdout_sequence=["not json at all", TRANSLATE_OK])
    r = client.post("/translate", json=TRANSLATE_BODY, headers=AUTH)
    assert r.status_code == 200


def test_translate_exhausts_attempts(client, fake_hermes):
    fake_hermes(stdout='{"command": "reboot"}')
    r = client.post("/translate", json=TRANSLATE_BODY, headers=AUTH)
    assert r.status_code == 502


def test_translate_disables_tools(client, fake_hermes):
    calls = fake_hermes(stdout=TRANSLATE_OK)
    client.post("/translate", json=TRANSLATE_BODY, headers=AUTH)
    assert "-t" in calls[0] and calls[0][calls[0].index("-t") + 1] == ""


def test_command_schema_copy_matches_contracts():
    src = (Path(__file__).resolve().parents[2] / "contracts" / "command.schema.json").read_text(encoding="utf-8")
    dst = (Path(__file__).with_name("command.schema.json")).read_text(encoding="utf-8")
    assert src == dst
```

若既有 fixture 名与上文不同（例如没有 `fake_hermes` 工厂），按文件里 analyze 用例的既有写法改写这些用例的驱动部分，断言内容保持不变。

- [ ] **Step 3: 跑测试确认失败**

Run: `.venv/bin/python -m pytest hermes/analyze_bridge -q -k translate`
Expected: FAIL（404 Not Found —— `/translate` 路由不存在）

- [ ] **Step 4: 抽出共用的校验重试内核**

编辑 `hermes/analyze_bridge/analyze_bridge.py`。在 `SCHEMA_PATH` 附近加载第二份 schema：

```python
COMMAND_SCHEMA_PATH = Path(__file__).with_name("command.schema.json")
COMMAND_SCHEMA = json.loads(COMMAND_SCHEMA_PATH.read_text(encoding="utf-8"))

TRANSLATE_REQUIRED_FIELDS = ("prompt", "raw_text", "context")
```

把 `build_prompt` 的重试提示改为接受 schema 名（其余不变）：

```python
def build_prompt(payload: dict, prev_errors: list[str], schema_name: str = "analysis.schema.json") -> str:
    """拼一次性 prompt:平台 prompt 模板 + 规则类别 + evidence + (重试时)校验错误。"""
    parts = [
        payload["prompt"],
        "",
        f"规则引擎判定类别(rule_category): {payload['rule_category']}",
        "",
        "evidence JSON:",
        json.dumps(payload["evidence"], ensure_ascii=False),
    ]
    if prev_errors:
        parts += [
            "",
            f"注意:你上一次的输出未通过 {schema_name} 校验,错误如下。",
            "这次只输出修正后的 JSON 对象本身,不要任何其他文本:",
            *[f"- {e}" for e in prev_errors[-2:]],
        ]
    return "\n".join(parts)


def build_translate_prompt(payload: dict, prev_errors: list[str]) -> str:
    """翻译 prompt:平台 prompt 模板 + 只读上下文快照 + 用户原文 + (重试时)校验错误。"""
    parts = [
        payload["prompt"],
        "",
        "运行时上下文快照 JSON:",
        json.dumps(payload["context"], ensure_ascii=False),
        "",
        "用户原话:",
        payload["raw_text"],
    ]
    if prev_errors:
        parts += [
            "",
            "注意:你上一次的输出未通过 command.schema.json 校验,错误如下。",
            "这次只输出修正后的 JSON 对象本身,不要任何其他文本:",
            *[f"- {e}" for e in prev_errors[-2:]],
        ]
    return "\n".join(parts)


def run_with_schema(payload: dict, schema: dict, prompt_builder, label: str) -> dict:
    """调 hermes 并做 Schema 校验打回重试;全部失败抛 BridgeError(502)。
    校验用哪份 schema 由路由写死,不接受请求方指定(契约选择权不外放)。"""
    errors: list[str] = []
    for attempt in range(1, MAX_ATTEMPTS + 1):
        stdout = run_hermes(prompt_builder(payload, errors), payload.get("model") or None)
        try:
            doc = extract_json(stdout)
            jsonschema.validate(doc, schema)
            log.info("%s ok: attempt=%d", label, attempt)
            return doc
        except (ValueError, jsonschema.ValidationError) as e:
            log.warning("%s attempt %d 校验失败: %s", label, attempt, str(e)[:ERR_SNIPPET])
            errors.append(str(e)[:ERR_SNIPPET])
    raise BridgeError(502, f"输出连续 {MAX_ATTEMPTS} 次未通过 Schema 校验,降级调用方保底")


def run_analysis(payload: dict) -> dict:
    return run_with_schema(payload, ANALYSIS_SCHEMA, build_prompt, "analyze")


def run_translation(payload: dict) -> dict:
    return run_with_schema(payload, COMMAND_SCHEMA, build_translate_prompt, "translate")
```

删除原来的 `run_analysis` 函数体（已被上面的两行版本取代）。

- [ ] **Step 5: 加路由**

在 `@app.post("/analyze")` 之后追加：

```python
@app.post("/translate")
async def translate(req: Request):
    """飞书指令层意图翻译(设计文档 §3.3):自然语言 + 只读上下文快照 → 一条封闭
    枚举指令。与 /analyze 同构:工具集全禁、服务端定形校验、打回重试同一上限。"""
    if err := check_auth(req):
        return err
    try:
        payload = await req.json()
    except json.JSONDecodeError:
        return JSONResponse({"error": "请求体不是合法 JSON"}, status_code=400)
    if not isinstance(payload, dict) or any(k not in payload for k in TRANSLATE_REQUIRED_FIELDS):
        return JSONResponse({"error": f"缺少必填字段: {TRANSLATE_REQUIRED_FIELDS}"}, status_code=400)
    try:
        return await run_in_threadpool(run_translation, payload)
    except BridgeError as e:
        log.error("translate failed: %s", e.msg)
        return JSONResponse({"error": e.msg}, status_code=e.status)
```

- [ ] **Step 6: 跑全部 bridge 测试**

Run: `.venv/bin/python -m pytest hermes/analyze_bridge -q`
Expected: 全部 PASS（既有 13 例 + 新增 7 例；既有 analyze 用例不得回归）

- [ ] **Step 7: 更新 README**

在 `hermes/analyze_bridge/README.md` 的"文件"小节加入 `command.schema.json`，并在开头段落补一句路由清单与部署提醒：

```markdown
- `POST /analyze` — Analyzer:evidence → analysis.schema.json
- `POST /translate` — 飞书指令层意图翻译:自然语言 + 上下文快照 → command.schema.json

> 部署提醒:本服务**不在 `deploy/docker-compose.yml` 里**,由实例内
> `start-analyze-bridge` 启停。Runtime 侧启用 `FEISHU_CMD_NL` 之前,必须先拉新代码
> 重启本服务,并 `curl -X POST .../translate` 确认路由存在,否则全部自然语言请求 502。
```

- [ ] **Step 8: Commit**

```bash
git add hermes/analyze_bridge/
git commit -m "feat(bridge): add /translate route mirroring /analyze"
```

---

### Task 3: `hermesclient.Translate`

**Files:**
- Modify: `runtime/internal/hermesclient/hermesclient.go`
- Modify: `runtime/internal/hermesclient/http.go`
- Modify: `runtime/internal/hermesclient/prompt.go`
- Create: `runtime/internal/hermesclient/command.schema.json`（`contracts/` 副本）
- Create: `runtime/internal/hermesclient/prompts/cmd_translate_v1.md`
- Modify: `runtime/internal/hermesclient/hermesclient_test.go`

**Interfaces:**
- Consumes: Task 1 的 schema、Task 2 的 `POST /translate`
- Produces:
  - `type Translation struct { TranslationVersion int; Command string; Args []string; Confidence float64; Reason string }`
  - `type TranslateRequest struct { RawText string; Context json.RawMessage; Model string }`
  - `type Translator interface { Translate(ctx context.Context, req TranslateRequest) (*Translation, error) }`
  - `func (c *HTTPClient) Translate(...)`、`const PromptVersionTranslate = "cmd_translate_v1"`
  - 被 Task 6 的 `feishucmd.Translator` 消费

- [ ] **Step 1: 复制 schema 副本**

```bash
cp contracts/command.schema.json runtime/internal/hermesclient/command.schema.json
```

- [ ] **Step 2: 写失败的测试**

在 `runtime/internal/hermesclient/hermesclient_test.go` 追加（沿用文件里既有的 httptest 写法）：

```go
func TestTranslateURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://h:18100/analyze", "http://h:18100/translate"},
		{"http://h:18100/analyze/", "http://h:18100/translate"},
		{"http://h/hermes/analyze", "http://h/hermes/translate"},
	}
	for _, c := range cases {
		got, err := translateURL(c.in)
		if err != nil {
			t.Fatalf("translateURL(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("translateURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTranslateOK(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"translation_version":1,"command":"devices","args":[],"confidence":0.95}`))
	}))
	defer srv.Close()
	c := NewHTTPClient(Config{Endpoint: srv.URL + "/analyze"})
	tr, err := c.Translate(context.Background(), TranslateRequest{
		RawText: "看下设备状态", Context: json.RawMessage(`{"now":"2026-07-28T09:12:00Z"}`),
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if gotPath != "/translate" {
		t.Errorf("path = %q, want /translate", gotPath)
	}
	if tr.Command != "devices" || tr.Confidence != 0.95 {
		t.Errorf("unexpected translation: %+v", tr)
	}
}

func TestTranslateRejectsInvalidSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// command 不在闭枚举内:必须被本地 Schema 校验挡下(跨进程边界不信任对端)
		_, _ = w.Write([]byte(`{"translation_version":1,"command":"reboot","confidence":0.9}`))
	}))
	defer srv.Close()
	c := NewHTTPClient(Config{Endpoint: srv.URL + "/analyze"})
	if _, err := c.Translate(context.Background(), TranslateRequest{RawText: "x"}); err == nil {
		t.Fatal("want error for schema-invalid response, got nil")
	}
}

func TestTranslateNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewHTTPClient(Config{Endpoint: srv.URL + "/analyze"})
	if _, err := c.Translate(context.Background(), TranslateRequest{RawText: "x"}); err == nil {
		t.Fatal("want error for 502, got nil")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `cd runtime && go test ./internal/hermesclient/ -run Translate -v`
Expected: 编译失败 —— `undefined: translateURL` / `c.Translate undefined`

- [ ] **Step 4: 加类型与接口**

编辑 `runtime/internal/hermesclient/hermesclient.go`，在文件末尾追加：

```go
// Translation 与 contracts/command.schema.json 字段一一对应,是意图翻译的结构化输出。
// Command 是封闭枚举(status|devices|rerun|unquarantine|none);none 表示信息不足
// 或输入根本不是指令。
type Translation struct {
	TranslationVersion int      `json:"translation_version"` // 契约固定为 1
	Command            string   `json:"command"`
	Args               []string `json:"args,omitempty"`
	Confidence         float64  `json:"confidence"`
	Reason             string   `json:"reason,omitempty"`
}

// TranslateRequest 是一次意图翻译请求的入参。Context 是 Runtime 组装的只读上下文
// 快照(设计文档 §4.2);Model 可选透传。
type TranslateRequest struct {
	RawText string
	Context json.RawMessage
	Model   string
}

// Translator 是意图翻译能力的抽象。实现需保证:尊重 ctx 超时;响应必须通过内嵌
// command.schema.json 校验,否则返回错误(校验不过视为翻译失败,调用方回 usage)。
type Translator interface {
	Translate(ctx context.Context, req TranslateRequest) (*Translation, error)
}
```

- [ ] **Step 5: 加 prompt**

编辑 `runtime/internal/hermesclient/prompt.go`，改为：

```go
package hermesclient

import _ "embed"

// PromptVersionAnalyze 是 Analyzer 当前 prompt 版本号,随请求发送便于平台侧追踪。
const PromptVersionAnalyze = "analyze_v1"

// PromptVersionTranslate 是意图翻译当前 prompt 版本号。
const PromptVersionTranslate = "cmd_translate_v1"

// PromptAnalyze 是编译进二进制的 prompt 文本(prompts/analyze_v1.md)。
// 约束:只依据 evidence 分析、证据不足明说、禁止臆测、只输出符合契约的 JSON。
//
//go:embed prompts/analyze_v1.md
var PromptAnalyze string

// PromptTranslate 是编译进二进制的意图翻译 prompt(prompts/cmd_translate_v1.md)。
// 约束:只输出封闭枚举内的指令、参数只能来自上下文快照、拿不准就返回 none。
//
//go:embed prompts/cmd_translate_v1.md
var PromptTranslate string
```

同步把 `http.go` 里 `Analyze` 用到的 `PromptVersion` / `Prompt` 改名为 `PromptVersionAnalyze` / `PromptAnalyze`（各一处）。

创建 `runtime/internal/hermesclient/prompts/cmd_translate_v1.md`：

```markdown
你是一个把中文自然语言映射成运维指令的翻译器。你**只做翻译**,不执行任何操作,
也不回答开放问题。

## 可用指令(封闭枚举,不得发明新指令)

- `status` — 查看运行中 workflow 数、活跃租约数、设备状态汇总。无参数。
- `devices` — 列出全部设备(serial/soc/status/fail_streak)。无参数。
- `rerun <sha> <pipeline_iid> [variant]` — 重跑某次构建的设备测试。
  `sha` 是 7-40 位小写十六进制;`pipeline_iid` 是正整数;`variant` 可选,
  省略表示重跑该次构建的全部变体。
- `unquarantine [device_id]` — 解除设备隔离。只有一台设备时可省略 device_id。

## 输入

你会收到两部分:

1. **上下文快照 JSON** — 包含 `now`(当前 UTC 时间)、`variants`(全部合法变体名)、
   `recent_runs`(最近若干次运行:commit/pipeline_iid/variant/verdict/ended_at)、
   `devices`(设备 id/serial/status)。
2. **用户原话** — 一句中文。

## 规则

1. 只输出一个 JSON 对象,不要任何解释文字、不要 markdown 代码围栏。
2. `command` 必须是上面五个值之一(含 `none`)。
3. `args` 里的每一项**只能来自上下文快照**或用户原话中明确出现的字面量。
   绝不允许编造 commit sha、pipeline_iid、变体名或设备 id。
4. `args` 的每一项不得包含空格、换行或任何空白字符。
5. 解析"昨天""最近一次""上一个失败的"这类指代时,以快照的 `now` 为基准,
   在 `recent_runs` 里查找;找不到唯一匹配就返回 `none`。
6. 变体名必须与 `variants` 中的某一项**完全一致**(用户可能只说"SNPE 1.68",
   你需要补全成 `aarch64_Android_SNPE_1.68`;若有多个候选无法区分,返回 `none`)。
7. `confidence` 如实反映把握程度。信息不足、指代不明、用户在问开放问题
   (如"为什么失败""成功率多少")一律 `command: "none"` 并给出简短 `reason`。
8. 宁可返回 `none`,也不要猜。猜错的代价是白跑一轮设备测试。

## 输出格式

```json
{"translation_version":1,"command":"rerun","args":["9da3b9d9","56","aarch64_Android_SNPE_1.68"],"confidence":0.92,"reason":"指代最近一次 SNPE 1.68 的失败运行"}
```
```

- [ ] **Step 6: 实现 Translate**

编辑 `runtime/internal/hermesclient/http.go`。在 `analysisSchema` 附近加：

```go
//go:embed command.schema.json
var commandSchemaJSON string

// commandSchema 是编译期嵌入的 contracts/command.schema.json(Draft2020)。
var commandSchema = mustCompileSchema("command.schema.json", commandSchemaJSON)

func mustCompileSchema(name, body string) *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource(name, strings.NewReader(body)); err != nil {
		panic(err)
	}
	return c.MustCompile(name)
}
```

把既有的 `mustCompileAnalysisSchema()` 改为 `mustCompileSchema("analysis.schema.json", analysisSchemaJSON)`，删除旧函数。

在文件末尾追加：

```go
// translatePayload 是发往 bridge POST /translate 的请求格式(§3.3)。
type translatePayload struct {
	PromptVersion string          `json:"prompt_version"`
	Model         string          `json:"model,omitempty"`
	Prompt        string          `json:"prompt"`
	RawText       string          `json:"raw_text"`
	Context       json.RawMessage `json:"context"`
}

// translateURL 由 Endpoint(指向 /analyze)推导出同一 bridge 的 /translate:
// 替换路径最后一段。不新增环境变量,避免两个 URL 被配到不同实例上(设计文档 §7)。
func translateURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("hermesclient: Endpoint 不是合法 URL: %w", err)
	}
	p := strings.TrimRight(u.Path, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		u.Path = p[:i] + "/translate"
	} else {
		u.Path = "/translate"
	}
	return u.String(), nil
}

// Translate 调用 bridge 执行一次意图翻译:响应经内嵌 command.schema.json 校验后
// 解析;校验不过或非 2xx 均返回 wrapped error,由调用方回退 usage(设计文档 §6)。
func (c *HTTPClient) Translate(ctx context.Context, req TranslateRequest) (*Translation, error) {
	endpoint, err := translateURL(c.cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	ctxJSON := req.Context
	if len(ctxJSON) == 0 {
		ctxJSON = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(translatePayload{
		PromptVersion: PromptVersionTranslate,
		Model:         req.Model,
		Prompt:        PromptTranslate,
		RawText:       req.RawText,
		Context:       ctxJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 编码翻译请求失败: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 构造翻译请求失败: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.cfg.AuthToken != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.cfg.AuthToken)
	}
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 调用 %s 失败: %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("hermesclient: 读取翻译响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := string(raw)
		if len(snippet) > errBodyLimit {
			snippet = snippet[:errBodyLimit] + "..."
		}
		return nil, fmt.Errorf("hermesclient: 平台返回 %d: %s", resp.StatusCode, snippet)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("hermesclient: 翻译响应不是合法 JSON: %w", err)
	}
	if err := commandSchema.Validate(doc); err != nil {
		return nil, fmt.Errorf("hermesclient: 响应不符合 command.schema.json(视为翻译失败): %w", err)
	}
	var tr Translation
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("hermesclient: 解析 Translation 失败: %w", err)
	}
	return &tr, nil
}
```

在 import 块补 `"net/url"`。

- [ ] **Step 7: 加防漂移测试**

在 `hermesclient_test.go` 追加：

```go
func TestCommandSchemaCopyMatchesContracts(t *testing.T) {
	src, err := os.ReadFile("../../../contracts/command.schema.json")
	if err != nil {
		t.Fatalf("read contracts copy: %v", err)
	}
	if string(src) != commandSchemaJSON {
		t.Error("command.schema.json 与 contracts/ 不一致,请同步副本")
	}
}
```

- [ ] **Step 8: 跑测试**

Run: `cd runtime && go test ./internal/hermesclient/ -v`
Expected: 全部 PASS（含既有 Analyze 用例）

- [ ] **Step 9: Commit**

```bash
git add runtime/internal/hermesclient/
git commit -m "feat(runtime): add hermesclient.Translate with command schema validation"
```

---

### Task 4: `command_translations` 表与审计写入

**Files:**
- Modify: `runtime/internal/store/schema.sql`
- Create: `deploy/postgres/migrations/2026-07-28-command-translations.sql`
- Create: `runtime/internal/store/translations.go`
- Create: `runtime/internal/store/postgres_translations.go`
- Modify: `runtime/internal/store/store.go`（MemStore 字段）
- Modify: `runtime/internal/store/conformance_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `type CommandTranslation struct { OpenID, RawText, PromptVersion, Model, ContextDigest string; Output []byte; Rendered, Outcome string }`
  - `func (s *MemStore|*PGStore) SaveCommandTranslation(ctx, row CommandTranslation) error`
  - `func (s *MemStore) CommandTranslations() []CommandTranslation`（仅测试用）
  - outcome 常量 `OutcomeExecuted` 等，被 Task 6/7 消费

- [ ] **Step 1: 写失败的 conformance 测试**

`conformance_test.go` 的形态是：一个 `fullStore` 接口（`:19`）罗列全部方法，
`runConformance`（`:60`）里用 `t.Run("名字", func(t *testing.T){ s := newStore(t); ... })`
写子测试，`ctx` 是包级变量。**先把两个新方法加进 `fullStore` 接口**：

```go
	SaveCommandTranslation(ctx context.Context, row CommandTranslation) error
	ListCommandTranslations(ctx context.Context, openID string, limit int) ([]CommandTranslation, error)
	RecentRuns(ctx context.Context, limit int) ([]RecentRun, error)
```

再在 `runConformance` 内追加子测试：

```go
	t.Run("CommandTranslationsAppendOnly", func(t *testing.T) {
		s := newStore(t)
		rows := []CommandTranslation{
		{OpenID: "ou_1", RawText: "看下设备状态", PromptVersion: "cmd_translate_v1",
			Model: "m", ContextDigest: "abc", Output: []byte(`{"command":"devices"}`),
			Rendered: "devices", Outcome: OutcomeExecuted},
		{OpenID: "ou_1", RawText: "重跑昨天那个", ContextDigest: "def",
			Output: []byte(`{"command":"rerun"}`), Rendered: "rerun 9da3b9d9 56",
			Outcome: OutcomePendingConfirm},
			{OpenID: "ou_1", RawText: "y", Rendered: "rerun 9da3b9d9 56", Outcome: OutcomeConfirmed},
		}
		for _, r := range rows {
			if err := s.SaveCommandTranslation(ctx, r); err != nil {
				t.Fatalf("SaveCommandTranslation: %v", err)
			}
		}
		// 追加式审计:三行都在,顺序即时序
		got, err := s.ListCommandTranslations(ctx, "ou_1", 10)
		if err != nil {
			t.Fatalf("ListCommandTranslations: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[0].Outcome != OutcomeConfirmed {
			t.Errorf("最新一行 outcome = %q, want %q", got[0].Outcome, OutcomeConfirmed)
		}
	})

	t.Run("CommandTranslationTruncatesOutput", func(t *testing.T) {
		s := newStore(t)
		big := append([]byte(`{"junk":"`), bytes.Repeat([]byte("x"), 8000)...)
		big = append(big, []byte(`"}`)...)
		if err := s.SaveCommandTranslation(ctx, CommandTranslation{
			OpenID: "ou_2", RawText: "x", Output: big, Outcome: OutcomeRejectedSchema,
		}); err != nil {
			t.Fatalf("SaveCommandTranslation: %v", err)
		}
		got, err := s.ListCommandTranslations(ctx, "ou_2", 1)
		if err != nil {
			t.Fatalf("ListCommandTranslations: %v", err)
		}
		// 上限留一倍余量:PG 侧非法 JSON 会被包装成 {"raw":"..."} 再存,
		// 转义后略长于 outputLimit,断言的是"截断生效"而非精确字节数。
		if len(got[0].Output) > outputLimit*2 {
			t.Errorf("output 未截断: %d(原始 8000+)", len(got[0].Output))
		}
	})
```

import 补 `"bytes"`。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/store/ -run CommandTranslation`
Expected: 编译失败 —— `undefined: CommandTranslation`

- [ ] **Step 3: 写类型与 MemStore 实现**

创建 `runtime/internal/store/translations.go`：

```go
package store

import "context"

// 翻译审计 outcome 取值(设计文档 §4.3)。追加式:确认流程不更新已有行,只追加新行。
const (
	OutcomeExecuted             = "executed"
	OutcomePendingConfirm       = "pending_confirm"
	OutcomeConfirmed            = "confirmed"
	OutcomeDeclined             = "declined"
	OutcomeExpired              = "expired"
	OutcomeRejectedSchema       = "rejected_schema"
	OutcomeRejectedNone         = "rejected_none"
	OutcomeRejectedArgs         = "rejected_args"
	OutcomeRejectedLowConfidence = "rejected_low_confidence"
	OutcomeTranslatorError      = "translator_error"
)

// outputLimit 是 output 列的落库上限(4KB)。Schema 校验失败时平台可能返回任意
// 长度的自由文本,审计要留证但不该让一行把表撑坏(设计文档 §4.3)。
const outputLimit = 4096

// truncatedMark 是被截断输出的尾标记。
const truncatedMark = "...(truncated)"

// CommandTranslation 对应 command_translations 表一行(设计文档 §4.3)。
type CommandTranslation struct {
	OpenID        string
	RawText       string
	PromptVersion string
	Model         string
	ContextDigest string // 上下文快照 sha256,可回放"当时看到了什么"
	Output        []byte // LLM 原始输出(校验失败也存),落库前截断至 outputLimit
	Rendered      string // 渲染出的那行指令
	Outcome       string
}

// truncateOutput 把 output 截断到 outputLimit 并加尾标记;短于上限时原样返回。
func truncateOutput(b []byte) []byte {
	if len(b) <= outputLimit {
		return b
	}
	out := make([]byte, 0, outputLimit+len(truncatedMark))
	out = append(out, b[:outputLimit]...)
	return append(out, truncatedMark...)
}

// SaveCommandTranslation 追加一行翻译审计。
func (s *MemStore) SaveCommandTranslation(_ context.Context, row CommandTranslation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row.Output = truncateOutput(row.Output)
	s.translations = append(s.translations, row)
	return nil
}

// ListCommandTranslations 按时间倒序返回某 open_id 的翻译审计(最新在前)。
func (s *MemStore) ListCommandTranslations(_ context.Context, openID string, limit int) ([]CommandTranslation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []CommandTranslation{}
	for i := len(s.translations) - 1; i >= 0 && len(out) < limit; i-- {
		if s.translations[i].OpenID == openID {
			out = append(out, s.translations[i])
		}
	}
	return out, nil
}
```

在 `runtime/internal/store/store.go` 的 `MemStore` 结构体加字段：

```go
	// translations 是 command_translations 表(设计文档 §4.3)的内存视图。
	translations []CommandTranslation
```

- [ ] **Step 4: 写 DDL**

在 `runtime/internal/store/schema.sql` 末尾追加：

```sql
-- 飞书指令层自然语言翻译审计(设计文档 §4.3)。翻译发生在任何 task 存在之前,
-- 无 task_id 可填,故不能复用 decisions 表(其 task_id 是 NOT NULL 外键)。
-- 追加式:确认流程不更新已有行,pending_confirm 与 confirmed 各占一行。
CREATE TABLE IF NOT EXISTS command_translations (
    translation_id BIGSERIAL PRIMARY KEY,
    open_id        TEXT        NOT NULL,
    raw_text       TEXT        NOT NULL,
    prompt_version TEXT        NOT NULL DEFAULT '',
    model          TEXT        NOT NULL DEFAULT '',
    context_digest TEXT        NOT NULL DEFAULT '',   -- 快照 sha256,可回放"当时看到了什么"
    output         JSONB       NOT NULL DEFAULT '{}', -- LLM 原始输出(校验失败也存,落库前截断 4KB)
    rendered       TEXT        NOT NULL DEFAULT '',   -- 渲染出的那行指令
    outcome        TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS command_translations_open_id_idx
    ON command_translations(open_id, created_at DESC);
```

创建 `deploy/postgres/migrations/2026-07-28-command-translations.sql`，内容与上面这段完全相同（照既有迁移文件的形态，幂等可重复执行）。

- [ ] **Step 5: 写 PGStore 实现**

创建 `runtime/internal/store/postgres_translations.go`（照 `postgres_decisions.go` 的写法）：

```go
package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// SaveCommandTranslation 追加一行翻译审计(设计文档 §4.3)。
// output 非合法 JSON 时(平台跑飞返回自由文本)包装成 {"raw": "..."} 存入,
// 保证 JSONB 列可写——审计要留证,不能因为输出是垃圾就丢掉。
func (s *PGStore) SaveCommandTranslation(ctx context.Context, row CommandTranslation) error {
	out := truncateOutput(row.Output)
	if len(out) == 0 || !json.Valid(out) {
		wrapped, err := json.Marshal(map[string]string{"raw": string(out)})
		if err != nil {
			return fmt.Errorf("save command translation: wrap output: %w", err)
		}
		out = wrapped
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO command_translations
		    (open_id, raw_text, prompt_version, model, context_digest, output, rendered, outcome)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		row.OpenID, row.RawText, row.PromptVersion, row.Model, row.ContextDigest,
		string(out), row.Rendered, row.Outcome)
	if err != nil {
		return fmt.Errorf("save command translation: %w", err)
	}
	return nil
}

// ListCommandTranslations 按时间倒序返回某 open_id 的翻译审计(最新在前)。
func (s *PGStore) ListCommandTranslations(ctx context.Context, openID string, limit int) ([]CommandTranslation, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT open_id, raw_text, prompt_version, model, context_digest, output::text, rendered, outcome
		FROM command_translations
		WHERE open_id = $1
		ORDER BY created_at DESC, translation_id DESC
		LIMIT $2`, openID, limit)
	if err != nil {
		return nil, fmt.Errorf("list command translations: %w", err)
	}
	defer rows.Close()
	out := []CommandTranslation{}
	for rows.Next() {
		var r CommandTranslation
		var output string
		if err := rows.Scan(&r.OpenID, &r.RawText, &r.PromptVersion, &r.Model,
			&r.ContextDigest, &output, &r.Rendered, &r.Outcome); err != nil {
			return nil, fmt.Errorf("list command translations: scan: %w", err)
		}
		r.Output = []byte(output)
		out = append(out, r)
	}
	return out, rows.Err()
}
```

注意 MemStore 的 `truncateOutput` 与 PG 一致；`json.Valid` 包装只在 PG 侧做（JSONB 列约束），MemStore 存原始字节即可——conformance 测试只断言长度上限，不断言字节完全一致。

若 `Store` 接口（`store.go` 里的聚合接口）显式罗列方法，把这两个方法加进去。

- [ ] **Step 6: 跑测试**

Run: `cd runtime && go test ./internal/store/ -run CommandTranslation -v`
Expected: PASS（PGStore 用例在无数据库时按既有 `pgtest_test.go` 的方式跳过）

- [ ] **Step 7: Commit**

```bash
git add runtime/internal/store/ deploy/postgres/migrations/
git commit -m "feat(runtime): add command_translations audit table"
```

---

### Task 5: `RecentRuns` 快照来源

**Files:**
- Modify: `runtime/internal/workflow/types.go`（提取 `BaseWorkflowID`）
- Modify: `runtime/internal/workflow/types_test.go` 或就近的 workflow 测试文件
- Modify: `runtime/internal/store/fleet.go`（MemStore 实现）
- Modify: `runtime/internal/store/postgres_fleet.go`（PG 实现）
- Modify: `runtime/internal/store/store.go`（artifacts/tasks 插入序）
- Modify: `runtime/internal/store/tasks.go`（taskRecord 加序号与 endedAt）
- Modify: `runtime/internal/store/conformance_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `func wf.BaseWorkflowID(project, commit string, pipelineID int) string`
  - `type RecentRun struct { Project, Commit string; PipelineID int; Variant, Verdict string; EndedAt time.Time }`
  - `func (s *MemStore|*PGStore) RecentRuns(ctx, limit int) ([]RecentRun, error)`
  - 被 Task 6 的快照组装消费

- [ ] **Step 1: 写失败的测试（BaseWorkflowID 同源）**

在 `runtime/internal/workflow/` 下既有的 types 测试文件追加（若无该文件则创建
`types_test.go`，package 与同目录一致；import 需含 `"strings"`、`"testing"`）：

```go
func TestWorkflowIDBuiltOnBase(t *testing.T) {
	in := DeviceTestInput{Project: "algo-super-sdk", Commit: "9da3b9d9", PipelineID: 56}
	base := BaseWorkflowID(in.Project, in.Commit, in.PipelineID)
	if base != "device-test-algo-super-sdk-g9da3b9d9-p56" {
		t.Fatalf("base = %q", base)
	}
	// bundle 级:ID 就是 base
	if got := in.WorkflowID(); got != base {
		t.Errorf("bundle WorkflowID = %q, want %q", got, base)
	}
	// 变体级与 retry:必须以 base + "-" 开头
	in.Scope, in.Attempt = "aarch64_Android_SNPE_1.68", 2
	if got := in.WorkflowID(); !strings.HasPrefix(got, base+"-") {
		t.Errorf("scoped WorkflowID = %q, want prefix %q", got, base+"-")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/workflow/ -run WorkflowIDBuiltOnBase`
Expected: 编译失败 —— `undefined: BaseWorkflowID`

- [ ] **Step 3: 提取 BaseWorkflowID**

编辑 `runtime/internal/workflow/types.go`，把 `WorkflowID()` 改为：

```go
// BaseWorkflowID 是 workflow ID 去掉 scope/attempt 后缀的公共前缀。
// RecentRuns 用它回查 tasks(设计文档 §3.2):格式只此一处定义,Go 侧单一真相来源,
// 不在 SQL 里重复拼接,格式漂移在编译期就不可能发生。
func BaseWorkflowID(project, commit string, pipelineID int) string {
	return "device-test-" + project + "-g" + commit + "-p" + strconv.Itoa(pipelineID)
}

func (in DeviceTestInput) WorkflowID() string {
	id := BaseWorkflowID(in.Project, in.Commit, in.PipelineID)
	if in.Scope != "" {
		id += "-" + in.Scope
	}
	if in.Attempt > 0 {
		id += "-r" + strconv.Itoa(in.Attempt)
	}
	return id
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd runtime && go test ./internal/workflow/ -run WorkflowIDBuiltOnBase -v`
Expected: PASS

- [ ] **Step 5: 写失败的 RecentRuns conformance 测试**

在 `runConformance` 内追加两个子测试（形态同 Task 4：`s := newStore(t)`，`ctx` 是包级变量）：

```go
	t.Run("RecentRunsFiltersByTestID", func(t *testing.T) {
		s := newStore(t)
		const proj, sha, iid = "Algo_Super_SDK", "9da3b9d9", 56 // 项目名含下划线:通配符地雷
		v1, v2 := "aarch64_Android_SNPE_1.68", "aarch64_Android_SNPE_2.21"
		base := wf.BaseWorkflowID(proj, sha, iid)

		if err := s.RegisterArtifacts(ctx, []Artifact{
			{Project: proj, CommitSHA: sha, PipelineID: iid, Variant: v1, URL: "u1", SHA256: "s1"},
			{Project: proj, CommitSHA: sha, PipelineID: iid, Variant: v2, URL: "u2", SHA256: "s2"},
		}); err != nil {
			t.Fatalf("RegisterArtifacts: %v", err)
		}

		// bundle workflow:两个变体的 task 挂在同一个 workflow_id 上,靠 test_id 区分
		for _, tc := range []struct{ variant, verdict string }{{v1, "TEST_FAILED"}, {v2, "PASSED"}} {
			taskID := base + ":" + tc.variant + ":a1"
			if err := s.CreateTask(ctx, wf.TaskRow{
				TaskID: taskID, WorkflowID: base, TestID: tc.variant, Attempt: 1,
				IdempotencyKey: taskID, Status: "RUNNING",
			}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			if err := s.FinishTask(ctx, wf.FinishRequest{
				TaskID: taskID, Status: "COMPLETED", Verdict: tc.verdict,
			}); err != nil {
				t.Fatalf("FinishTask: %v", err)
			}
		}

		runs, err := s.RecentRuns(ctx, 10)
		if err != nil {
			t.Fatalf("RecentRuns: %v", err)
		}
		byVariant := map[string]RecentRun{}
		for _, r := range runs {
			byVariant[r.Variant] = r
		}
		if got := byVariant[v1].Verdict; got != "TEST_FAILED" {
			t.Errorf("%s verdict = %q, want TEST_FAILED(bundle 下必须按 test_id 过滤,不得串变体)", v1, got)
		}
		if got := byVariant[v2].Verdict; got != "PASSED" {
			t.Errorf("%s verdict = %q, want PASSED", v2, got)
		}

		// 变体级 retry workflow:更晚的行应覆盖 bundle 的结论
		retryWF := base + "-" + v1 + "-r2"
		retryTask := retryWF + ":" + v1 + ":a1"
		if err := s.CreateTask(ctx, wf.TaskRow{
			TaskID: retryTask, WorkflowID: retryWF, TestID: v1, Attempt: 1,
			IdempotencyKey: retryTask, Status: "RUNNING",
		}); err != nil {
			t.Fatalf("CreateTask retry: %v", err)
		}
		if err := s.FinishTask(ctx, wf.FinishRequest{
			TaskID: retryTask, Status: "COMPLETED", Verdict: "PASSED",
		}); err != nil {
			t.Fatalf("FinishTask retry: %v", err)
		}
		runs, err = s.RecentRuns(ctx, 10)
		if err != nil {
			t.Fatalf("RecentRuns after retry: %v", err)
		}
		for _, r := range runs {
			if r.Variant == v1 && r.Verdict != "PASSED" {
				t.Errorf("retry 后 %s verdict = %q, want PASSED(应取最新一条)", v1, r.Verdict)
			}
		}
	})

	t.Run("RecentRunsRespectsLimit", func(t *testing.T) {
		s := newStore(t)
		arts := []Artifact{}
		for i := 0; i < 5; i++ {
			arts = append(arts, Artifact{
				Project: "p", CommitSHA: fmt.Sprintf("sha%d", i), PipelineID: i + 1,
				Variant: "v", URL: "u", SHA256: "s",
			})
		}
		if err := s.RegisterArtifacts(ctx, arts); err != nil {
			t.Fatalf("RegisterArtifacts: %v", err)
		}
		runs, err := s.RecentRuns(ctx, 3)
		if err != nil {
			t.Fatalf("RecentRuns: %v", err)
		}
		if len(runs) != 3 {
			t.Errorf("len = %d, want 3", len(runs))
		}
	})
```

`RecentRuns` 已在 Task 4 Step 1 加进 `fullStore` 接口。import 补 `"fmt"`（若尚未引入）。

- [ ] **Step 6: 跑测试确认失败**

Run: `cd runtime && go test ./internal/store/ -run RecentRuns`
Expected: 编译失败 —— `undefined: RecentRun`

- [ ] **Step 7: MemStore 加插入序与终态时间**

编辑 `runtime/internal/store/store.go`，给 `MemStore` 加字段：

```go
	// seq 是插入序计数器,给 artifacts/tasks 提供确定的"最近"排序
	// (内存实现无 created_at 列)。
	seq        int64
	rowSeq     map[string]int64 // artifacts key → 插入序
```

在 `NewMemStore()` 里初始化 `rowSeq: map[string]int64{}`。

在 `RegisterArtifacts` 里，成功插入新行时记录序号（在 `s.rows[key] = a` 之后）：

```go
			s.seq++
			s.rowSeq[key] = s.seq
```

编辑 `runtime/internal/store/tasks.go`，给 `taskRecord` 加两个字段：

```go
type taskRecord struct {
	row      wf.TaskRow
	verdict  string
	category string
	reason   string
	seq      int64     // 插入序,给"最新一条"提供确定顺序
	endedAt  time.Time // FinishTask 落终态的时刻(UTC)
}
```

`CreateTask` 里插入时赋序号：

```go
	s.seq++
	s.tasks[row.TaskID] = &taskRecord{row: row, seq: s.seq}
```

`FinishTask` 里补时间戳（在赋 verdict 之后）：

```go
	rec.endedAt = time.Now().UTC()
```

在 `tasks.go` 的 import 补 `"time"`。

- [ ] **Step 8: 实现 MemStore.RecentRuns**

在 `runtime/internal/store/fleet.go` 末尾追加：

```go
// RecentRun 是快照里的一次运行(设计文档 §4.2)。Verdict 为空表示尚无终态结论。
type RecentRun struct {
	Project    string
	Commit     string
	PipelineID int
	Variant    string
	Verdict    string
	EndedAt    time.Time
}

// RecentRuns 返回最近 limit 条产物及其最新一次运行结论(飞书指令层翻译上下文)。
// 关联规则(设计文档 §3.2):同一 (commit,iid,variant) 的 task 可能挂在 bundle
// workflow(ID = base)、变体 workflow(base-variant)或两者的 -r{N} 重跑下,
// 且 bundle 下多个变体共享同一 workflow_id——必须同时按 test_id 过滤才不串变体。
func (s *MemStore) RecentRuns(_ context.Context, limit int) ([]RecentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type keyed struct {
		art Artifact
		seq int64
	}
	all := make([]keyed, 0, len(s.rows))
	for k, a := range s.rows {
		all = append(all, keyed{art: a, seq: s.rowSeq[k]})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq > all[j].seq })
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]RecentRun, 0, len(all))
	for _, k := range all {
		a := k.art
		run := RecentRun{
			Project: a.Project, Commit: a.CommitSHA,
			PipelineID: a.PipelineID, Variant: a.Variant,
		}
		base := wf.BaseWorkflowID(a.Project, a.CommitSHA, a.PipelineID)
		var best *taskRecord
		for _, rec := range s.tasks {
			if rec.row.TestID != a.Variant {
				continue
			}
			id := rec.row.WorkflowID
			if id != base && !strings.HasPrefix(id, base+"-") {
				continue
			}
			if best == nil || rec.seq > best.seq {
				best = rec
			}
		}
		if best != nil {
			run.Verdict, run.EndedAt = best.verdict, best.endedAt
		}
		out = append(out, run)
	}
	return out, nil
}
```

在 `fleet.go` 的 import 补 `"sort"`、`"strings"`、`"time"`，以及 `wf "hermes-devops/runtime/internal/workflow"`。

- [ ] **Step 9: 实现 PGStore.RecentRuns**

在 `runtime/internal/store/postgres_fleet.go` 末尾追加：

```go
// RecentRuns 见 MemStore 同名方法的语义说明(设计文档 §3.2)。
// 实现为 1 + limit 次查询而非单条 SQL:baseID 的构造只在 Go 侧(wf.BaseWorkflowID)
// 存在一份,不在 SQL 里重复拼接字符串——格式漂移在编译期即不可能。limit 为 10 量级,
// 且只在人机交互路径上调用,查询次数可接受。
func (s *PGStore) RecentRuns(ctx context.Context, limit int) ([]RecentRun, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT project, commit_sha, pipeline_id, variant
		FROM artifacts
		ORDER BY created_at DESC, artifact_id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent runs: %w", err)
	}
	out := []RecentRun{}
	for rows.Next() {
		var r RecentRun
		if err := rows.Scan(&r.Project, &r.Commit, &r.PipelineID, &r.Variant); err != nil {
			rows.Close()
			return nil, fmt.Errorf("recent runs: scan: %w", err)
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent runs: %w", err)
	}
	for i := range out {
		base := wf.BaseWorkflowID(out[i].Project, out[i].Commit, out[i].PipelineID)
		var verdict sql.NullString
		var endedAt sql.NullTime
		// starts_with 而非 LIKE:项目名可能含下划线(Algo_Super_SDK),
		// 而 _ 是 LIKE 的单字符通配符,走 LIKE 就得加 ESCAPE。
		err := s.DB.QueryRowContext(ctx, `
			SELECT verdict, ended_at FROM tasks
			WHERE test_id = $1
			  AND (workflow_id = $2 OR starts_with(workflow_id, $2 || '-'))
			ORDER BY created_at DESC, task_id DESC
			LIMIT 1`, out[i].Variant, base).Scan(&verdict, &endedAt)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("recent runs: lookup %s: %w", out[i].Variant, err)
		}
		out[i].Verdict = verdict.String
		if endedAt.Valid {
			out[i].EndedAt = endedAt.Time.UTC()
		}
	}
	return out, nil
}
```

import 需含 `"database/sql"`、`"errors"`、`"fmt"`、`wf "hermes-devops/runtime/internal/workflow"`。

- [ ] **Step 10: 跑测试**

Run: `cd runtime && go test ./internal/store/ ./internal/workflow/ -v -run 'RecentRuns|WorkflowID'`
Expected: 全部 PASS

- [ ] **Step 11: Commit**

```bash
git add runtime/internal/store/ runtime/internal/workflow/
git commit -m "feat(runtime): add RecentRuns for command translation context"
```

---

### Task 6: `feishucmd.Translator`（渲染、回灌、复校、门限）

**Files:**
- Create: `runtime/internal/feishucmd/translate.go`
- Create: `runtime/internal/feishucmd/translate_test.go`
- Modify: `runtime/internal/feishucmd/executor.go`（扩展 `Store` 接口）

**Interfaces:**
- Consumes: Task 3 的 `hermesclient.Translator`/`Translation`、Task 4 的 outcome 常量与 `SaveCommandTranslation`、Task 5 的 `RecentRuns`
- Produces:
  - `func render(cmd string, args []string) string`
  - `type Translator struct { Client hermesclient.Translator; Store Store; Variants []string; Model string; Now func() time.Time }`
  - `type TranslateResult struct { Cmd Command; Rendered string; Reason string; NeedsConfirm bool; Outcome string; Reply string; OK bool }`
  - `func (t *Translator) Translate(ctx, openID, rawText string) TranslateResult`
  - 被 Task 7 的 `Executor` 消费

- [ ] **Step 1: 写失败的测试**

创建 `runtime/internal/feishucmd/translate_test.go`：

```go
package feishucmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/store"
)

// fakeTranslator 记录调用次数,便于断言"某些路径下零 LLM 调用"。
type fakeTranslator struct {
	out   *hermesclient.Translation
	err   error
	calls int
	gotCtxJSON string
}

func (f *fakeTranslator) Translate(_ context.Context, req hermesclient.TranslateRequest) (*hermesclient.Translation, error) {
	f.calls++
	f.gotCtxJSON = string(req.Context)
	return f.out, f.err
}

func newTranslator(f *fakeTranslator, st Store) *Translator {
	return &Translator{
		Client:   f,
		Store:    st,
		Variants: []string{"aarch64_Android_SNPE_1.68", "aarch64_Android_SNPE_2.21"},
		Now:      func() time.Time { return time.Date(2026, 7, 28, 9, 12, 0, 0, time.UTC) },
	}
}

// TestRenderParseRoundTrip 是方案 1 封闭性的核心断言:schema 允许的任何输出
// 渲染成一行文本后,Parse 必须无损切回同一条指令。
func TestRenderParseRoundTrip(t *testing.T) {
	cases := []struct {
		cmd  string
		args []string
	}{
		{"status", nil},
		{"devices", []string{}},
		{"rerun", []string{"9da3b9d9", "56"}},
		{"rerun", []string{"9da3b9d9", "56", "aarch64_Android_SNPE_1.68"}},
		{"unquarantine", []string{"dev-1"}},
		{"unquarantine", []string{"a.b_c-d"}},
	}
	for _, c := range cases {
		line := render(c.cmd, c.args)
		got := Parse(line)
		if got.Name != c.cmd {
			t.Errorf("render+Parse(%q,%v) name = %q, want %q", c.cmd, c.args, got.Name, c.cmd)
		}
		if strings.Join(got.Args, ",") != strings.Join(c.args, ",") {
			t.Errorf("render+Parse(%q,%v) args = %v, want %v", c.cmd, c.args, got.Args, c.args)
		}
	}
}

func TestTranslateReadOnlyCommandExecutesDirectly(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "devices", Confidence: 0.95, Reason: "询问设备状态",
	}}
	st := store.NewMemStore()
	res := newTranslator(f, st).Translate(context.Background(), "ou_1", "看下设备状态")
	if !res.OK || res.NeedsConfirm {
		t.Fatalf("res = %+v, want OK 且不需确认", res)
	}
	if res.Rendered != "devices" || res.Outcome != store.OutcomeExecuted {
		t.Errorf("rendered=%q outcome=%q", res.Rendered, res.Outcome)
	}
}

func TestTranslateSideEffectCommandNeedsConfirm(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "rerun",
		Args: []string{"9da3b9d9", "56", "aarch64_Android_SNPE_1.68"}, Confidence: 0.92,
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "重跑昨天那个")
	if !res.OK || !res.NeedsConfirm {
		t.Fatalf("res = %+v, want OK 且需确认", res)
	}
	if res.Outcome != store.OutcomePendingConfirm {
		t.Errorf("outcome = %q", res.Outcome)
	}
}

func TestTranslateRejectsUnknownVariant(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "rerun",
		Args: []string{"9da3b9d9", "56", "aarch64_Android_RKNN_9.9"}, Confidence: 0.95,
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "重跑那个")
	if res.OK || res.Outcome != store.OutcomeRejectedArgs {
		t.Fatalf("res = %+v, want 拒绝且 rejected_args", res)
	}
}

func TestTranslateRejectsBadSHA(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "rerun", Args: []string{"zzz", "56"}, Confidence: 0.95,
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "重跑")
	if res.OK || res.Outcome != store.OutcomeRejectedArgs {
		t.Fatalf("res = %+v", res)
	}
}

func TestTranslateRejectsBadIID(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "rerun", Args: []string{"9da3b9d9", "0"}, Confidence: 0.95,
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "重跑")
	if res.OK || res.Outcome != store.OutcomeRejectedArgs {
		t.Fatalf("res = %+v", res)
	}
}

func TestTranslateRejectsLowConfidence(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "devices", Confidence: 0.5,
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "嗯")
	if res.OK || res.Outcome != store.OutcomeRejectedLowConfidence {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Reply, "devices") {
		t.Errorf("低置信度回复应带上翻译结果供人工判断: %q", res.Reply)
	}
}

func TestTranslateNone(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "none", Confidence: 0.9, Reason: "与设备测试无关",
	}}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "今天天气怎么样")
	if res.OK || res.Outcome != store.OutcomeRejectedNone {
		t.Fatalf("res = %+v", res)
	}
}

func TestTranslateClientError(t *testing.T) {
	f := &fakeTranslator{err: errors.New("boom")}
	res := newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "随便说点什么")
	if res.OK || res.Outcome != store.OutcomeTranslatorError {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Reply, "翻译服务暂时不可用") {
		t.Errorf("reply = %q", res.Reply)
	}
}

func TestTranslateSnapshotCarriesNow(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "devices", Confidence: 0.9,
	}}
	newTranslator(f, store.NewMemStore()).Translate(context.Background(), "ou_1", "设备")
	var snap map[string]any
	if err := json.Unmarshal([]byte(f.gotCtxJSON), &snap); err != nil {
		t.Fatalf("snapshot 不是合法 JSON: %v", err)
	}
	if snap["now"] != "2026-07-28T09:12:00Z" {
		t.Errorf("snapshot.now = %v,缺了它 LLM 无法锚定“昨天”", snap["now"])
	}
	for _, k := range []string{"variants", "recent_runs", "devices"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("snapshot 缺字段 %q", k)
		}
	}
}

func TestTranslateAuditsEveryOutcome(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "none", Confidence: 0.2,
	}}
	st := store.NewMemStore()
	newTranslator(f, st).Translate(context.Background(), "ou_1", "什么鬼")
	rows, err := st.ListCommandTranslations(context.Background(), "ou_1", 10)
	if err != nil {
		t.Fatalf("ListCommandTranslations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(rows))
	}
	if rows[0].RawText != "什么鬼" || rows[0].ContextDigest == "" {
		t.Errorf("审计行 = %+v,原文与 context_digest 都必须留痕", rows[0])
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/feishucmd/ -run Translate`
Expected: 编译失败 —— `undefined: Translator` / `undefined: render`

- [ ] **Step 3: 扩展 Store 接口**

编辑 `runtime/internal/feishucmd/executor.go`，给 `Store` 接口加三个方法：

```go
type Store interface {
	FleetOverview(ctx context.Context) (*store.FleetOverview, error)
	UnquarantineDevice(ctx context.Context, deviceID string) (bool, error)
	ListArtifacts(ctx context.Context, commitSHA string, pipelineID int) ([]store.Artifact, error)
	NextWorkflowAttempt(ctx context.Context, commitSHA string, pipelineID int, variant string) (int, error)
	NextWorkflowAttemptAll(ctx context.Context, commitSHA string, pipelineID int) (int, error)
	// 以下三个供意图翻译层使用(设计文档 §3.1)
	RecentRuns(ctx context.Context, limit int) ([]store.RecentRun, error)
	SaveCommandTranslation(ctx context.Context, row store.CommandTranslation) error
	ListCommandTranslations(ctx context.Context, openID string, limit int) ([]store.CommandTranslation, error)
}
```

- [ ] **Step 4: 实现 Translator**

创建 `runtime/internal/feishucmd/translate.go`：

```go
package feishucmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/store"
)

// minConfidence 是执行门限:低于此值一律不执行,只回翻译结果供人工判断。
const minConfidence = 0.75

// recentRunsLimit 是上下文快照携带的历史运行条数上限(设计文档 §4.2)。
const recentRunsLimit = 10

// sideEffect 标记需要二次确认的指令:LLM 猜错参数的代价是白跑一轮设备测试。
var sideEffect = map[string]bool{"rerun": true, "unquarantine": true}

// TranslateResult 是一次翻译的完整结论。OK=false 时 Reply 即最终回复文本。
type TranslateResult struct {
	Cmd          Command
	Rendered     string // 渲染出的那行指令(可直接展示给用户照打)
	Reason       string // LLM 给出的依据
	NeedsConfirm bool
	Outcome      string
	Reply        string
	OK           bool
}

// Translator 把自然语言翻译成一行既有指令文本(设计文档 §3.1)。
// 它不执行指令,只产出"可执行的 Command"或"不可执行 + 原因"。
type Translator struct {
	Client   hermesclient.Translator
	Store    Store
	Variants []string // 合法变体名单(来自 specCfg)
	Model    string
	Now      func() time.Time // 可注入,便于测试
}

func (t *Translator) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now().UTC()
}

// render 把翻译结果拼成一行指令文本。args 各项已由 command.schema.json 保证不含
// 空白字符,故 strings.Fields 可无损切回(方案 1 封闭性的实现基础)。
func render(cmd string, args []string) string {
	if len(args) == 0 {
		return cmd
	}
	return cmd + " " + strings.Join(args, " ")
}

// snapshot 是发给平台的只读上下文(设计文档 §4.2)。
type snapshot struct {
	Now        string          `json:"now"`
	Variants   []string        `json:"variants"`
	RecentRuns []snapshotRun   `json:"recent_runs"`
	Devices    []snapshotDev   `json:"devices"`
}

type snapshotRun struct {
	Commit      string `json:"commit"`
	PipelineIID int    `json:"pipeline_iid"`
	Variant     string `json:"variant"`
	Verdict     string `json:"verdict,omitempty"`
	EndedAt     string `json:"ended_at,omitempty"`
}

type snapshotDev struct {
	DeviceID string `json:"device_id"`
	Serial   string `json:"serial"`
	Status   string `json:"status"`
}

// buildSnapshot 组装上下文快照。查库失败时降级为只含 now 的空快照——
// 快照缺失只会让 LLM 返回 none,是安全降级,不该阻断(设计文档 §6)。
func (t *Translator) buildSnapshot(ctx context.Context) snapshot {
	snap := snapshot{
		Now:        t.now().UTC().Format(time.RFC3339),
		Variants:   t.Variants,
		RecentRuns: []snapshotRun{},
		Devices:    []snapshotDev{},
	}
	if snap.Variants == nil {
		snap.Variants = []string{}
	}
	if runs, err := t.Store.RecentRuns(ctx, recentRunsLimit); err == nil {
		for _, r := range runs {
			sr := snapshotRun{Commit: r.Commit, PipelineIID: r.PipelineID, Variant: r.Variant, Verdict: r.Verdict}
			if !r.EndedAt.IsZero() {
				sr.EndedAt = r.EndedAt.UTC().Format(time.RFC3339)
			}
			snap.RecentRuns = append(snap.RecentRuns, sr)
		}
	}
	if ov, err := t.Store.FleetOverview(ctx); err == nil {
		for _, d := range ov.Devices {
			snap.Devices = append(snap.Devices, snapshotDev{
				DeviceID: d.DeviceID, Serial: d.Serial, Status: d.Status,
			})
		}
	}
	return snap
}

// Translate 执行一次翻译。无论成败都落一行审计(设计文档 §4.3)。
func (t *Translator) Translate(ctx context.Context, openID, rawText string) TranslateResult {
	snap := t.buildSnapshot(ctx)
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		snapJSON = []byte(`{}`)
	}
	sum := sha256.Sum256(snapJSON)
	digest := hex.EncodeToString(sum[:])

	audit := store.CommandTranslation{
		OpenID: openID, RawText: rawText,
		PromptVersion: hermesclient.PromptVersionTranslate,
		Model:         t.Model, ContextDigest: digest,
	}

	tr, err := t.Client.Translate(ctx, hermesclient.TranslateRequest{
		RawText: rawText, Context: snapJSON, Model: t.Model,
	})
	if err != nil {
		audit.Outcome = store.OutcomeTranslatorError
		audit.Output = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
		t.save(ctx, audit)
		return TranslateResult{Outcome: audit.Outcome,
			Reply: "翻译服务暂时不可用。\n" + usage}
	}
	if raw, mErr := json.Marshal(tr); mErr == nil {
		audit.Output = raw
	}

	if tr.Command == "none" {
		audit.Outcome = store.OutcomeRejectedNone
		t.save(ctx, audit)
		reply := "没理解这句话。"
		if tr.Reason != "" {
			reply += "(" + tr.Reason + ")"
		}
		return TranslateResult{Outcome: audit.Outcome, Reason: tr.Reason, Reply: reply + "\n" + usage}
	}

	rendered := render(tr.Command, tr.Args)
	audit.Rendered = rendered
	cmd := Parse(rendered)
	if cmd.Name == "help" {
		// 渲染后回灌未命中四指令:值域被破坏,拒绝
		audit.Outcome = store.OutcomeRejectedArgs
		t.save(ctx, audit)
		return TranslateResult{Outcome: audit.Outcome, Rendered: rendered, Reply: "没理解这句话。\n" + usage}
	}
	if why := t.checkArgs(cmd); why != "" {
		audit.Outcome = store.OutcomeRejectedArgs
		t.save(ctx, audit)
		return TranslateResult{Outcome: audit.Outcome, Rendered: rendered,
			Reply: fmt.Sprintf("你是想说 `%s` 吗?该指令参数不合法: %s\n%s", rendered, why, usage)}
	}
	if tr.Confidence < minConfidence {
		audit.Outcome = store.OutcomeRejectedLowConfidence
		t.save(ctx, audit)
		return TranslateResult{Outcome: audit.Outcome, Rendered: rendered,
			Reply: fmt.Sprintf("不太确定,你是想说 `%s` 吗?确认请直接发这行。", rendered)}
	}

	res := TranslateResult{
		Cmd: cmd, Rendered: rendered, Reason: tr.Reason, OK: true,
		NeedsConfirm: sideEffect[cmd.Name],
	}
	if res.NeedsConfirm {
		res.Outcome = store.OutcomePendingConfirm
	} else {
		res.Outcome = store.OutcomeExecuted
	}
	audit.Outcome = res.Outcome
	t.save(ctx, audit)
	return res
}

// checkArgs 复用既有参数校验(设计文档 §5.3);返回空串表示通过。
// 变体/设备存在性按快照成员判定;execute 内部仍会独立查库,两层都保留。
func (t *Translator) checkArgs(cmd Command) string {
	switch cmd.Name {
	case "rerun":
		if len(cmd.Args) < 2 || len(cmd.Args) > 3 {
			return "rerun 需要 <sha> <pipeline_iid> [variant]"
		}
		if err := validateSHA(strings.ToLower(cmd.Args[0])); err != nil {
			return err.Error()
		}
		if iid, err := strconv.Atoi(cmd.Args[1]); err != nil || iid <= 0 {
			return fmt.Sprintf("pipeline_iid %q 不是正整数", cmd.Args[1])
		}
		if len(cmd.Args) == 3 && !contains(t.Variants, cmd.Args[2]) {
			return fmt.Sprintf("变体 %s 不在已知变体名单内", cmd.Args[2])
		}
	case "unquarantine":
		if len(cmd.Args) > 1 {
			return "unquarantine 最多一个 device_id"
		}
	case "status", "devices":
		if len(cmd.Args) != 0 {
			return cmd.Name + " 不接受参数"
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// save 落审计;失败只记日志不阻断(与 persistEvidenceSnapshot 的降级一致)。
func (t *Translator) save(ctx context.Context, row store.CommandTranslation) {
	if t.Store == nil {
		return
	}
	_ = t.Store.SaveCommandTranslation(ctx, row)
}
```

- [ ] **Step 5: 跑测试**

Run: `cd runtime && go test ./internal/feishucmd/ -v`
Expected: 全部 PASS（含既有 executor 用例）

- [ ] **Step 6: Commit**

```bash
git add runtime/internal/feishucmd/
git commit -m "feat(runtime): add intent translator rendering into existing command text"
```

---

### Task 7: `Executor` 接入与待确认态

**Files:**
- Modify: `runtime/internal/feishucmd/executor.go`
- Modify: `runtime/internal/feishucmd/executor_test.go`

**Interfaces:**
- Consumes: Task 6 的 `Translator` / `TranslateResult`
- Produces: `Executor.Translator *Translator`、`Executor.Now func() time.Time` 两个新字段；`HandleMessage` 的新流程。被 Task 8 的 worker 装配消费

- [ ] **Step 1: 写失败的测试**

在 `runtime/internal/feishucmd/executor_test.go` 追加。文件里既有
`type fakeSender struct{ texts []string }`（`executor_test.go:69`，累积全部回复），
先加一个取最后一条的辅助，再加用例：

```go
// lastText 返回最后一条回复;无回复时返回空串。
func lastText(s *fakeSender) string {
	if len(s.texts) == 0 {
		return ""
	}
	return s.texts[len(s.texts)-1]
}

func TestHandleMessageTranslatesUnknownInput(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "devices", Confidence: 0.95,
	}}
	st := store.NewMemStore()
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "看下设备都什么状态")
	if f.calls != 1 {
		t.Fatalf("translator calls = %d, want 1", f.calls)
	}
	if !strings.Contains(lastText(sender), "已理解为: devices") {
		t.Errorf("回复应告知理解结果,便于用户下次直接打: %q", lastText(sender))
	}
}

func TestHandleMessageKnownCommandSkipsTranslator(t *testing.T) {
	f := &fakeTranslator{}
	st := store.NewMemStore()
	e := &Executor{Store: st, Sender: &fakeSender{}, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "devices")
	if f.calls != 0 {
		t.Errorf("已能解析的指令不得走翻译层, calls = %d", f.calls)
	}
}

func TestHandleMessageNonWhitelistNeverCallsTranslator(t *testing.T) {
	f := &fakeTranslator{}
	st := store.NewMemStore()
	e := &Executor{Store: st, Sender: &fakeSender{}, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_evil", "帮我重跑一下")
	if f.calls != 0 {
		t.Errorf("非白名单必须零 LLM 调用, calls = %d", f.calls)
	}
}

func TestConfirmFlowExecutesOnYes(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-1"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1") // 见既有测试里的设备准备辅助函数
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "把那台板子放出来")
	if !strings.Contains(lastText(sender), "将执行") {
		t.Fatalf("应先回执待确认: %q", lastText(sender))
	}
	e.HandleMessage(context.Background(), "ou_1", "y")
	if !strings.Contains(lastText(sender), "已解隔离") {
		t.Errorf("确认后应执行: %q", lastText(sender))
	}
}

func TestConfirmFlowCancelsOnNoWithoutTranslating(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-1"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1")
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "把那台板子放出来")
	before := f.calls
	e.HandleMessage(context.Background(), "ou_1", "n")
	if f.calls != before {
		t.Errorf("n 必须短路,不得再触发翻译: calls %d → %d", before, f.calls)
	}
	if !strings.Contains(lastText(sender), "已取消") {
		t.Errorf("reply = %q", lastText(sender))
	}
}

func TestConfirmFlowFallsThroughOnOtherInput(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-1"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1")
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "把那台板子放出来")
	e.HandleMessage(context.Background(), "ou_1", "devices")
	if !strings.Contains(lastText(sender), "已取消上一条待确认") {
		t.Errorf("改口时应提示待确认已取消: %q", lastText(sender))
	}
	if !strings.Contains(lastText(sender), "dev-1") && !strings.Contains(lastText(sender), "serial") {
		t.Errorf("devices 应被执行: %q", lastText(sender))
	}
}

func TestConfirmExpires(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-1"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1")
	sender := &fakeSender{}
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st), Now: func() time.Time { return now }}
	e.HandleMessage(context.Background(), "ou_1", "把那台板子放出来")
	now = now.Add(121 * time.Second)
	e.HandleMessage(context.Background(), "ou_1", "y")
	if strings.Contains(lastText(sender), "已解隔离") {
		t.Error("TTL 过期后 y 不得执行")
	}
}

func TestNewTranslationSupersedesPending(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-1"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1")
	e := &Executor{Store: st, Sender: &fakeSender{}, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "把那台板子放出来")
	e.HandleMessage(context.Background(), "ou_1", "再放一次")
	rows, err := st.ListCommandTranslations(context.Background(), "ou_1", 10)
	if err != nil {
		t.Fatalf("ListCommandTranslations: %v", err)
	}
	var expired int
	for _, r := range rows {
		if r.Outcome == store.OutcomeExpired {
			expired++
		}
	}
	if expired != 1 {
		t.Errorf("被覆盖的待确认应落一行 expired, got %d", expired)
	}
}

func TestTranslatorDisabledFallsBackToUsage(t *testing.T) {
	st := store.NewMemStore()
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true}}
	e.HandleMessage(context.Background(), "ou_1", "随便说点什么")
	if lastText(sender) != usage {
		t.Errorf("翻译层禁用时必须与改动前逐字节一致:\n got %q\nwant %q", lastText(sender), usage)
	}
}
```

`seedQuarantinedDevice(t, st, "dev-1")` 是本任务要新加的辅助：用既有 `TestUnquarantine`
（`executor_test.go:209`）里准备设备的同一套 store 调用，造一台 `QUARANTINED` 设备。
照抄那段准备代码抽成函数即可，不要发明新的设备登记路径。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/feishucmd/ -run 'HandleMessage|Confirm|Supersede|Disabled'`
Expected: 编译失败 —— `Executor` 无 `Translator` / `Now` 字段

- [ ] **Step 3: 给 Executor 加字段**

编辑 `runtime/internal/feishucmd/executor.go`，给 `Executor` 结构体追加：

```go
	// Translator 非 nil 时启用自然语言翻译旁路(设计文档 §3.1);
	// nil = 未启用,未知输入回 usage(改动前的行为)。
	Translator *Translator
	// Now 可注入,便于测试待确认 TTL;nil 用 time.Now().UTC()。
	Now func() time.Time

	pendingMu sync.Mutex
	pending   map[string]pendingCmd
```

并在同文件加类型与常量：

```go
// confirmTTL 是待确认态存活时长(设计文档 §5.2)。过期后回 y 视同未理解。
const confirmTTL = 120 * time.Second

// pendingCmd 是一条等待用户确认的副作用指令。存内存而非落库:worker 重启丢失
// 待确认项,代价只是用户重说一遍,绝不会误执行一个跨重启的陈旧 rerun。
type pendingCmd struct {
	cmd      Command
	rendered string
	expires  time.Time
}

func (e *Executor) nowFn() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}
```

import 补 `"sync"`、`"time"`。

- [ ] **Step 4: 改 HandleMessage**

把 `HandleMessage` 替换为：

```go
// HandleMessage 处理一条单聊文本消息。安全红线:非白名单 open_id 静默忽略
// (不回复,防探测;翻译层永远看不到非白名单消息),记 info 日志。
func (e *Executor) HandleMessage(ctx context.Context, openID, text string) {
	log := e.log()
	if !e.Whitelist[openID] {
		log.Info().Str("open_id", openID).Msg("feishu cmd from non-whitelist sender, ignored")
		return
	}
	trimmed := strings.TrimSpace(text)
	prefix := "" // 待确认被取消时,前置到最终回复里

	// 待确认态检查必须在 Parse 之前:否则用户打的 y 会被当成未知指令(设计文档 §5.2)
	if pend, ok := e.takePending(openID); ok {
		switch strings.ToLower(trimmed) {
		case "y", "yes":
			e.audit(ctx, openID, trimmed, pend.rendered, store.OutcomeConfirmed)
			reply, err := e.execute(ctx, pend.cmd)
			if err != nil {
				log.Error().Err(err).Str("cmd", pend.cmd.Name).Msg("feishu cmd failed")
				reply = fmt.Sprintf("指令执行失败: %v", err)
			}
			e.reply(ctx, reply)
			return
		case "n", "no":
			// 与 y/yes 对称:确认问句给两个答案,不是一个答案加一堆非答案。
			// 顺带省掉一次必然落 rejected_none 的 LLM 调用。
			e.audit(ctx, openID, trimmed, pend.rendered, store.OutcomeDeclined)
			e.reply(ctx, "已取消: "+pend.rendered)
			return
		default:
			e.audit(ctx, openID, trimmed, pend.rendered, store.OutcomeDeclined)
			prefix = "已取消上一条待确认(" + pend.rendered + ")\n"
		}
	}

	cmd := Parse(trimmed)
	if cmd.Name == "help" && e.Translator != nil {
		e.reply(ctx, prefix+e.handleTranslated(ctx, openID, trimmed))
		return
	}
	reply, err := e.execute(ctx, cmd)
	if err != nil {
		log.Error().Err(err).Str("cmd", cmd.Name).Msg("feishu cmd failed")
		reply = fmt.Sprintf("指令执行失败: %v", err)
	}
	log.Info().Str("open_id", openID).Str("cmd", cmd.Name).Msg("feishu cmd executed")
	e.reply(ctx, prefix+reply)
}

// handleTranslated 走翻译旁路并返回回复文本(不含 prefix)。
func (e *Executor) handleTranslated(ctx context.Context, openID, text string) string {
	log := e.log()
	res := e.Translator.Translate(ctx, openID, text)
	log.Info().Str("open_id", openID).Str("outcome", res.Outcome).
		Str("rendered", res.Rendered).Msg("feishu cmd translated")
	if !res.OK {
		return res.Reply
	}
	if res.NeedsConfirm {
		e.putPending(ctx, openID, pendingCmd{
			cmd: res.Cmd, rendered: res.Rendered,
			expires: e.nowFn().Add(confirmTTL),
		})
		msg := fmt.Sprintf("将执行: %s", res.Rendered)
		if res.Reason != "" {
			msg += "\n(依据: " + res.Reason + ")"
		}
		return msg + fmt.Sprintf("\n回复 y 确认,n 取消,%d 秒后自动失效", int(confirmTTL.Seconds()))
	}
	reply, err := e.execute(ctx, res.Cmd)
	if err != nil {
		log.Error().Err(err).Str("cmd", res.Cmd.Name).Msg("feishu cmd failed")
		return fmt.Sprintf("指令执行失败: %v", err)
	}
	// 带上"已理解为 X":用户下次可以直接打 X,翻译层因此是自我消解的
	return fmt.Sprintf("(已理解为: %s)\n%s", res.Rendered, reply)
}

// takePending 取出并清空某用户的待确认项;不存在或已过期返回 (_, false),
// 过期项落一行 expired 审计。
func (e *Executor) takePending(openID string) (pendingCmd, bool) {
	e.pendingMu.Lock()
	defer e.pendingMu.Unlock()
	pend, ok := e.pending[openID]
	if !ok {
		return pendingCmd{}, false
	}
	delete(e.pending, openID)
	if e.nowFn().After(pend.expires) {
		return pendingCmd{}, false
	}
	return pend, true
}

// putPending 放入待确认项(单槽:覆盖旧项,被覆盖的落一行 expired 审计)。
func (e *Executor) putPending(ctx context.Context, openID string, pend pendingCmd) {
	e.pendingMu.Lock()
	if e.pending == nil {
		e.pending = map[string]pendingCmd{}
	}
	old, had := e.pending[openID]
	e.pending[openID] = pend
	e.pendingMu.Unlock()
	if had {
		e.audit(ctx, openID, "", old.rendered, store.OutcomeExpired)
	}
}

// audit 追加一行翻译审计(确认/取消/过期这些非翻译事件也留痕,设计文档 §4.3)。
func (e *Executor) audit(ctx context.Context, openID, rawText, rendered, outcome string) {
	if e.Store == nil {
		return
	}
	_ = e.Store.SaveCommandTranslation(ctx, store.CommandTranslation{
		OpenID: openID, RawText: rawText, Rendered: rendered, Outcome: outcome,
	})
}

// reply 发送回复;Sender 为 nil 时只执行不回复(测试)。
func (e *Executor) reply(ctx context.Context, text string) {
	if e.Sender == nil {
		return
	}
	if err := e.Sender.SendText(ctx, text); err != nil {
		e.log().Error().Err(err).Msg("feishu cmd reply failed")
	}
}
```

`execute` 与 `Parse` 一行未改：翻译旁路只在 `cmd.Name == "help" && e.Translator != nil`
时接管，其余路径与改动前完全相同。

- [ ] **Step 5: 跑测试**

Run: `cd runtime && go test ./internal/feishucmd/ -v`
Expected: 全部 PASS

- [ ] **Step 6: 跑全量回归**

Run: `cd runtime && go test ./...`
Expected: 全部 PASS

- [ ] **Step 7: Commit**

```bash
git add runtime/internal/feishucmd/
git commit -m "feat(runtime): wire intent translation into the feishu command executor"
```

---

### Task 8: worker 装配、配置与文档

**Files:**
- Modify: `runtime/cmd/worker/config.go`
- Modify: `runtime/cmd/worker/config_test.go`
- Modify: `runtime/cmd/worker/main.go`
- Modify: `deploy/.env.example`
- Modify: `deploy/README.md`
- Modify: `runtime/README.md`

**Interfaces:**
- Consumes: Task 3 的 `hermesclient.HTTPClient`、Task 6/7 的 `Translator`/`Executor`
- Produces: 可运行的完整链路（无新导出符号）

- [ ] **Step 1: 写失败的配置测试**

在 `runtime/cmd/worker/config_test.go` 追加（沿用既有的 env 设置方式）：

```go
func TestFeishuCmdNLConfig(t *testing.T) {
	cfg, err := loadConfig(lookup(map[string]string{
		"VARIANTS_CONFIG":           "../../ci/variants.yaml",
		"FEISHU_CMD_NL":             "true",
		"FEISHU_CMD_NL_TIMEOUT_SEC": "90",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.Activity.FeishuCmdNL {
		t.Error("FeishuCmdNL = false, want true")
	}
	if cfg.Activity.FeishuCmdNLTimeout != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", cfg.Activity.FeishuCmdNLTimeout)
	}
}

func TestFeishuCmdNLDefaults(t *testing.T) {
	cfg, err := loadConfig(lookup(map[string]string{
		"VARIANTS_CONFIG": "../../ci/variants.yaml",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Activity.FeishuCmdNL {
		t.Error("缺省必须关闭(灰度)")
	}
	if cfg.Activity.FeishuCmdNLTimeout != 60*time.Second {
		t.Errorf("timeout 缺省 = %v, want 60s", cfg.Activity.FeishuCmdNLTimeout)
	}
}
```

配置加载函数是 `loadConfig(getenv func(string) string) (Config, error)`（`config.go:25`），
测试用同文件既有的 `lookup(map[string]string)` 辅助注入环境变量。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./cmd/worker/ -run FeishuCmdNL`
Expected: 编译失败 —— 无 `FeishuCmdNL` 字段

- [ ] **Step 3: 加配置项**

编辑 `runtime/cmd/worker/config.go`。在 `FeishuCmdWhitelist` 附近加字段与读取：

```go
			FeishuCmdWhitelist: getenv("FEISHU_CMD_WHITELIST"),
			// §12 Phase 2:自然语言翻译旁路总开关(缺省关,灰度)。
			FeishuCmdNL:        getenv("FEISHU_CMD_NL") == "true",
			FeishuCmdNLTimeout: time.Duration(nlTimeoutSec) * time.Second,
```

在既有 `envInt` 调用附近加：

```go
	// 翻译超时不复用 HERMES_TIMEOUT_SEC:bridge 实测 -t "" 冷/热约 76s/13s,
	// 分析用 60s 起步,交互用需要单独调(设计文档 §6)。
	nlTimeoutSec, err := envInt("FEISHU_CMD_NL_TIMEOUT_SEC", 60)
	if err != nil {
		return nil, err
	}
```

在 Activity 配置结构体加对应字段：

```go
	FeishuCmdNL        bool
	FeishuCmdNLTimeout time.Duration
```

- [ ] **Step 4: 装配 Translator**

编辑 `runtime/cmd/worker/main.go`，把 feishucmd listener 装配块里构造 `exec` 的部分改为：

```go
			exec := &feishucmd.Executor{
				Store: st, Sender: feishuSender, Log: &log, Whitelist: wl,
				Starter:          &trigger.TemporalStarter{Client: tc, TaskQueue: cfg.TemporalTaskQueue},
				ExpectedVariants: specCfg.VariantCount(),
			}
			// 自然语言翻译旁路(设计文档 §3.1):三个条件合取才启用——
			// 开关打开、bridge 端点已配、指令 listener 本身已启用。
			nlReason := ""
			switch {
			case !cfg.Activity.FeishuCmdNL:
				nlReason = "FEISHU_CMD_NL != true"
			case cfg.Activity.HermesEndpoint == "":
				nlReason = "HERMES_ENDPOINT empty"
			}
			if nlReason == "" {
				nlClient := hermesclient.NewHTTPClient(hermesclient.Config{
					Endpoint:  cfg.Activity.HermesEndpoint,
					AuthToken: cfg.Activity.HermesAuthToken,
					Timeout:   cfg.Activity.FeishuCmdNLTimeout,
				})
				exec.Translator = &feishucmd.Translator{
					Client:   nlClient,
					Store:    st,
					Variants: specCfg.VariantNames(),
					Model:    cfg.Activity.HermesModel,
				}
				log.Info().Dur("timeout", cfg.Activity.FeishuCmdNLTimeout).Msg("feishu cmd nl=enabled")
			} else {
				log.Info().Str("reason", nlReason).Msg("feishu cmd nl=disabled")
			}
```

若 `specCfg` 没有 `VariantNames()` 方法，在 `runtime/internal/activity/specs.go` 加一个（照 `VariantCount()` 的写法，返回全部变体名的切片，顺序稳定）：

```go
// VariantNames 返回全部已声明变体名(排序后顺序稳定),供翻译层的上下文快照与
// 变体存在性校验使用。
func (c *SpecConfig) VariantNames() []string {
	if c == nil {
		return []string{}
	}
	out := make([]string, 0, len(c.file.Variants))
	for name := range c.file.Variants {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
```

`c.file.Variants` 是 `map[string]variantDecl`（`specs.go:41`，`VariantCount()` 取的就是它
的 `len`），所以变体名是 map 的键；必须排序，否则快照内容随 map 遍历序抖动，
`context_digest` 失去可比性。import 补 `"sort"`。

- [ ] **Step 5: 跑测试**

Run: `cd runtime && go test ./... && go build ./...`
Expected: 全部 PASS，编译通过

- [ ] **Step 6: 更新部署文档**

在 `deploy/.env.example` 的飞书小节追加：

```bash
# 飞书指令层自然语言翻译(设计文档 2026-07-28)。缺省关闭;打开前必须先确认
# analyze_bridge 已部署 /translate 路由(它不在 compose 里)。
FEISHU_CMD_NL=false
FEISHU_CMD_NL_TIMEOUT_SEC=60
```

在 `deploy/README.md` 的飞书指令小节补一段：

```markdown
### 飞书指令自然语言翻译(可选)

`FEISHU_CMD_NL=true` 后,不在 `status|devices|rerun|unquarantine` 里的输入会经
hermes-agent 翻译成一条指令再执行。启用前置条件(三者合取):

1. `FEISHU_CMD_WHITELIST` 非空(指令 listener 本身已启用)
2. `HERMES_ENDPOINT` 非空
3. **analyze_bridge 已部署 `/translate` 路由** —— 它不在本 compose 内,由
   hermes-agent 实例内的 `start-analyze-bridge` 启停。先
   `curl -X POST -H "Authorization: Bearer $HERMES_AUTH_TOKEN" .../translate`
   确认路由存在,否则全部自然语言请求 502(手打指令不受影响)。

行为要点:只读指令(status/devices)直接执行;副作用指令(rerun/unquarantine)
先回执待确认,回复 `y` 执行、`n` 取消,120 秒过期。每次翻译在
`command_translations` 表留痕。
```

在 `runtime/README.md` 的环境变量表补 `FEISHU_CMD_NL`、`FEISHU_CMD_NL_TIMEOUT_SEC` 两行。

- [ ] **Step 7: Commit**

```bash
git add runtime/ deploy/
git commit -m "feat(runtime): wire NL translation into worker startup and config"
```

---

## 完成后的手工验收

按设计文档 §8.6 执行。前置：analyzer 容器已拉新代码并重启，`curl` 确认 `/translate` 存在。

1. "看下设备状态" → 返回 devices 列表，回复含"已理解为: devices"
2. "帮我重跑昨天 SNPE 1.68 那个失败的" → 回执待确认 → `y` → workflow 启动，ID 含 `-r{N}`
3. 同上但回 `n` → 回"已取消"，`command_translations` 有 `pending_confirm` + `declined` 两行，bridge 侧无新增调用日志
4. 同上但改口打 `devices` → 回复以"已取消上一条待确认"开头，且 `devices` 被执行
5. "今天天气怎么样" → 回 usage，落 `rejected_none`
6. 停掉 bridge → 自然语言回"翻译服务暂时不可用" + usage，四个手打指令照常工作
