# Runtime Worker Activities 实施计划(Phase 1 第 6 步收尾)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 DeviceTestWorkflow 所需的全部真实 Activity、回调 API(心跳/事件/结果 → store + Temporal signal)与 `cmd/worker` 主程序,使 Trigger→Workflow→(假)Client→回调→verdict→飞书通知的链路可端到端跑通(内存 store,Postgres 持久化另立计划)。

**Architecture:** Activity 是薄胶水:store 型活动直调 `store.MemStore` 上已实现并测试过的方法;HTTP 型活动按 `contracts/client-agent-api.openapi.yaml` 调 Client Agent;回调服务按 `contracts/callbacks-api.openapi.yaml` 收 Client 上报,落库去重后向 workflow 投 signal(禁止轮询,§14)。结果持久化从 workflow 的 `RecordResult` 活动移到回调服务(SaveResult 先落库去重、再 signal),workflow 相应删掉该活动调用。

**Tech Stack:** Go 1.26、Temporal Go SDK、santhosh-tekuri/jsonschema/v5(已是直接依赖)、gopkg.in/yaml.v3(将从间接转直接)、zerolog。

## Global Constraints

- CLAUDE.md §3/§14 红线:Runtime 禁止轮询等 Client 结果(必须 signal);未经 Schema 校验不消费 result.json;附件不经 Runtime 中转。
- §10 缺省值:租约 120s;任务级机械重试 max 2(仅 INFRA);设备隔离阈值连续 3 次 INFRA。
- 契约只加字段不删字段;时间一律 UTC;跨网络调用带 context 超时;提交信息英文。
- 所有测试命令在 `runtime/` 目录下执行,Go 位于 `~/.local/go/bin`(需 `export PATH=$PATH:~/.local/go/bin`)。
- workflow 内代码必须确定性:map 迭代不可影响输出顺序(按 `in.Packages` 切片顺序产出)。

---

### Task 1: SelectTestSpecs 活动(variants 配置 → TestSpec)

**Files:**
- Create: `runtime/internal/activity/specs.go`
- Create: `runtime/internal/activity/specs_test.go`
- Create: `runtime/internal/activity/testdata/variants.yaml`

**Interfaces:**
- Consumes: `wf.DeviceTestInput / wf.TestSpec / wf.DeviceSelector`(workflow 包,已存在)、`rules.Category`。
- Produces: `LoadSpecConfig(path string, d SpecDefaults) (*SpecConfig, error)`;`(*Acts).SelectTestSpecs(ctx, wf.DeviceTestInput) ([]wf.TestSpec, error)`。`Acts` 结构体本任务先只声明 `SpecCfg *SpecConfig` 字段,后续任务补其余字段。

- [ ] **Step 1: 写 testdata 与失败测试**

`runtime/internal/activity/testdata/variants.yaml`(ci/variants.yaml 的最小截取,结构一致):

```yaml
defaults:
  test:
    timeout_sec: 900
  signatures_common_android:
    - { id: native_crash, where: logcat, pattern: "Fatal signal|tombstone", classify: CODE }

variants:
  aarch64_Android_SNPE_2.21:
    requirements: { os: android, abi: arm64-v8a, soc: [QCM6125], capabilities: [hexagon], min_free_storage_mb: 512 }
    test:
      args: ["--suite", "snpe-smoke", "--output", "results/"]
    signatures:
      - { id: cpu_fallback, where: logcat, pattern: "Falling back to CPU", classify: MODEL }
  aarch64_Linux_SNPE_2.21:
    requirements: { os: linux, abi: aarch64 }
```

`runtime/internal/activity/specs_test.go`:

```go
package activity

import (
	"context"
	"testing"

	"hermes-devops/runtime/internal/rules"
	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

func testActs(t *testing.T) *Acts {
	t.Helper()
	cfg, err := LoadSpecConfig("testdata/variants.yaml", SpecDefaults{
		MaxInfraRetries: 2, LeaseSeconds: 120, HardTimeoutMargin: 1200,
		DeviceWaitRounds: 20, DeviceWaitSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Acts{SpecCfg: cfg}
}

func TestSelectTestSpecsAndroidOnly(t *testing.T) {
	a := testActs(t)
	in := wf.DeviceTestInput{Project: "algo-super-sdk", Commit: "abc1234", PipelineID: 42,
		Packages: []wf.PackageRef{
			{Variant: "aarch64_Android_SNPE_2.21", URL: "https://gitlab/pkg1", SHA256: "aa", ManifestDigest: "dd"},
			{Variant: "aarch64_Linux_SNPE_2.21", URL: "https://gitlab/pkg2"}, // Linux:不进链路(§6.4)
			{Variant: "unknown_variant", URL: "https://gitlab/pkg3"},         // 未配置:跳过
		}}
	specs, err := a.SelectTestSpecs(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1(仅 Android 变体)", len(specs))
	}
	s := specs[0]
	if s.TestID != "aarch64_Android_SNPE_2.21" || s.Variant != s.TestID || s.Package.URL != "https://gitlab/pkg1" {
		t.Errorf("spec = %+v", s)
	}
	if len(s.Selector.SOC) != 1 || s.Selector.SOC[0] != "QCM6125" || s.Selector.Capabilities[0] != "hexagon" {
		t.Errorf("selector = %+v", s.Selector)
	}
	// 签名分类 = 公共 Android 签名 + 变体私有签名
	if s.SignatureCategory["native_crash"] != rules.CategoryCode || s.SignatureCategory["cpu_fallback"] != rules.CategoryModel {
		t.Errorf("signatures = %+v", s.SignatureCategory)
	}
	// §10 缺省 + 硬超时 = timeout_sec + margin
	if s.MaxInfraRetries != 2 || s.LeaseSeconds != 120 || s.HardTimeoutSec != 2100 {
		t.Errorf("knobs = %+v", s)
	}
}

func TestLoadSpecConfigMissingFile(t *testing.T) {
	if _, err := LoadSpecConfig("testdata/nonexistent.yaml", SpecDefaults{}); err == nil {
		t.Error("缺失文件应报错(worker 启动时 fail fast)")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/activity/ -run TestSelectTestSpecs -v`
Expected: FAIL,`undefined: Acts` / `undefined: LoadSpecConfig`(编译失败即正确的 RED)。

- [ ] **Step 3: 最小实现**

`runtime/internal/activity/specs.go`:

