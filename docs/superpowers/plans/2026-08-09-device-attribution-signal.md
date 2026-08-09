# 设备级归因信号源 实施计划(差距 #10)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `device_fail_streak` 第一次拥有真实信号,使「设备连续 3 次故障 → QUARANTINED」从死代码变成生效的安全网。

**Architecture:** Agent 是唯一知道"失败发生在哪一层"的一方,由它在 `result.json` 上报 `failure_scope`/`failure_stage`;Runtime 只采信终态、且 verdict 为 PASSED 时强制忽略。隔离在同一事务内写入既有 outbox,由 Relay 投递飞书通知(at-least-once)。全程不引入 rules v2——新字段只影响归因(scope),不影响判定(verdict)。

**Tech Stack:** Go 1.26.5(两个独立 module:`runtime/`、`agent/`)、PostgreSQL、Temporal、JSON Schema(santhosh-tekuri/jsonschema v5, Draft2020)、Docker Compose。

设计依据:`docs/superpowers/specs/2026-08-09-device-attribution-signal-design.md`(下称 spec)。

## Global Constraints

- **Go 版本**:`runtime/go.mod` 声明 `go 1.26.5`、`agent/go.mod` 声明 `1.25.0`。本机 Go 在 `/home/maxin/.local/go/bin`,已入 `~/.bashrc`。
- **两个 module 独立**:`cd runtime && go test ./...` 与 `cd agent && go test ./...` 分别执行,不能在仓库根跑。
- **质量门禁**:每个任务提交前跑 `bash scripts/lint.sh`(gofmt + go vet + go test,覆盖两个 module),必须 `ALL LINT CHECKS PASSED`。
- **红线 §14**:不得新增任意 shell 接口;所有 ADB 命令必须是 `internal/adb` 里的模板化白名单构造器,且一律带 `-s <serial>`(`Devices()` 是唯一例外)。
- **契约变更规则 §13**:只加字段不删字段;`contracts/` 下 schema 改动必须同步所有 `go:embed` 副本,三处均有 `TestEmbeddedSchemaMatchesContract` 会挡住不同步。
- **归因铁律**:归因只能来自明确信号,不能来自沉默。`siteDispatchFailed` / `siteLeaseExpired` / `siteHardDeadline` 等站点**永不产出 `device`**。
- **保守默认**:任何无法可靠区分的情形一律归 `none`。宁可漏计一次,也不误隔离。
- **提交信息用英文**,注释中英皆可(§13)。

---

### Task 1: adb 错误种类区分(证据保真 #1)

`ExecRunner.Run` 目前把"adb 二进制没跑起来"和其它错误压成同一个 `fmt.Errorf`,调用方无从分辨。这是 spec §5.4 第 1 处。

**Files:**
- Modify: `agent/internal/adb/adb.go`(`ExecRunner.Run`,约 223-244 行)
- Test: `agent/internal/adb/adb_test.go`

**Interfaces:**
- Produces: `adb.LaunchError`(带 `Unwrap`),供 Task 6 判定 `client`

- [ ] **Step 1: 写失败测试**

```go
// agent/internal/adb/adb_test.go
func TestRunWrapsLaunchFailureAsLaunchError(t *testing.T) {
	r := &ExecRunner{ADBPath: "/nonexistent/adb-binary-xyz"}
	_, err := r.Run(context.Background(), Devices())
	if err == nil {
		t.Fatal("adb 二进制不存在时应返回 error")
	}
	var le *LaunchError
	if !errors.As(err, &le) {
		t.Fatalf("应为 *LaunchError(供调用方归因 client),got %T: %v", err, err)
	}
}

// 非零退出码不是 error,更不能是 LaunchError:它是"设备/远端命令的客观结果"
func TestRunNonZeroExitIsNotLaunchError(t *testing.T) {
	r := &ExecRunner{ADBPath: "/bin/false"}
	res, err := r.Run(context.Background(), Devices())
	if err != nil {
		t.Fatalf("非零退出不应作为 error: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("期望非零退出码")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd agent && go test ./internal/adb/ -run 'LaunchError' -v`
Expected: FAIL,`undefined: LaunchError`

- [ ] **Step 3: 实现**

```go
// agent/internal/adb/adb.go

// LaunchError 表示 adb 二进制本身没能执行(缺失、不可执行、权限不足)。
// 归因意义:这是 Client 侧故障,与任何具体设备无关(spec §5.1)。
//
// 注意它**不**覆盖"私有 adb server 起不来":那种情况 adb 进程正常启动、
// 以非零退出码结束,走 ExitError 分支返回 (res, nil),连 error 都不是,
// 只能由 spec §5.3 的两级复核区分。
type LaunchError struct {
	Args []string
	Err  error
}

func (e *LaunchError) Error() string {
	return fmt.Sprintf("adb %s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *LaunchError) Unwrap() error { return e.Err }
```

把 `Run` 的 `default` 分支改为:

```go
	default:
		return res, &LaunchError{Args: args, Err: err}
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd agent && go test ./internal/adb/ -v`
Expected: PASS(含既有用例)

- [ ] **Step 5: 提交**

```bash
git add agent/internal/adb/adb.go agent/internal/adb/adb_test.go
git commit -m "feat(agent): distinguish adb launch failures from device errors

Run collapsed every non-ExitError into one fmt.Errorf, so callers could
not tell a missing adb binary from a device fault. Typed as LaunchError
so attribution can charge it to the client."
```

---

### Task 2: 存活复核所需的白名单能力

spec §5.3 的两级复核需要两样目前没有的东西。`ParseTransports` 只保留 `fields[1] == "device"` 的行,offline/unauthorized 会被丢掉——而那正是二级判定的核心证据。

**Files:**
- Modify: `agent/internal/adb/adb.go`
- Test: `agent/internal/adb/adb_test.go`

**Interfaces:**
- Produces: `adb.GetState(serial string) []string`、`adb.ParseDeviceStates(out string) map[string]string`,供 Task 6 使用

- [ ] **Step 1: 写失败测试**

```go
func TestGetStateIsSerialScoped(t *testing.T) {
	got := GetState("dev1")
	want := []string{"-s", "dev1", "get-state"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetState = %v, want %v(红线 §14:必须带 -s)", got, want)
	}
}

// 二级复核要靠 offline/unauthorized 判定设备不可达,不能像 ParseTransports 那样丢弃
func TestParseDeviceStatesRetainsNonDeviceStates(t *testing.T) {
	out := "List of devices attached\n" +
		"dev1\tdevice\n" +
		"dev2\toffline\n" +
		"dev3\tunauthorized\n" +
		"\n"
	got := ParseDeviceStates(out)
	want := map[string]string{"dev1": "device", "dev2": "offline", "dev3": "unauthorized"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseDeviceStates = %v, want %v", got, want)
	}
	// 对照:ParseTransports 会把后两台丢掉,故不可复用
	if len(ParseTransports(out)) != 1 {
		t.Fatal("前提失效:ParseTransports 不再只保留 device 行,请重新评估 §5.3 设计")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd agent && go test ./internal/adb/ -run 'GetState|ParseDeviceStates' -v`
Expected: FAIL,`undefined: GetState`

- [ ] **Step 3: 实现**

```go
// GetState 查询单台设备的连接状态(存活复核一级,spec §5.3)。
// 输出为 "device" / "offline" / "unauthorized" 之一,或非零退出。
func GetState(serial string) []string { return withSerial(serial, "get-state") }

// ParseDeviceStates 解析 `adb devices -l`,返回 serial → state 全量映射。
//
// 与 ParseTransports 的区别:后者只保留 state == "device" 的行。
// 存活复核二级(spec §5.3)恰恰需要 offline / unauthorized —— 它们是
// "设备确实不可达"的证据,被丢掉就无法与"adb server 挂了"区分。
func ParseDeviceStates(out string) map[string]string {
	states := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "List" {
			continue
		}
		states[fields[0]] = fields[1]
	}
	return states
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd agent && go test ./internal/adb/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add agent/internal/adb/
git commit -m "feat(agent): add get-state probe and state-preserving device parser

Liveness re-check needs offline/unauthorized rows, which ParseTransports
drops by design. Both additions stay inside the templated whitelist."
```

---

### Task 3: SoC 探测链返回错误(证据保真 #3)

`ProbeAndroidSOCChain` 静默吞掉全部 getprop 错误(spec §5.4 第 3 处)。设备在 ABI 检查后掉线时返回空链,上层报 `soc mismatch` → 按 §3 归 `none`,**真设备故障被伪装成配置问题**。

**Files:**
- Modify: `agent/internal/adb/soc_probe.go`
- Modify: `agent/internal/executor/executor.go`(调用点)、`agent/internal/reporter/probe.go`(调用点)
- Test: `agent/internal/adb/soc_probe_test.go`

**Interfaces:**
- Produces: `adb.ProbeAndroidSOCChain(ctx, runner, serial) ([]string, error)` —— 签名变更,error 非 nil 表示探测过程中出现传输层失败

- [ ] **Step 1: 写失败测试**

```go
// agent/internal/adb/soc_probe_test.go
type errRunner struct{ err error }

func (r errRunner) Run(context.Context, []string) (Result, error) { return Result{}, r.err }

func TestProbeChainReportsTransportFailure(t *testing.T) {
	_, err := ProbeAndroidSOCChain(context.Background(),
		errRunner{err: &LaunchError{Args: []string{"getprop"}, Err: errors.New("boom")}}, "dev1")
	if err == nil {
		t.Fatal("全部 getprop 失败时必须报错,否则空链会被上层误读为 soc mismatch")
	}
}

type okRunner struct{ out string }

func (r okRunner) Run(context.Context, []string) (Result, error) {
	return Result{Stdout: r.out}, nil
}

func TestProbeChainNoErrorWhenPropsMerelyEmpty(t *testing.T) {
	chain, err := ProbeAndroidSOCChain(context.Background(), okRunner{out: ""}, "dev1")
	if err != nil {
		t.Fatalf("属性为空不是故障,不应报错: %v", err)
	}
	if len(chain) != 0 {
		t.Fatalf("chain = %v, want empty", chain)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd agent && go test ./internal/adb/ -run 'ProbeChain' -v`
Expected: FAIL,`assignment mismatch: 2 variables but ProbeAndroidSOCChain returns 1 value`

- [ ] **Step 3: 实现**