```go
// Package activity 实现 DeviceTestWorkflow 引用的全部活动(CLAUDE.md §12.6)。
// 活动是薄胶水:store 型直调 store 方法,HTTP 型按 contracts/ 契约调外部服务。
package activity

import (
	"context"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"hermes-devops/runtime/internal/rules"
	wf "hermes-devops/runtime/internal/workflow"
)

// Acts 承载全部活动;字段随任务推进补齐(Store/Cfg/HTTP 见后续任务)。
type Acts struct {
	SpecCfg *SpecConfig
}

// SpecDefaults 是 TestSpec 调度参数缺省值(§10)。
type SpecDefaults struct {
	MaxInfraRetries   int // 缺省 2(仅 INFRA)
	LeaseSeconds      int // 缺省 120
	HardTimeoutMargin int // 叠加在 test.timeout_sec 上,容纳下载/部署/收集
	DeviceWaitRounds  int
	DeviceWaitSeconds int
}

type signatureDecl struct {
	ID       string `yaml:"id"`
	Classify string `yaml:"classify"`
}

// variantsFile 是 ci/variants.yaml 的运行时视图,只解析调度所需字段。
type variantsFile struct {
	Defaults struct {
		Test struct {
			TimeoutSec int `yaml:"timeout_sec"`
		} `yaml:"test"`
		SignaturesCommonAndroid []signatureDecl `yaml:"signatures_common_android"`
	} `yaml:"defaults"`
	Variants map[string]variantDecl `yaml:"variants"`
}

type variantDecl struct {
	Requirements struct {
		OS           string   `yaml:"os"`
		SOC          []string `yaml:"soc"`
		Capabilities []string `yaml:"capabilities"`
	} `yaml:"requirements"`
	Test struct {
		TimeoutSec int `yaml:"timeout_sec"`
	} `yaml:"test"`
	Signatures []signatureDecl `yaml:"signatures"`
}

// SpecConfig 是 worker 启动时加载的变体配置(加载失败 fail fast)。
type SpecConfig struct {
	file     variantsFile
	defaults SpecDefaults
}

func LoadSpecConfig(path string, d SpecDefaults) (*SpecConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read variants config: %w", err)
	}
	var f variantsFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse variants config: %w", err)
	}
	return &SpecConfig{file: f, defaults: d}, nil
}

// SelectTestSpecs 把 bundle 中的 Android 变体映射为 TestSpec;
// Linux 变体第一阶段不进设备测试链路(§6.4),未配置变体跳过。
// 输出顺序跟随 in.Packages(workflow 依赖确定性)。
func (a *Acts) SelectTestSpecs(_ context.Context, in wf.DeviceTestInput) ([]wf.TestSpec, error) {
	var specs []wf.TestSpec
	for _, p := range in.Packages {
		v, ok := a.SpecCfg.file.Variants[p.Variant]
		if !ok || v.Requirements.OS != "android" {
			continue
		}
		timeout := v.Test.TimeoutSec
		if timeout == 0 {
			timeout = a.SpecCfg.file.Defaults.Test.TimeoutSec
		}
		sigs := map[string]rules.Category{}
		for _, s := range a.SpecCfg.file.Defaults.SignaturesCommonAndroid {
			sigs[s.ID] = rules.Category(s.Classify)
		}
		for _, s := range v.Signatures {
			sigs[s.ID] = rules.Category(s.Classify)
		}
		d := a.SpecCfg.defaults
		specs = append(specs, wf.TestSpec{
			TestID:  p.Variant,
			Variant: p.Variant,
			Package: p,
			Selector: wf.DeviceSelector{
				SOC:          v.Requirements.SOC,
				Capabilities: v.Requirements.Capabilities,
			},
			SignatureCategory: sigs,
			MaxInfraRetries:   d.MaxInfraRetries,
			LeaseSeconds:      d.LeaseSeconds,
			HardTimeoutSec:    timeout + d.HardTimeoutMargin,
			DeviceWaitRounds:  d.DeviceWaitRounds,
			DeviceWaitSeconds: d.DeviceWaitSeconds,
		})
	}
	return specs, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go mod tidy && go test ./internal/activity/ -v`
Expected: 2 个测试 PASS;`go.mod` 中 `gopkg.in/yaml.v3` 转为直接依赖。

- [ ] **Step 5: Commit**

```bash
git add runtime/internal/activity/ runtime/go.mod runtime/go.sum
git commit -m "feat(runtime): SelectTestSpecs activity maps variants config to TestSpec"
```

---

### Task 2: store 型活动(AcquireDevice/CreateTask/FinishTask/ReleaseDevice)

**Files:**
- Create: `runtime/internal/activity/acts.go`
- Create: `runtime/internal/activity/acts_test.go`
- Modify: `runtime/internal/activity/specs.go`(Acts 增加字段)

**Interfaces:**
- Consumes: `store.MemStore` 上已测试的方法(`AcquireDevice(ctx, sel, taskID, leaseSeconds)`, `ReleaseDevice(ctx, deviceID, taskID, infraFail, quarantineAfter)`, `CreateTask`, `FinishTask`)。
- Produces: `activity.Store` 接口;`Config{LeaseSeconds, QuarantineAfter, ...}`;`(*Acts)` 方法 `AcquireDevice(ctx, wf.AcquireRequest) (*wf.Lease, error)`、`CreateTask(ctx, wf.TaskRow) error`、`FinishTask(ctx, wf.FinishRequest) error`、`ReleaseDevice(ctx, wf.ReleaseRequest) error`(方法名即 workflow 内的活动字符串名)。

- [ ] **Step 1: 写失败测试**

`runtime/internal/activity/acts_test.go`:

```go
package activity

import (
	"testing"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

func storeWithDevice(t *testing.T) *store.MemStore {
	t.Helper()
	s := store.NewMemStore()
	err := s.UpsertClientDevices(ctx,
		store.Client{ClientID: "c1", BaseURL: "https://client:8443"},
		[]store.Device{{DeviceID: "513cd3de", Serial: "513cd3de", ClientID: "c1",
			SOC: "QCM6125", ABI: "arm64-v8a", Capabilities: []string{"hexagon"}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreActsPassConfigThrough(t *testing.T) {
	s := storeWithDevice(t)
	a := &Acts{Store: s, Cfg: Config{LeaseSeconds: 120, QuarantineAfter: 3}}

	l, err := a.AcquireDevice(ctx, wf.AcquireRequest{TaskID: "t1",
		Selector: wf.DeviceSelector{SOC: []string{"QCM6125"}}})
	if err != nil || l == nil || l.ClientBaseURL != "https://client:8443" {
		t.Fatalf("lease=%+v err=%v", l, err)
	}
	if err := a.CreateTask(ctx, wf.TaskRow{TaskID: "t1", IdempotencyKey: "t1", Status: "DISPATCHING"}); err != nil {
		t.Fatal(err)
	}
	if err := a.FinishTask(ctx, wf.FinishRequest{TaskID: "t1", Status: "COMPLETED", Verdict: "PASSED"}); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseDevice(ctx, wf.ReleaseRequest{DeviceID: l.DeviceID, TaskID: "t1", InfraFail: false}); err != nil {
		t.Fatal(err)
	}
	// QuarantineAfter=3 生效:连续 3 次 INFRA 释放后设备隔离
	for i := 0; i < 3; i++ {
		l, _ := a.AcquireDevice(ctx, wf.AcquireRequest{TaskID: "tx"})
		if l == nil {
			t.Fatalf("第 %d 次应能获取", i+1)
		}
		_ = a.ReleaseDevice(ctx, wf.ReleaseRequest{DeviceID: l.DeviceID, TaskID: "tx", InfraFail: true})
	}
	if l, _ := a.AcquireDevice(ctx, wf.AcquireRequest{TaskID: "ty"}); l != nil {
		t.Error("连续 3 次 INFRA 后设备应 QUARANTINED(§10)")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/activity/ -run TestStoreActs -v`