```go
func ProbeAndroidSOCChain(ctx context.Context, runner Runner, serial string) ([]string, error) {
	var out []string
	var lastErr error
	seen := map[string]bool{}
	for _, prop := range socProbeChain {
		soc, err := getPropQuiet(ctx, runner, serial, prop)
		if err != nil {
			lastErr = err // 保留传输层失败,不再静默丢弃(spec §5.4 第 3 处)
			continue
		}
		if soc == "" || !ValidSOC(soc) || seen[soc] {
			continue
		}
		seen[soc] = true
		out = append(out, soc)
	}
	// 一个都没探到、且过程中有传输层失败 → 设备很可能掉线了。
	// 报错让调用方走存活复核,而不是让空链被误读成 soc mismatch。
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}
```

`ProbeAndroidSOC` 同步适配:

```go
func ProbeAndroidSOC(ctx context.Context, runner Runner, serial string) string {
	chain, err := ProbeAndroidSOCChain(ctx, runner, serial)
	if err != nil || len(chain) == 0 {
		return ""
	}
	return chain[0]
}
```

调用点各加一处错误处理:`executor.go` 的 `precheckAndroid` 把 error 向上返回(Task 6 会用到);`reporter/probe.go` 保持尽力而为,`err != nil` 时按空链处理并记日志。

- [ ] **Step 4: 跑测试确认通过**

Run: `cd agent && go test ./... 2>&1 | grep -v "^ok\|no test files"`
Expected: 无输出(全绿,含既有 A1 别名用例)

- [ ] **Step 5: 提交**

```bash
git add agent/internal/adb/ agent/internal/executor/executor.go agent/internal/reporter/probe.go
git commit -m "fix(agent): stop swallowing getprop errors in the SoC probe chain

A device dropping after the ABI check produced an empty chain, which the
caller reported as a soc mismatch - a real device fault disguised as a
config problem, the A1 failure mode inverted."
```

---

### Task 4: resolveTransport 查退出码 + resolve 阶段归因(证据保真 #2)

`resolveTransport` 只判 `err != nil`,`ExitCode != 0` 时会拿着残缺 stdout 继续,最终落到 `device %q not found via adb` —— **server 级故障被伪装成设备不存在**(spec §5.4 第 2 处)。同时按 spec §5.3.1 给 resolve 阶段定归因。

**Files:**
- Modify: `agent/internal/executor/executor.go`(`resolveTransport`,265-296 行)
- Test: `agent/internal/executor/executor_test.go`

**Interfaces:**
- Produces: `resolveTransport` 失败时返回可被 Task 6 归因的错误;新增 `errAdbServer`(sentinel)标识 server 级故障

- [ ] **Step 1: 写失败测试**

```go
// adb devices 非零退出是 server/宿主机问题,不能伪装成"设备不存在"
func TestResolveTransportSurfacesServerFailure(t *testing.T) {
	f := &fakeADB{props: defaultProps(), devicesExit: 1, devicesStderr: "cannot connect to daemon"}
	e, _ := newExecutor(f)
	_, err := e.resolveTransport(context.Background(), serial)
	if err == nil {
		t.Fatal("adb devices 非零退出必须报错")
	}
	if !errors.Is(err, errAdbServer) {
		t.Fatalf("应标识为 server 级故障(归 client),got: %v", err)
	}
}

// 逻辑 serial 与 transport 允许不同(RK3568/USB gadget),
// 列表里没有同名项不等于设备不在(spec §5.3.1)
func TestResolveTransportNoMatchIsNotServerFailure(t *testing.T) {
	f := &fakeADB{props: defaultProps(), devicesList: "List of devices attached\nother1\tdevice\n"}
	e, _ := newExecutor(f)
	_, err := e.resolveTransport(context.Background(), serial)
	if err == nil {
		t.Fatal("无匹配应报错")
	}
	if errors.Is(err, errAdbServer) {
		t.Fatal("无匹配不是 server 故障;它必须归 none 而非 client/device")
	}
}
```

`fakeADB` 需要新增 `devicesExit` / `devicesStderr` 字段并在匹配 `devices` 命令时返回它们。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd agent && go test ./internal/executor/ -run ResolveTransport -v`
Expected: FAIL,`undefined: errAdbServer`

- [ ] **Step 3: 实现**

```go
// errAdbServer 标识 adb server / 宿主机级故障(非设备故障)。
// 归因为 client(spec §5.1);包裹后用 errors.Is 判别。
var errAdbServer = errors.New("adb server failure")

func (e *Executor) resolveTransport(ctx context.Context, logical string) (string, error) {
	res, err := e.Runner.Run(ctx, adb.Devices())
	if err != nil {
		return "", fmt.Errorf("resolve serial: adb devices: %w: %w", errAdbServer, err)
	}
	// 非零退出同样是 server 级故障:此前未检查,会让残缺 stdout 流向
	// "device not found",把 server 故障伪装成设备不存在(spec §5.4 第 2 处)
	if res.ExitCode != 0 {
		return "", fmt.Errorf("resolve serial: adb devices exit=%d: %s: %w",
			res.ExitCode, strings.TrimSpace(res.Stderr), errAdbServer)
	}
	// ... 以下快路径/慢路径逻辑不变 ...
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd agent && go test ./internal/executor/ -v 2>&1 | grep -E "^(--- FAIL|ok|FAIL)"`
Expected: `ok`

- [ ] **Step 5: 提交**

```bash
git add agent/internal/executor/
git commit -m "fix(agent): treat non-zero adb devices exit as a server failure

resolveTransport only checked err, so a dead adb server surfaced as
'device not found' - a host-level fault wearing a device's name."
```

---

### Task 5: 契约新增 failure_scope / failure_stage

**Files:**
- Modify: `contracts/result.schema.json`
- Modify: `agent/internal/reporter/result.schema.json`、`runtime/internal/callbacks/result.schema.json`(embed 副本)
- Create: `contracts/tests/examples/result/invalid/failure_scope_without_stage.yaml`、`.../failure_stage_without_scope.yaml`、`.../failure_scope_bad_enum.yaml`
- Test: `contracts/tests/test_result_schema.py`(既有 glob 会自动拾取新例子)

**Interfaces:**
- Produces: `failure_scope`(enum `device|client|none`)、`failure_stage`(enum `resolve|precheck|download|unpack|deploy|run|collect`),两者由 `dependentRequired` 双向绑定

- [ ] **Step 1: 写失败例子(反例即测试)**

先确认既有测试如何拾取例子:`contracts/tests/test_result_schema.py` 用 `example_files("result", "invalid")` glob。新增三个反例文件即自动纳入。

```yaml
# contracts/tests/examples/result/invalid/failure_scope_without_stage.yaml
# 只给 scope 不给 stage:设备会被隔离,通知却没有阶段可展示(spec §4)
result_version: 1
task_id: "wf-1:v1:a1"
attempt: 1
status: FAILED
exit_code: -1
duration_sec: 3.5
cases: { total: 0, passed: 0, failed: 0, skipped: 0 }
failure_scope: device
```

```yaml
# contracts/tests/examples/result/invalid/failure_stage_without_scope.yaml
result_version: 1
task_id: "wf-1:v1:a1"
attempt: 1
status: FAILED
exit_code: -1
duration_sec: 3.5
cases: { total: 0, passed: 0, failed: 0, skipped: 0 }
failure_stage: precheck
```

```yaml
# contracts/tests/examples/result/invalid/failure_scope_bad_enum.yaml
result_version: 1
task_id: "wf-1:v1:a1"
attempt: 1
status: FAILED
exit_code: -1
duration_sec: 3.5
cases: { total: 0, passed: 0, failed: 0, skipped: 0 }
failure_scope: broken
failure_stage: precheck
```

- [ ] **Step 2: 跑测试确认失败**

Run: `source .venv/bin/activate && python -m pytest contracts/tests/test_result_schema.py -q`
Expected: FAIL —— 三个反例目前都会被接受(schema 尚未约束)

- [ ] **Step 3: 实现 schema**

在 `contracts/result.schema.json` 的 `properties` 内加两个字段,并在顶层对象加 `dependentRequired`:

```json
    "failure_scope": {
      "enum": ["device", "client", "none"],
      "description": "导致最终非 PASSED 结局的主失败归因。缺省 = 未归因,Runtime 回落既有 category 映射。旁路/可选/清理失败一律不填。"
    },
    "failure_stage": {
      "enum": ["resolve", "precheck", "download", "unpack", "deploy", "run", "collect"],
      "description": "主失败发生的阶段,供通知与排障展示。不参与任何判定。"
    }
```

```json
  "dependentRequired": {
    "failure_scope": ["failure_stage"],
    "failure_stage": ["failure_scope"]
  }
```

同步两处 embed 副本:

```bash
cp contracts/result.schema.json agent/internal/reporter/result.schema.json
cp contracts/result.schema.json runtime/internal/callbacks/result.schema.json
```

- [ ] **Step 4: 跑测试确认通过**

Run: `source .venv/bin/activate && python -m pytest contracts/tests -q`
Expected: PASS
Run: `cd runtime && go test ./internal/callbacks/ -run EmbeddedSchema && cd ../agent && go test ./internal/reporter/ -run EmbeddedSchema`
Expected: PASS(防漂移测试确认三份一致)

- [ ] **Step 5: 提交**

```bash
git add contracts/ agent/internal/reporter/result.schema.json runtime/internal/callbacks/result.schema.json
git commit -m "feat(contracts): add paired failure_scope and failure_stage to result.json

dependentRequired binds them both ways, so a scope can never arrive
without the stage the quarantine notification needs to display."
```

---

### Task 6: Agent 归因赋值 + 两级存活复核

本计划的核心。落地 spec §5.1 判定表、§5.3 两级复核、§5.3.1 resolve 规则、§6 防线 1。

**Files:**
- Modify: `agent/internal/executor/executor.go`(`Summary`、`Execute` 各 `fail` 站点、新增 `classifyFailure`)
- Test: `agent/internal/executor/attribution_test.go`(新建)

**Interfaces:**
- Consumes: `adb.LaunchError`(Task 1)、`adb.GetState`/`adb.ParseDeviceStates`(Task 2)、`errAdbServer`(Task 4)
- Produces: `Summary.FailureScope string`、`Summary.FailureStage string`

- [ ] **Step 1: 写失败测试(表驱动,覆盖 spec §11 全部要求行)**

```go
// agent/internal/executor/attribution_test.go
func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name        string
		stage       string
		err         error
		liveness    string // 一级 get-state 的 stdout;"" = 调用失败
		devicesOut  string // 二级 adb devices -l 的 stdout
		devicesErr  error  // 二级调用失败
		wantScope   string
	}{
		{"adb 二进制起不来", "precheck", &adb.LaunchError{}, "", "", nil, "client"},
		{"adb devices 非零退出", "resolve", errAdbServer, "", "", nil, "client"},
		{"resolve 无匹配(逻辑 serial ≠ transport)", "resolve", errNoMatch, "", "", nil, "none"},
		{"ctx 取消", "run", context.Canceled, "", "", nil, "none"},
		{"只读分区 mkdir 失败但设备活着", "deploy", errRemoteExit, "device", "", nil, "none"},
		{"设备掉线:一级非 device + 二级确认缺席", "run", errRemoteExit, "", "List of devices attached\n", nil, "device"},
		{"设备 offline:二级看到非 device 状态", "run", errRemoteExit, "", "List of devices attached\ndev1\toffline\n", nil, "device"},
		{"二级调用失败:排除不掉 server 故障", "run", errRemoteExit, "", "", errors.New("boom"), "none"},
		{"属性不符", "precheck", errSOCMismatch, "", "", nil, "none"},
		{"空间不足(不走复核)", "precheck", errNoSpace, "", "", nil, "device"},
		{"本地下载失败", "download", errors.New("http 500"), "", "", nil, "client"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeADB{getStateOut: tc.liveness, devicesList: tc.devicesOut, devicesErr: tc.devicesErr}
			e, _ := newExecutor(f)
			got := e.classifyFailure(context.Background(), "dev1", tc.stage, tc.err)
			if got != tc.wantScope {
				t.Fatalf("scope = %q, want %q", got, tc.wantScope)
			}
		})
	}
}
```

需要在 `executor.go` 里定义 sentinel:`errNoMatch`、`errSOCMismatch`、`errNoSpace`、`errRemoteExit`,并让对应站点用 `%w` 包裹它们。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd agent && go test ./internal/executor/ -run ClassifyFailure -v`
Expected: FAIL,`undefined: classifyFailure`