Expected: FAIL,`unknown field Store in struct literal` 等编译错误。

- [ ] **Step 3: 最小实现**

`runtime/internal/activity/acts.go`:

```go
package activity

import (
	"context"
	"net/http"

	wf "hermes-devops/runtime/internal/workflow"
)

// Store 是活动依赖的持久层子集;store.MemStore 与(后续)PGStore 均满足。
type Store interface {
	AcquireDevice(ctx context.Context, sel wf.DeviceSelector, taskID string, leaseSeconds int) (*wf.Lease, error)
	ReleaseDevice(ctx context.Context, deviceID, taskID string, infraFail bool, quarantineAfter int) error
	CreateTask(ctx context.Context, row wf.TaskRow) error
	FinishTask(ctx context.Context, req wf.FinishRequest) error
}

// Config 是活动运行参数(§10 缺省值 + 外部端点)。
type Config struct {
	LeaseSeconds      int    // 任务租约,缺省 120
	QuarantineAfter   int    // 连续 INFRA 隔离阈值,缺省 3
	CallbackBaseURL   string // 派单时告知 Client 的回调地址(§8.1)
	ArtifactAuthType  string // bearer | job_token
	ArtifactAuthToken string
	FeishuWebhookURL  string // 为空则 Notify 仅记日志(开发模式)
}

func (a *Acts) AcquireDevice(ctx context.Context, req wf.AcquireRequest) (*wf.Lease, error) {
	return a.Store.AcquireDevice(ctx, req.Selector, req.TaskID, a.Cfg.LeaseSeconds)
}

func (a *Acts) CreateTask(ctx context.Context, row wf.TaskRow) error {
	return a.Store.CreateTask(ctx, row)
}

func (a *Acts) FinishTask(ctx context.Context, req wf.FinishRequest) error {
	return a.Store.FinishTask(ctx, req)
}

func (a *Acts) ReleaseDevice(ctx context.Context, req wf.ReleaseRequest) error {
	return a.Store.ReleaseDevice(ctx, req.DeviceID, req.TaskID, req.InfraFail, a.Cfg.QuarantineAfter)
}
```

`specs.go` 中 Acts 定义改为:

```go
// Acts 承载全部活动,方法名即 workflow 内引用的活动名。
type Acts struct {
	Store   Store
	Cfg     Config
	HTTP    *http.Client // Dispatch/CancelTask/Notify 用(Task 3)
	SpecCfg *SpecConfig
}
```

(`http` import 加到 specs.go 或统一挪 Acts 定义到 acts.go——二选一,推荐把 `Acts` 结构体挪到 acts.go,specs.go 只留 SpecConfig 相关。)

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/activity/ -v`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add runtime/internal/activity/
git commit -m "feat(runtime): store-backed activities for device lease and task rows"
```

---

### Task 3: HTTP 型活动(Dispatch/CancelTask/Notify)

**Files:**
- Create: `runtime/internal/activity/dispatch.go`
- Create: `runtime/internal/activity/notify.go`
- Create: `runtime/internal/activity/dispatch_test.go`
- Create: `runtime/internal/activity/notify_test.go`

**Interfaces:**
- Consumes: `wf.DispatchRequest / wf.CancelRequest`(workflow 包);`contracts/client-agent-api.openapi.yaml` 的 `TaskDispatchRequest` 载荷。
- Produces: `(*Acts).Dispatch(ctx, wf.DispatchRequest) error`、`(*Acts).CancelTask(ctx, wf.CancelRequest) error`、`(*Acts).Notify(ctx, text string) error`。

- [ ] **Step 1: 写失败测试**

`runtime/internal/activity/dispatch_test.go`:

```go
package activity

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	wf "hermes-devops/runtime/internal/workflow"
)

func TestDispatchPostsContractPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a := &Acts{HTTP: srv.Client(), Cfg: Config{
		CallbackBaseURL: "https://runtime:8091", ArtifactAuthType: "bearer", ArtifactAuthToken: "tok"}}
	err := a.Dispatch(ctx, wf.DispatchRequest{
		TaskID: "w:t:a1", IdempotencyKey: "w:t:a1", Attempt: 1,
		PackageURL: "https://gitlab/pkg", PackageSHA256: "ab12", ManifestDigest: "cd34",
		DeviceSerial: "513cd3de", ClientBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	// §8.1 TaskDispatchRequest 必填字段
	if got["task_id"] != "w:t:a1" || got["idempotency_key"] != "w:t:a1" ||
		got["manifest_digest"] != "cd34" || got["device_serial"] != "513cd3de" ||
		got["callback_base_url"] != "https://runtime:8091" {
		t.Errorf("payload = %v", got)
	}
	art := got["artifact"].(map[string]any)
	auth := art["auth"].(map[string]any)
	if art["url"] != "https://gitlab/pkg" || art["sha256"] != "ab12" ||
		auth["type"] != "bearer" || auth["token"] != "tok" {
		t.Errorf("artifact = %v", art)
	}
}

func TestDispatchNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"code":"version_too_low","message":"agent too old"}`, http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	a := &Acts{HTTP: srv.Client(), Cfg: Config{ArtifactAuthType: "bearer", ArtifactAuthToken: "t"}}
	if err := a.Dispatch(ctx, wf.DispatchRequest{TaskID: "t", ClientBaseURL: srv.URL}); err == nil {
		t.Error("422 应返回 error(触发活动重试/INFRA 处理)")
	}
}

func TestCancelTask404IsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/tasks/w:t:a1" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	a := &Acts{HTTP: srv.Client()}
	if err := a.CancelTask(ctx, wf.CancelRequest{TaskID: "w:t:a1", ClientBaseURL: srv.URL}); err != nil {
		t.Errorf("404(任务已不存在)应视为取消成功: %v", err)
	}
}
```

`runtime/internal/activity/notify_test.go`:

```go
package activity

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNotifyPostsFeishuText(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()
	a := &Acts{HTTP: srv.Client(), Cfg: Config{FeishuWebhookURL: srv.URL}}
	if err := a.Notify(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if got["msg_type"] != "text" || got["content"].(map[string]any)["text"] != "hello" {
		t.Errorf("payload = %v", got)
	}
}

func TestNotifyFeishuBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":19001,"msg":"param invalid"}`))
	}))
	defer srv.Close()
	a := &Acts{HTTP: srv.Client(), Cfg: Config{FeishuWebhookURL: srv.URL}}
	if err := a.Notify(ctx, "hello"); err == nil || !strings.Contains(err.Error(), "19001") {
		t.Errorf("飞书业务错误码应报错, got %v", err)
	}
}