- [ ] **Step 3: 实现**

```go
// classifyFailure 按 spec §5 判定主失败归因。不解析 stderr 文本。
func (e *Executor) classifyFailure(ctx context.Context, transport, stage string, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "none" // 多义:不归任何一方
	case errors.As(err, new(*adb.LaunchError)), errors.Is(err, errAdbServer):
		return "client" // 本地进程/server 层,与具体设备无关
	case errors.Is(err, errNoSpace):
		return "device" // df 已成功执行 → 设备明确活着,数值不足是自足证据
	case errors.Is(err, errSOCMismatch), errors.Is(err, errABIMismatch):
		return "none" // 属性可读但不匹配 = 任务派错了板
	case errors.Is(err, errNoMatch):
		return "none" // resolve 阶段:逻辑 serial 与 transport 允许不同(§5.3.1)
	case stage == "resolve":
		return "none" // resolve 未确定 transport,无从复核,保守
	}
	return e.livenessScope(ctx, transport)
}

// livenessScope 是 spec §5.3 的两级存活复核。
func (e *Executor) livenessScope(ctx context.Context, transport string) string {
	// 一级:目标设备自身状态
	if res, err := e.Runner.Run(ctx, adb.GetState(transport)); err == nil &&
		res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == "device" {
		return "none" // 设备活着,失败另有原因
	}
	// 二级:全局列表。它成功本身就证明 server 与宿主机是好的
	res, err := e.Runner.Run(ctx, adb.Devices())
	if err != nil || res.ExitCode != 0 {
		return "none" // 排除不掉 server/宿主机故障,保守
	}
	if st, ok := adb.ParseDeviceStates(res.Stdout)[transport]; !ok || st != "device" {
		return "device" // server 好的,设备却缺席或异常 → 设备证据
	}
	return "none" // 矛盾(列表说好、一级说坏),保守
}
```

各 `fail` 站点改为记录 stage 与 scope。**只有走 `fail(err)` 的路径才赋值**——`collect` 等 best-effort 路径一律不碰这两个字段(spec §6 防线 1):

```go
	fail := func(stage string, err error) (*Summary, error) {
		sum.FailureStage = stage
		sum.FailureScope = e.classifyFailure(ctx, opts.Serial, stage, err)
		e.transition(sum, StatusFailed)
		e.writeSummary(sum)
		return sum, err
	}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd agent && go test ./... 2>&1 | grep -v "^ok\|no test files"`
Expected: 无输出

- [ ] **Step 5: 提交**

```bash
git add agent/internal/executor/
git commit -m "feat(agent): attribute primary failures by call layer

Non-zero exits from adb shell are the remote command's, not the device's
- this project already hit a read-only rootfs where mkdir fails. Only a
two-tier liveness re-check promotes a failure to device evidence."
```

---

### Task 7: reporter 写入 result.json

**Files:**
- Modify: `agent/internal/reporter/result.go`(构造 result.json 处)
- Test: `agent/internal/reporter/result_test.go`

**Interfaces:**
- Consumes: `Summary.FailureScope` / `.FailureStage`(Task 6)
- Produces: result.json 含两字段(或都不含),必成对

- [ ] **Step 1: 写失败测试**

```go
func TestResultCarriesFailureAttribution(t *testing.T) {
	sum := &executor.Summary{Status: executor.StatusFailed, ExitCode: -1,
		FailureScope: "device", FailureStage: "precheck"}
	got := buildResult("t1", 1, sum, nil)
	if got.FailureScope != "device" || got.FailureStage != "precheck" {
		t.Fatalf("归因未写入 result: %+v", got)
	}
}

// 成功任务不得带归因字段(spec §6 防线 1;schema 的 dependentRequired 也要求成对)
func TestSuccessfulResultOmitsAttribution(t *testing.T) {
	sum := &executor.Summary{Status: executor.StatusCompleted, ExitCode: 0}
	got := buildResult("t1", 1, sum, nil)
	if got.FailureScope != "" || got.FailureStage != "" {
		t.Fatalf("成功任务不应带归因: %+v", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd agent && go test ./internal/reporter/ -run FailureAttribution -v`
Expected: FAIL(字段不存在)

- [ ] **Step 3: 实现**

在 reporter 的 result 结构体加两个 `omitempty` 字段并从 Summary 透传:

```go
	FailureScope string `json:"failure_scope,omitempty"`
	FailureStage string `json:"failure_stage,omitempty"`
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd agent && go test ./internal/reporter/ -v 2>&1 | grep -E "^(--- FAIL|ok|FAIL)"`
Expected: `ok`

- [ ] **Step 5: 提交**

```bash
git add agent/internal/reporter/
git commit -m "feat(agent): report failure attribution in result.json"
```

---

### Task 8: Runtime 持久化与回读

**Files:**
- Modify: `runtime/internal/workflow/devicetest.go`(`TaskResultSignal`)
- Modify: `runtime/internal/callbacks/handler.go`(result 落库)
- Modify: `runtime/internal/store/` 的 results 读写(`LoadResult` 来源)
- Test: `runtime/internal/callbacks/handler_test.go`

**Interfaces:**
- Consumes: result.json 两字段(Task 5/7)
- Produces: `wf.TaskResultSignal.FailureScope` / `.FailureStage`,供 Task 9 使用

- [ ] **Step 1: 写失败测试**

```go
func TestResultCallbackPersistsAttribution(t *testing.T) {
	// POST /callbacks/v1/results,body 含 failure_scope/failure_stage
	// 断言 LoadResult 回读时两字段完整
}
```

(实现时按 `handler_test.go` 既有的 POST 辅助函数填充完整 body。)

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/callbacks/ -run Attribution -v`
Expected: FAIL

- [ ] **Step 3: 实现**

`TaskResultSignal` 加两字段(进 workflow history,故用 `omitempty` 保持旧 history 可重放):

```go
	FailureScope string `json:"failure_scope,omitempty"`
	FailureStage string `json:"failure_stage,omitempty"`
```

callbacks 落库时一并写入 `results.result_json`;`LoadResult` 天然回读整个 JSON,无需额外改动。

- [ ] **Step 4: 跑测试确认通过**

Run: `cd runtime && go test ./internal/callbacks/ ./internal/workflow/ 2>&1 | grep -v "^ok"`
Expected: 无输出

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/callbacks/ runtime/internal/workflow/ runtime/internal/store/
git commit -m "feat(runtime): persist and reload failure attribution"
```

---

### Task 9: failScope 采信上报归因 + PASSED 纵深保护

**Files:**
- Modify: `runtime/internal/workflow/devicetest.go`(`failScope`,489-521 行)
- Test: `runtime/internal/workflow/devicetest_test.go`(既有 failScope 表驱动测试)

**Interfaces:**
- Consumes: `TaskResultSignal.FailureScope`(Task 8)
- Produces: `failScope()` 新签名,增加 `verdict` 与 `reportedScope` 入参

- [ ] **Step 1: 写失败测试**

```go
func TestFailScopeHonorsReportedScope(t *testing.T) {
	got := failScope(siteTerminal, rules.CategoryInfra, "FAILED",
		string(rules.VerdictInfraError), "device")
	if got != FailScopeDevice {
		t.Fatalf("scope = %v, want device(应采信 Agent 上报)", got)
	}
}

// 纵深防御:PASSED 时忽略任何上报 scope(spec §6 防线 2)
func TestFailScopePassedForcesOK(t *testing.T) {
	got := failScope(siteTerminal, "", "COMPLETED", string(rules.VerdictPassed), "device")
	if got != FailScopeOK {
		t.Fatalf("scope = %v, want ok(PASSED 必须清零,不许扣设备的分)", got)
	}
}

// 沉默站点永不采信(归因铁律)
func TestFailScopeIgnoresReportedScopeOnSilentSites(t *testing.T) {
	got := failScope(siteLeaseExpired, "", "", "", "device")
	if got == FailScopeDevice {
		t.Fatal("租约过期是沉默,不得据此隔离设备")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/workflow/ -run FailScope -v`
Expected: FAIL(参数个数不符)

- [ ] **Step 3: 实现**

```go
func failScope(site releaseSite, category rules.Category, resultStatus, verdict, reportedScope string) FailScope {
	switch site {
	// ... 沉默站点分支保持不变,完全不看 reportedScope ...
	case siteTerminal:
		// 防线 2 优先级最高:成功的任务永远不扣任何一方的分(spec §6)
		if verdict == string(rules.VerdictPassed) {
			return FailScopeOK
		}
		switch reportedScope {
		case "device":
			return FailScopeDevice
		case "client":
			return FailScopeClient
		case "none":
			return FailScopeNone
		}
		// 未上报 → 回落既有 category 映射,零行为变化
		// ... 原有 switch 保持不变 ...
	}
}
```

调用点补传 `d.Verdict` 与 `res.FailureScope`。

- [ ] **Step 4: 跑测试确认通过**

Run: `cd runtime && go test ./internal/workflow/ 2>&1 | grep -v "^ok"`
Expected: 无输出

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/workflow/
git commit -m "feat(runtime): honor reported attribution, force OK on PASSED