func TestNotifyNoWebhookConfigured(t *testing.T) {
	a := &Acts{}
	if err := a.Notify(ctx, "hello"); err != nil {
		t.Errorf("未配置 webhook 应静默成功(开发模式): %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/activity/ -run 'TestDispatch|TestCancel|TestNotify' -v`
Expected: FAIL,`a.Dispatch undefined` 等编译错误。

- [ ] **Step 3: 最小实现**

`runtime/internal/activity/dispatch.go`:

```go
package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	wf "hermes-devops/runtime/internal/workflow"
)

// Dispatch 按 §8.1 POST /api/v1/tasks 派单;非 2xx 返回 error,
// 由 workflow 的 on_infra_error 策略处理。凭据与回调地址由 Config 补充。
func (a *Acts) Dispatch(ctx context.Context, req wf.DispatchRequest) error {
	payload := map[string]any{
		"task_id":         req.TaskID,
		"idempotency_key": req.IdempotencyKey,
		"attempt":         req.Attempt,
		"artifact": map[string]any{
			"url":    req.PackageURL,
			"sha256": req.PackageSHA256,
			"auth":   map[string]any{"type": a.Cfg.ArtifactAuthType, "token": a.Cfg.ArtifactAuthToken},
		},
		"manifest_digest":   req.ManifestDigest,
		"device_serial":     req.DeviceSerial,
		"callback_base_url": a.Cfg.CallbackBaseURL,
		"presigned_uploads": []any{}, // MinIO 预签名直传属 Phase 1.7
	}
	return a.post(ctx, req.ClientBaseURL+"/api/v1/tasks", payload, http.StatusAccepted)
}

// CancelTask 尽力而为取消(§8.1);404 表示 Client 已无此任务,视为成功。
func (a *Acts) CancelTask(ctx context.Context, req wf.CancelRequest) error {
	u := req.ClientBaseURL + "/api/v1/tasks/" + url.PathEscape(req.TaskID)
	hr, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := a.HTTP.Do(hr)
	if err != nil {
		return fmt.Errorf("cancel task: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return fmt.Errorf("cancel task: unexpected status %d", resp.StatusCode)
}

// post 发 JSON 并校验预期状态码(2xx 且优先匹配 want)。
func (a *Acts) post(ctx context.Context, u string, payload any, want int) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(hr)
	if err != nil {
		return fmt.Errorf("post %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("post %s: status %d: %s", u, resp.StatusCode, msg)
	}
	return nil
}
```

`runtime/internal/activity/notify.go`:

```go
package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Notify 发飞书自定义机器人纯文本(Phase 1,§12.6;交互卡片属 Phase 2)。
// 未配置 webhook 时静默成功(开发模式)。
func (a *Acts) Notify(ctx context.Context, text string) error {
	if a.Cfg.FeishuWebhookURL == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	})
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Cfg.FeishuWebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(hr)
	if err != nil {
		return fmt.Errorf("feishu notify: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feishu notify: status %d", resp.StatusCode)
	}
	var ack struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return fmt.Errorf("feishu notify: decode ack: %w", err)
	}
	if ack.Code != 0 {
		return fmt.Errorf("feishu notify: code %d: %s", ack.Code, ack.Msg)
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/activity/ -v`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add runtime/internal/activity/
git commit -m "feat(runtime): dispatch/cancel/notify HTTP activities per client-agent contract"
```

---

### Task 4: workflow 移除 RecordResult(结果改由回调服务落库)

**背景:** 回调服务(Task 5)在向 workflow 投 signal 前先 `SaveResult` 落库并去重("signal 只投递一次",§8.2)。workflow 里再调 `RecordResult` 就成了重复写,删除之。

**Files:**
- Modify: `runtime/internal/workflow/devicetest.go:272-276`
- Modify: `runtime/internal/workflow/devicetest_test.go:30,75-79,164-165`

**Interfaces:**
- Produces: workflow 不再引用名为 `RecordResult` 的活动;`wf.ResultRecord` 类型保留(回调服务与 store.SaveResult 使用)。

- [ ] **Step 1: 修改 workflow 与测试**

`devicetest.go` 删除:

```go
	// ---- 落结果 + 规则引擎判 verdict ----
	if err := workflow.ExecuteActivity(ctx, "RecordResult",
		ResultRecord{TaskID: taskID, Result: *res}).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("record result failed", "error", err)
	}
```

替换为注释:

```go
	// ---- 规则引擎判 verdict(结果本体已由回调服务 SaveResult 落库,§8.2) ----
```

`ResultRecord` 类型定义处(devicetest.go:108)补注释:

```go
// ResultRecord 是 results 表一行;由回调服务在投 signal 前落库(SaveResult 去重),
// workflow 不再经手结果持久化。
```

`devicetest_test.go` 删除:
- 第 30 行 `recorded []ResultRecord` 字段;
- 第 75-79 行 `func (f *fakeActs) RecordResult(...)` 整个方法;
- 第 164-165 行断言改为:

```go
	if len(f.finished) != 1 || f.finished[0].Verdict != "PASSED" {
		t.Errorf("finished=%+v", f.finished)
	}
```

- [ ] **Step 2: 跑 workflow 测试确认仍绿**

Run: `go test ./internal/workflow/ -count=1 -v`
Expected: 全部 PASS(fakeActs 少注册一个活动不影响,workflow 不再调用它)。

- [ ] **Step 3: Commit**

```bash
git add runtime/internal/workflow/
git commit -m "refactor(runtime): move result persistence from workflow to callbacks service"
```

---

### Task 5: 回调服务(heartbeat / task-events / results → store + signal)

**Files:**
- Create: `runtime/internal/callbacks/handler.go`
- Create: `runtime/internal/callbacks/handler_test.go`
- Create: `runtime/internal/callbacks/result.schema.json`(从 `contracts/result.schema.json` 原样复制,同 trigger 包对 bundle.schema.json 的做法)
- Modify: `contracts/callbacks-api.openapi.yaml`(Heartbeat 增加可选 `base_url`)

**Interfaces:**
- Consumes: `store.MemStore` 的 `UpsertClientDevices / AppendTaskEvent / SetTaskStatus / GetTask / SaveResult`;`wf.SignalTaskResult / wf.SignalTaskHeartbeat / wf.TaskResultSignal / wf.TaskHeartbeat / wf.ResultRecord`。
- Produces: `callbacks.Store` 接口、`callbacks.Signaler` 接口(`SignalWorkflow(ctx, workflowID, runID, signalName string, arg interface{}) error`,temporal `client.Client` 天然满足)、`callbacks.New(Store, Signaler, *zerolog.Logger) *Handler`、`(*Handler).Mux() *http.ServeMux`。

- [ ] **Step 1: 契约增加 base_url(只加不删)**

`contracts/callbacks-api.openapi.yaml` 的 `Heartbeat.properties` 中 `client_id` 之后加:

```yaml
        base_url:
          type: string
          format: uri
          description: |
            本 Client 的 API 基地址(https://host:port),Runtime 派单用。
            CONTRACT-ISSUE: §8.2 原文心跳载荷未含派单地址,Runtime 无从得知
            Client API 端点;按"只加字段不删字段"新增可选字段。
```

- [ ] **Step 2: 写失败测试**

`runtime/internal/callbacks/handler_test.go`:

```go
package callbacks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

type fakeSignaler struct {
	mu    sync.Mutex
	calls []string // "workflowID/signalName/taskID"
	err   error
}

func (f *fakeSignaler) SignalWorkflow(_ context.Context, wfID, _ string, name string, arg interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var tid string
	switch v := arg.(type) {
	case wf.TaskHeartbeat:
		tid = v.TaskID
	case wf.TaskResultSignal:
		tid = v.TaskID
	}
	f.calls = append(f.calls, fmt.Sprintf("%s/%s/%s", wfID, name, tid))
	return f.err
}

func newEnv(t *testing.T) (*store.MemStore, *fakeSignaler, *httptest.Server) {
	t.Helper()
	s := store.NewMemStore()
	sig := &fakeSignaler{}
	h := New(s, sig, nil)
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return s, sig, srv
}

func post(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func validResult(taskID string) map[string]any {
	return map[string]any{
		"result_version": 1, "task_id": taskID, "attempt": 1,
		"status": "COMPLETED", "exit_code": 0, "duration_sec": 412.5,
		"cases":          map[string]any{"total": 38, "passed": 38, "failed": 0, "skipped": 0},
		"signatures_hit": []string{},
		"metrics":        map[string]any{"latency_ms_p50": 12.4},
		"attachments": []map[string]any{{"name": "logcat.txt", "object_key": "runs/x/logcat.txt",
			"sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "size": 1024}},
	}
}

func TestHeartbeatUpsertsAndRenewsLeases(t *testing.T) {
	s, sig, srv := newEnv(t)
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1", IdempotencyKey: "w1:t:a1"})

	resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
		"client_id": "c1", "agent_version": "0.1.0", "base_url": "https://client:8443",
		"ts": "2026-07-17T08:00:00.000Z",
		"devices": []map[string]any{{"serial": "513cd3de", "state": "IDLE",
			"props": map[string]any{"soc": "QCM6125", "abi": "arm64-v8a", "capabilities": []string{"hexagon"}}}},
		"active_task_ids": []string{"w1:t:a1", "unknown-task"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// 设备入库可被调度
	l, err := s.AcquireDevice(ctx, wf.DeviceSelector{SOC: []string{"QCM6125"}}, "t9", 120)
	if err != nil || l == nil || l.ClientBaseURL != "https://client:8443" {
		t.Errorf("lease=%+v err=%v", l, err)
	}
	// 已知任务续租 signal;未知任务忽略不报错
	if len(sig.calls) != 1 || sig.calls[0] != "w1/"+wf.SignalTaskHeartbeat+"/w1:t:a1" {
		t.Errorf("signals = %v", sig.calls)
	}
}

func TestTaskEventDedupAndStatus(t *testing.T) {
	s, _, srv := newEnv(t)
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1", IdempotencyKey: "w1:t:a1", Status: "DISPATCHING"})
	ev := map[string]any{"task_id": "w1:t:a1", "idempotency_key": "w1:t:a1", "seq": 1,
		"from": "ACCEPTED", "to": "RUNNING", "ts": "2026-07-17T08:00:01.000Z"}
	if resp := post(t, srv.URL+"/callbacks/v1/task-events", ev); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/callbacks/v1/task-events", ev); resp.StatusCode != http.StatusOK {
		t.Fatalf("重发 status = %d(幂等,§8.2)", resp.StatusCode)
	}
	row, _ := s.GetTask(ctx, "w1:t:a1")
	if row.Status != "RUNNING" {
		t.Errorf("status = %s", row.Status)
	}
}

func TestResultValidateSaveSignalOnce(t *testing.T) {
	s, sig, srv := newEnv(t)
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1", IdempotencyKey: "w1:t:a1"})

	body := map[string]any{"task_id": "w1:t:a1", "idempotency_key": "w1:t:a1", "result": validResult("w1:t:a1")}
	if resp := post(t, srv.URL+"/callbacks/v1/results", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/callbacks/v1/results", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("重发 status = %d", resp.StatusCode)
	}
	// signal 只投递一次(§8.2),载荷字段来自 result.json
	if len(sig.calls) != 1 || sig.calls[0] != "w1/"+wf.SignalTaskResult+"/w1:t:a1" {
		t.Errorf("signals = %v", sig.calls)
	}
}

func TestResultSchemaViolationIs400(t *testing.T) {
	s, sig, srv := newEnv(t)
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1", IdempotencyKey: "w1:t:a1"})
	bad := validResult("w1:t:a1")
	delete(bad, "cases") // 缺必填字段
	resp := post(t, srv.URL+"/callbacks/v1/results",
		map[string]any{"task_id": "w1:t:a1", "idempotency_key": "w1:t:a1", "result": bad})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400(红线:未经 Schema 校验不消费,§14)", resp.StatusCode)
	}
	if len(sig.calls) != 0 {
		t.Errorf("非法 result 不得 signal: %v", sig.calls)
	}
}

func TestResultUnknownTaskIs400(t *testing.T) {
	_, sig, srv := newEnv(t)
	resp := post(t, srv.URL+"/callbacks/v1/results",
		map[string]any{"task_id": "ghost", "idempotency_key": "ghost", "result": validResult("ghost")})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if len(sig.calls) != 0 {
		t.Errorf("未知任务不得 signal: %v", sig.calls)
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/callbacks/ -v`
Expected: FAIL,`undefined: New` 等编译错误。

- [ ] **Step 4: 复制 schema + 最小实现**

```bash
cp contracts/result.schema.json runtime/internal/callbacks/result.schema.json
```

`runtime/internal/callbacks/handler.go`:

```go
// Package callbacks 实现 Client → Runtime 回调 API(CLAUDE.md §8.2,
// contracts/callbacks-api.openapi.yaml):心跳(设备注册 + 租约续期 signal)、
// 任务事件(按 task_id+seq 去重)、终态结果(Schema 校验 → SaveResult 去重 → signal)。
package callbacks

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/rs/zerolog"
	"github.com/santhosh-tekuri/jsonschema/v5"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

//go:embed result.schema.json
var resultSchemaJSON string

var resultSchema = mustCompileResultSchema()

func mustCompileResultSchema() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("result.schema.json", strings.NewReader(resultSchemaJSON)); err != nil {
		panic(err)
	}
	return c.MustCompile("result.schema.json")
}

// Store 是回调服务依赖的持久层子集。
type Store interface {
	UpsertClientDevices(ctx context.Context, c store.Client, devs []store.Device) error
	AppendTaskEvent(ctx context.Context, ev store.TaskEvent) (bool, error)
	SetTaskStatus(ctx context.Context, taskID, status string) error
	GetTask(ctx context.Context, taskID string) (*wf.TaskRow, error)
	SaveResult(ctx context.Context, rec wf.ResultRecord) (bool, error)
}

// Signaler 是 temporal client.Client 的 signal 子集。
type Signaler interface {
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
}

type Handler struct {
	store    Store
	signaler Signaler
	log      zerolog.Logger
}

func New(s Store, sig Signaler, log *zerolog.Logger) *Handler {
	l := zerolog.Nop()
	if log != nil {
		l = *log
	}
	return &Handler{store: s, signaler: sig, log: l}
}

func (h *Handler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /callbacks/v1/heartbeat", h.heartbeat)
	mux.HandleFunc("POST /callbacks/v1/task-events", h.taskEvent)
	mux.HandleFunc("POST /callbacks/v1/results", h.result)
	return mux
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

// ---- heartbeat ----

type heartbeatReq struct {
	ClientID     string `json:"client_id"`
	AgentVersion string `json:"agent_version"`
	BaseURL      string `json:"base_url"` // 契约新增可选字段(见 openapi CONTRACT-ISSUE)
	Devices      []struct {
		Serial string `json:"serial"`
		State  string `json:"state"`
		Props  struct {
			SOC          string   `json:"soc"`
			ABI          string   `json:"abi"`
			Capabilities []string `json:"capabilities"`
		} `json:"props"`
	} `json:"devices"`
	ActiveTaskIDs []string `json:"active_task_ids"`
}

func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ClientID == "" {
		writeErr(w, http.StatusBadRequest, "bad_heartbeat", "invalid heartbeat payload")
		return
	}
	devs := make([]store.Device, 0, len(req.Devices))
	for _, d := range req.Devices {
		devs = append(devs, store.Device{
			DeviceID: d.Serial, Serial: d.Serial, ClientID: req.ClientID,
			SOC: d.Props.SOC, ABI: d.Props.ABI, Capabilities: d.Props.Capabilities,
		})
	}
	if err := h.store.UpsertClientDevices(r.Context(), store.Client{
		ClientID: req.ClientID, Version: req.AgentVersion, BaseURL: req.BaseURL,
	}, devs); err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	// 进行中任务 → 续租 signal(§8.2);未知任务(如 Runtime 已重启丢内存)忽略,
	// 租约过期由 workflow 的 on_infra_error 兜底
	for _, tid := range req.ActiveTaskIDs {
		row, err := h.store.GetTask(r.Context(), tid)
		if err != nil || row == nil {
			continue
		}
		if err := h.signaler.SignalWorkflow(r.Context(), row.WorkflowID, "",
			wf.SignalTaskHeartbeat, wf.TaskHeartbeat{TaskID: tid}); err != nil {
			h.log.Error().Err(err).Str("task_id", tid).Msg("heartbeat signal failed")
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ---- task-events ----

type taskEventReq struct {
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Seq            int    `json:"seq"`
	From           string `json:"from"`
	To             string `json:"to"`
	TS             string `json:"ts"`
	Detail         string `json:"detail"`
}

func (h *Handler) taskEvent(w http.ResponseWriter, r *http.Request) {
	var ev taskEventReq
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil || ev.TaskID == "" || ev.Seq < 1 || ev.To == "" {
		writeErr(w, http.StatusBadRequest, "bad_event", "invalid task event")
		return
	}
	ins, err := h.store.AppendTaskEvent(r.Context(), store.TaskEvent{
		TaskID: ev.TaskID, Seq: ev.Seq, From: ev.From, To: ev.To,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if ins { // 重发事件去重后无副作用(§8.2)
		if err := h.store.SetTaskStatus(r.Context(), ev.TaskID, ev.To); err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ---- results ----

type resultReq struct {
	TaskID         string          `json:"task_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Result         json.RawMessage `json:"result"`
}

// resultDoc 是 result.json v1 中 Runtime 消费的字段子集(校验后解析)。
type resultDoc struct {
	Status      string  `json:"status"`
	ExitCode    int     `json:"exit_code"`
	DurationSec float64 `json:"duration_sec"`
	Cases       struct {
		Total  int `json:"total"`
		Failed int `json:"failed"`
	} `json:"cases"`
	SignaturesHit []string           `json:"signatures_hit"`
	Metrics       map[string]float64 `json:"metrics"`
	Attachments   []wf.Attachment    `json:"attachments"`
}

func (h *Handler) result(w http.ResponseWriter, r *http.Request) {
	var req resultReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TaskID == "" || len(req.Result) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_result", "invalid result report")
		return
	}
	// 红线 §14:未经 Schema 校验不消费 result.json
	var doc any
	if err := json.Unmarshal(req.Result, &doc); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_result", "result is not JSON")
		return
	}
	if err := resultSchema.Validate(doc); err != nil {
		writeErr(w, http.StatusBadRequest, "schema_violation", fmt.Sprintf("result.schema.json: %v", err))
		return
	}
	row, err := h.store.GetTask(r.Context(), req.TaskID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if row == nil {
		writeErr(w, http.StatusBadRequest, "unknown_task", "no such task: "+req.TaskID)
		return
	}
	var parsed resultDoc
	if err := json.Unmarshal(req.Result, &parsed); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_result", err.Error())
		return
	}
	sig := wf.TaskResultSignal{
		TaskID: req.TaskID, Status: parsed.Status, ExitCode: parsed.ExitCode,
		DurationSec: parsed.DurationSec, CasesTotal: parsed.Cases.Total,
		CasesFailed: parsed.Cases.Failed, SignaturesHit: parsed.SignaturesHit,
		Metrics: parsed.Metrics, Attachments: parsed.Attachments,
	}
	// 先落库去重再 signal:重发不重投("signal 只投递一次",§8.2)。
	// 落库成功但 signal 失败的窗口由租约过期 → on_infra_error 兜底收敛。
	ins, err := h.store.SaveResult(r.Context(), wf.ResultRecord{TaskID: req.TaskID, Result: sig})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if ins {
		if err := h.signaler.SignalWorkflow(r.Context(), row.WorkflowID, "",
			wf.SignalTaskResult, sig); err != nil {
			h.log.Error().Err(err).Str("task_id", req.TaskID).Msg("result signal failed")
			writeErr(w, http.StatusInternalServerError, "signal_error", err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/callbacks/ -v && go test ./... 2>&1 | tail -10`
Expected: callbacks 5 个测试 PASS,全仓无回归。

- [ ] **Step 6: Commit**

```bash
git add runtime/internal/callbacks/ contracts/callbacks-api.openapi.yaml
git commit -m "feat(runtime): callbacks service validates, dedups and signals workflow"
```

---

### Task 6: cmd/worker 主程序(Temporal worker + 回调 HTTP 同进程)

**Files:**
- Create: `runtime/cmd/worker/main.go`

**Interfaces:**
- Consumes: Task 1–5 的全部产出 + `wf.DeviceTestWorkflow` / `wf.DeviceTestWorkflowName`。
- Produces: 可执行 `worker`。**设计说明:** 回调 HTTP 与 Temporal worker 同进程,共享同一 store 实例——MemStore 无法跨进程共享,Postgres 落地前这是唯一正确形态;PG 之后也保持单进程(部署简单),届时 §5 的 cmd/api 再拆。

- [ ] **Step 1: 写 main**

```go
// worker — Phase 1.6 Runtime Worker(CLAUDE.md §12.6)。
// 同进程承载:Temporal worker(DeviceTestWorkflow + 全部活动)与
// Client 回调 HTTP 服务(/callbacks/v1/*,与 worker 共享 store 与 temporal client)。
//
// 配置(环境变量):
//
//	TEMPORAL_ADDRESS      缺省 127.0.0.1:7233
//	TEMPORAL_TASK_QUEUE   缺省 device-test
//	CALLBACKS_ADDR        回调服务监听地址,缺省 :8091
//	CALLBACK_BASE_URL     派单时告知 Client 的回调基地址(必填,如 https://runtime:8091)
//	VARIANTS_CONFIG       variants.yaml 路径(必填,与 ci/variants.yaml 同构)
//	ARTIFACT_AUTH_TYPE    bearer|job_token,缺省 bearer
//	ARTIFACT_AUTH_TOKEN   下载产物的凭据(必填)
//	FEISHU_WEBHOOK_URL    飞书机器人 webhook(可选,为空只记日志)
//	LEASE_SECONDS         任务租约,缺省 120(§10)
//	QUARANTINE_AFTER      连续 INFRA 隔离阈值,缺省 3(§10)
//	DATABASE_URL          预留;Postgres 持久化落地前使用内存 store(仅开发)
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"hermes-devops/runtime/internal/activity"
	"hermes-devops/runtime/internal/callbacks"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"

	sdkworkflow "go.temporal.io/sdk/workflow"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(log zerolog.Logger, key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatal().Str("key", key).Str("value", v).Msg("整数环境变量非法")
	}
	return n
}

func main() {
	zerolog.TimeFieldFormat = "2006-01-02T15:04:05.000Z07:00" // UTC + 毫秒(§4)
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC() }
	log := zerolog.New(os.Stderr).With().Timestamp().Str("service", "worker").Logger()

	variantsPath := os.Getenv("VARIANTS_CONFIG")
	callbackBase := os.Getenv("CALLBACK_BASE_URL")
	authToken := os.Getenv("ARTIFACT_AUTH_TOKEN")
	if variantsPath == "" || callbackBase == "" || authToken == "" {
		log.Fatal().Msg("VARIANTS_CONFIG / CALLBACK_BASE_URL / ARTIFACT_AUTH_TOKEN 必填")
	}

	specCfg, err := activity.LoadSpecConfig(variantsPath, activity.SpecDefaults{
		MaxInfraRetries:   2,
		LeaseSeconds:      envInt(log, "LEASE_SECONDS", 120),
		HardTimeoutMargin: 1200,
		DeviceWaitRounds:  20,
		DeviceWaitSeconds: 30,
	})
	if err != nil {
		log.Fatal().Err(err).Msg("load variants config")
	}

	if os.Getenv("DATABASE_URL") != "" {
		log.Warn().Msg("DATABASE_URL 已设置但 PG 持久化尚未落地,仍使用内存 store")
	}
	st := store.NewMemStore()
	log.Warn().Msg("使用内存 store:任务/设备登记不持久,重启后靠心跳重建(仅开发)")

	tc, err := client.Dial(client.Options{HostPort: env("TEMPORAL_ADDRESS", "127.0.0.1:7233")})
	if err != nil {
		log.Fatal().Err(err).Msg("dial temporal")
	}
	defer tc.Close()

	acts := &activity.Acts{
		Store: st,
		Cfg: activity.Config{
			LeaseSeconds:      envInt(log, "LEASE_SECONDS", 120),
			QuarantineAfter:   envInt(log, "QUARANTINE_AFTER", 3),
			CallbackBaseURL:   callbackBase,
			ArtifactAuthType:  env("ARTIFACT_AUTH_TYPE", "bearer"),
			ArtifactAuthToken: authToken,
			FeishuWebhookURL:  os.Getenv("FEISHU_WEBHOOK_URL"),
		},
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		SpecCfg: specCfg,
	}

	w := worker.New(tc, env("TEMPORAL_TASK_QUEUE", "device-test"), worker.Options{})
	w.RegisterWorkflowWithOptions(wf.DeviceTestWorkflow,
		sdkworkflow.RegisterOptions{Name: wf.DeviceTestWorkflowName})
	w.RegisterActivity(acts) // 按方法名注册:SelectTestSpecs/AcquireDevice/...

	cb := callbacks.New(st, tc, &log)
	srv := &http.Server{
		Addr:              env("CALLBACKS_ADDR", ":8091"),
		Handler:           cb.Mux(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info().Str("addr", srv.Addr).Msg("callbacks service listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("callbacks serve")
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info().Msg("worker starting")
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatal().Err(err).Msg("worker run")
	}
	log.Info().Msg("worker stopped")
}
```

- [ ] **Step 2: 构建 + 静态检查**

Run: `go build ./... && go vet ./...`
Expected: 无错误。(main 是纯装配,行为由 Task 7 端到端测试覆盖。)

- [ ] **Step 3: Commit**

```bash
git add runtime/cmd/worker/
git commit -m "feat(runtime): worker binary hosting Temporal worker and callbacks service"
```

---

### Task 7: 端到端集成测试(temporal dev server + 假 Agent)

**Files:**
- Create: `runtime/internal/e2e/e2e_test.go`
- Create: `runtime/internal/e2e/testdata/variants.yaml`(内容与 Task 1 的 testdata 相同,原样复制)

**Interfaces:**
- Consumes: 全部前序产出 + `testtemporal.StartDevServer`(已存在,temporal CLI 不在 PATH 时自动 skip)。

**测试剧本:** 起 dev server 与真 worker(MemStore + 真活动);假 Agent 用 httptest:收到派单 202 后异步向回调服务 POST 一条 RUNNING 事件和 COMPLETED 结果;假飞书 webhook 捕获通知文本。断言 workflow 返回 PASSED 且通知含变体名。

- [ ] **Step 1: 写失败测试**

`runtime/internal/e2e/e2e_test.go`:

```go
// Package e2e 端到端串联 Phase 1.6 全链路:
// ExecuteWorkflow → SelectTestSpecs → AcquireDevice → Dispatch(假 Agent)
// → 回调(事件/结果 → signal)→ 规则引擎 verdict → ReleaseDevice → Notify(假飞书)。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"hermes-devops/runtime/internal/activity"
	"hermes-devops/runtime/internal/callbacks"
	"hermes-devops/runtime/internal/store"
	"hermes-devops/runtime/internal/testtemporal"
	wf "hermes-devops/runtime/internal/workflow"
)

func postJSON(t *testing.T, url string, body any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Errorf("post %s: %v", url, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("post %s: status %d", url, resp.StatusCode)
	}
}

func TestEndToEndPassedVerdict(t *testing.T) {
	addr := testtemporal.StartDevServer(t) // temporal CLI 缺失时 skip

	tc, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()

	st := store.NewMemStore()

	// ---- 回调服务(真 handler,假 Agent 会往这里回报) ----
	cb := callbacks.New(st, tc, nil)
	cbSrv := httptest.NewServer(cb.Mux())
	defer cbSrv.Close()

	// ---- 假飞书 ----
	var notified atomic.Value
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		notified.Store(p.Content.Text)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer feishu.Close()

	// ---- 假 Agent:202 受理,随后异步回报 事件 → 结果 ----
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var d struct {
			TaskID string `json:"task_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&d)
		w.WriteHeader(http.StatusAccepted)
		go func() {
			time.Sleep(50 * time.Millisecond)
			postJSON(t, cbSrv.URL+"/callbacks/v1/task-events", map[string]any{
				"task_id": d.TaskID, "idempotency_key": d.TaskID, "seq": 1,
				"from": "ACCEPTED", "to": "RUNNING", "ts": "2026-07-17T08:00:01.000Z"})
			postJSON(t, cbSrv.URL+"/callbacks/v1/results", map[string]any{
				"task_id": d.TaskID, "idempotency_key": d.TaskID,
				"result": map[string]any{
					"result_version": 1, "task_id": d.TaskID, "attempt": 1,
					"status": "COMPLETED", "exit_code": 0, "duration_sec": 1.5,
					"cases": map[string]any{"total": 3, "passed": 3, "failed": 0, "skipped": 0},
				}})
		}()
	}))
	defer agent.Close()

	// ---- 设备经心跳注册(base_url 指向假 Agent) ----
	postJSON(t, cbSrv.URL+"/callbacks/v1/heartbeat", map[string]any{
		"client_id": "c1", "agent_version": "0.1.0", "base_url": agent.URL,
		"ts": "2026-07-17T08:00:00.000Z",
		"devices": []map[string]any{{"serial": "513cd3de", "state": "IDLE",
			"props": map[string]any{"soc": "QCM6125", "abi": "arm64-v8a", "capabilities": []string{"hexagon"}}}},
		"active_task_ids": []string{}})

	// ---- 真 worker ----
	specCfg, err := activity.LoadSpecConfig("testdata/variants.yaml", activity.SpecDefaults{
		MaxInfraRetries: 2, LeaseSeconds: 120, HardTimeoutMargin: 1200,
		DeviceWaitRounds: 3, DeviceWaitSeconds: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	acts := &activity.Acts{
		Store: st,
		Cfg: activity.Config{LeaseSeconds: 120, QuarantineAfter: 3,
			CallbackBaseURL: cbSrv.URL, ArtifactAuthType: "bearer", ArtifactAuthToken: "tok",
			FeishuWebhookURL: feishu.URL},
		HTTP: &http.Client{Timeout: 10 * time.Second}, SpecCfg: specCfg,
	}
	w := worker.New(tc, "device-test", worker.Options{})
	w.RegisterWorkflowWithOptions(wf.DeviceTestWorkflow,
		sdkworkflow.RegisterOptions{Name: wf.DeviceTestWorkflowName})
	w.RegisterActivity(acts)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	// ---- 触发并断言 ----
	in := wf.DeviceTestInput{Project: "algo-super-sdk", Commit: "abc1234", PipelineID: 7, Version: "1.2.3",
		Packages: []wf.PackageRef{{Variant: "aarch64_Android_SNPE_2.21",
			URL: "https://gitlab/pkg", SHA256: "aa", ManifestDigest: "dd"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	run, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: in.WorkflowID(), TaskQueue: "device-test",
	}, wf.DeviceTestWorkflowName, in)
	if err != nil {
		t.Fatal(err)
	}
	var out wf.DeviceTestOutput
	if err := run.Get(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].Verdict != "PASSED" {
		t.Fatalf("out = %+v", out)
	}
	// 设备归还
	if l, _ := st.AcquireDevice(context.Background(), wf.DeviceSelector{}, "post", 120); l == nil {
		t.Error("workflow 结束后设备应已释放")
	}
	// 飞书通知已发且含变体与 verdict
	text, _ := notified.Load().(string)
	if text == "" || !bytes.Contains([]byte(text), []byte("aarch64_Android_SNPE_2.21")) {
		t.Errorf("notify text = %q", text)
	}
}
```

- [ ] **Step 2: 复制 testdata 并跑测试确认失败(或首次即过则人工核对剧本覆盖点)**

```bash
cp runtime/internal/activity/testdata/variants.yaml runtime/internal/e2e/testdata/variants.yaml
```

Run: `go test ./internal/e2e/ -v -count=1 -timeout 120s`
Expected: 前序任务全部正确时本测试直接 PASS——它是验收测试,不是 TDD 红测试;若 FAIL,按失败信息回修对应任务(常见:活动注册名不匹配、signal 名不匹配、回调 400)。

- [ ] **Step 3: 全仓回归**

Run: `go test ./... -count=1 2>&1 | tail -12 && gofmt -l internal/ cmd/`
Expected: 全部 ok,gofmt 无输出。

- [ ] **Step 4: Commit**

```bash
git add runtime/internal/e2e/
git commit -m "test(runtime): end-to-end device test loop with fake agent and feishu"
```

---

## 后续(不在本计划内)

- **Postgres 持久化**:schema.sql 增补 clients/devices/device_leases/tasks/task_events/results 表,PGStore 实现 activity.Store + callbacks.Store,集成测试沿用 TEST_DATABASE_URL 门控——另立计划。
- **Phase 1.7**:agent-cli 套 RPC 壳 + 心跳/事件/结果回调客户端 + MinIO 预签名直传 + Windows Service 化(Client 侧,对接本计划的回调服务)。