Silent sites still never yield device: a quiet agent is
indistinguishable from a dead board or a Runtime outage."
```

---

### Task 10: 隔离事务内写 outbox + audit

**Files:**
- Modify: `runtime/internal/store/devices.go`(MemStore)、`runtime/internal/store/postgres_devices.go`(PGStore `ReleaseDevice`)
- Modify: `runtime/internal/store/outbox.go`(新增 `EventTypeDeviceQuarantined` 与 payload 类型)
- Modify: `runtime/internal/activity/acts.go`(audit)
- Test: `runtime/internal/store/conformance_test.go`

**Interfaces:**
- Produces: `store.EventTypeDeviceQuarantined = "device-quarantined"`、`store.QuarantineEventPayload`,供 Task 11 消费

- [ ] **Step 1: 写失败测试**

```go
func TestQuarantineEmitsOutboxEventInSameTx(t *testing.T) {
	// 连续 3 次 device scope 释放 → 设备 QUARANTINED 且恰好一行 outbox
}

// 完整周期:event_key 用 fail_streak 时这条必失败(spec §9.2)
func TestReQuarantineAfterUnquarantineEmitsSecondEvent(t *testing.T) {
	// 隔离 → UnquarantineDevice(streak 清零)→ 再连续 3 次 → 必须有第二行 outbox
}

func TestQuarantinePayloadCarriesStageFromThatTask(t *testing.T) {
	// payload.failure_stage 必须取自触发本次隔离的 task 的已存结果,不得串号
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/store/ -run Quarantine -v`
Expected: FAIL

- [ ] **Step 3: 实现**

```go
// runtime/internal/store/outbox.go
const EventTypeDeviceQuarantined = "device-quarantined"

// QuarantineEventPayload 是设备自动隔离通知的载荷(spec §9.2)。
type QuarantineEventPayload struct {
	DeviceID     string `json:"device_id"`
	ClientID     string `json:"client_id"`
	Serial       string `json:"serial"`
	DisplayName  string `json:"display_name"`
	FailStreak   int    `json:"fail_streak"`
	TaskID       string `json:"task_id"`
	FailureStage string `json:"failure_stage"`
}
```

`PGStore.ReleaseDevice` 在置 QUARANTINED 的同一事务内:

1. 按 `task_id` 从 `results.result_json ->> 'failure_stage'` 读 stage(读不到留空)
2. `INSERT INTO outbox (...) VALUES (...) ON CONFLICT (event_key) DO NOTHING`
3. `event_key = device_id || ':quarantined:' || task_id` —— **不能用 fail_streak**,`UnquarantineDevice` 会清零它,第二次隔离会生成相同键被 UNIQUE 挡掉

- [ ] **Step 4: 跑测试确认通过**

Run: `cd runtime && go test ./internal/store/ 2>&1 | grep -v "^ok"`
Expected: 无输出

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/store/ runtime/internal/activity/
git commit -m "feat(runtime): emit a quarantine event inside the release transaction

Keyed on task_id, not fail_streak: unquarantine resets the streak, so a
streak-keyed event would collide with the first quarantine and the
second notification would never be sent."
```

---

### Task 11: Relay 投递能力 + 未配置语义 + 部署接线

spec §9.3:至少一次的保证依赖 Relay 能真的发出去,而它现在没有通知端。

**Files:**
- Modify: `runtime/internal/relay/relay.go`(`Notifier` 依赖、`deliver` 分支)
- Modify: `runtime/cmd/relay/main.go`(配置解析、Sender 构造)
- Modify: `deploy/docker-compose.yml`(relay 服务环境变量)、`deploy/.env.example`、部署文档
- Test: `runtime/internal/relay/relay_test.go`

**Interfaces:**
- Consumes: `store.EventTypeDeviceQuarantined`、`store.QuarantineEventPayload`(Task 10)
- Produces: `relay.Notifier` 接口(与 `feishu.Sender` 同形:`SendText(ctx, text) error`)

- [ ] **Step 1: 写失败测试**

```go
func TestDeliverQuarantineNotification(t *testing.T) {
	// 投递成功 → MarkPublished;Notifier 返回错误 → MarkFailed 并保持 pending
}

// 保证等级的关键:未配置且未显式关闭时不得静默成功(spec §9.3)
func TestQuarantineEventStaysPendingWhenNotifierUnconfigured(t *testing.T) {
	r := &Relay{Store: st /* Notifier 为 nil */}
	r.deliver(ctx, quarantineEvent())
	if published(st) {
		t.Fatal("未配置通知端却标记已投递 = 静默丢弃隔离通知")
	}
}

func TestQuarantineEventPublishedWhenNotifyExplicitlyOff(t *testing.T) {
	r := &Relay{Store: st, DeviceNotifyDisabled: true}
	r.deliver(ctx, quarantineEvent())
	if !published(st) {
		t.Fatal("显式关闭时应标记已投递,避免永久占用 backlog")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/relay/ -run Quarantine -v`
Expected: FAIL

- [ ] **Step 3: 实现**

```go
// Notifier 是隔离通知的发送端(与 feishu.Sender 同形)。
type Notifier interface {
	SendText(ctx context.Context, text string) error
}
```

`Relay` 加 `Notifier Notifier` 与 `DeviceNotifyDisabled bool`;`deliver` 增加分支:

```go
	case store.EventTypeDeviceQuarantined:
		var p store.QuarantineEventPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			r.fail(ctx, ev, "decode payload: "+err.Error())
			return
		}
		if r.DeviceNotifyDisabled {
			log.Info().Str("device_id", p.DeviceID).Msg("device notify disabled, event consumed")
			r.publish(ctx, ev)
			return
		}
		if r.Notifier == nil {
			// 不得静默 MarkPublished:那会把"绝不丢"退化成静默丢弃,
			// 丢的还正是最需要人知道的事件(spec §9.3)
			r.fail(ctx, ev, "notifier not configured; set FEISHU_* or RELAY_DEVICE_NOTIFY=off")
			return
		}
		text := fmt.Sprintf("[hermes-devops] 设备已自动隔离\n设备: %s (%s)\nclient: %s\n连续失败: %d 次\n触发任务: %s\n失败阶段: %s\n解除: 发送 unquarantine %s",
			p.DisplayName, p.DeviceID, p.ClientID, p.FailStreak, p.TaskID, p.FailureStage, p.DeviceID)
		if err := r.Notifier.SendText(ctx, text); err != nil {
			r.fail(ctx, ev, err.Error())
			return
		}
		r.publish(ctx, ev)
```

`cmd/relay/main.go` 读取 `FEISHU_*` 与 `RELAY_DEVICE_NOTIFY`;compose 的 relay 服务补传这些变量(共享锚点 `runtime-environment` 内不含 `FEISHU_*`,需显式加);`.env.example` 与部署文档同步说明三种语义。

- [ ] **Step 4: 跑测试确认通过**

Run: `cd runtime && go test ./internal/relay/ -v 2>&1 | grep -E "^(--- FAIL|ok|FAIL)"`
Expected: `ok`

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/relay/ runtime/cmd/relay/ deploy/
git commit -m "feat(runtime): deliver device-quarantine notifications from the relay

An unconfigured notifier leaves the event pending instead of marking it
published - otherwise at-least-once quietly becomes at-most-never for
the one event that most needs a human."
```

---

### Task 12: 端到端故障注入 + 文档收口

**Files:**
- Modify: `runtime/internal/workflow/fault_injection_test.go`
- Modify: `CLAUDE.md`(§9 表格、§10 参数、§12 Phase 4)
- Modify: `docs/device-test-sequence.md`(时序图注记、差距 #10 行)

- [ ] **Step 1: 写三条端到端用例**

```go
// 设备不可达 3 次 → QUARANTINED + 通知事件 + audit
func TestFaultInjectionDeviceUnreachableQuarantines(t *testing.T) { /* ... */ }

// 配置错误不许误伤好板(§3 核心护栏)
func TestFaultInjectionSocMismatchDoesNotQuarantine(t *testing.T) { /* ... */ }

// 旁路失败不许误伤好板(§6 防线 2 护栏)
func TestFaultInjectionPassedWithCollectFailureDoesNotQuarantine(t *testing.T) { /* ... */ }
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/workflow/ -run FaultInjection -v`
Expected: 三条新用例 FAIL

- [ ] **Step 3: 补齐实现直至通过**

若测试暴露前序任务的缺口,回到对应任务修复而不是在此处打补丁。

- [ ] **Step 4: 全量验证 + 文档**

Run: `bash scripts/lint.sh`
Expected: `ALL LINT CHECKS PASSED`

删掉三处已不成立的描述:CLAUDE.md §9 与 §10 各一处「device 归因无信号源,该阈值暂不触发」,`docs/device-test-sequence.md` 时序图注记一处「当前无信号源,暂不触发」;差距 #10 行改为已实现并指向本 spec。

- [ ] **Step 5: 提交**

```bash
git add runtime/ CLAUDE.md docs/
git commit -m "test(runtime): end-to-end quarantine fault injection; close gap #10

The two negative cases are the important ones: a config error and a
best-effort side failure must both leave a healthy board in the pool."
```

---

## 自查

**Spec 覆盖**:§4 契约 → Task 5;§5.1/5.2/5.3 → Task 6(依赖 Task 1/2);§5.3.1 → Task 4+6;§5.4 三处 → Task 1/4/3;§6 两道防线 → Task 6(防线 1)+ Task 9(防线 2);§7 → Task 8/9;§8 不变量 → Task 9 的沉默站点用例;§9.2 → Task 10;§9.3 → Task 11;§10 版本兼容 → Task 5 的 `omitempty` + 无 `additionalProperties`;§11 测试 → 分散在各任务 + Task 12;§12 十步 → 12 个任务(第 1-2 步合为 Task 1/2,第 7-8 步合为 Task 10/11)。无遗漏。

**类型一致性**:`Summary.FailureScope`(string)→ result.json `failure_scope` → `TaskResultSignal.FailureScope`(string)→ `failScope(..., reportedScope string)` → `FailScope` 枚举,全链路命名一致。`adb.LaunchError`(Task 1 产出)在 Task 6 消费;`adb.GetState`/`ParseDeviceStates`(Task 2)在 Task 6 消费;`errAdbServer`(Task 4)在 Task 6 消费;`EventTypeDeviceQuarantined`/`QuarantineEventPayload`(Task 10)在 Task 11 消费。

**风险提示**:Task 6 是唯一改动面较大的任务,它依赖 Task 1/2/4 全部完成。若执行中发现 `fakeADB` 需要较多新字段,优先扩展 fake 而不是绕过复核逻辑——复核路径正是本设计的安全核心。
