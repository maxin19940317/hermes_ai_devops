# 预签名 URL 按需签发 实施计划（差距 #8）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把预签名 PUT URL 从派单时一次性签发改为收集完成后按需签发，同时让 `collect` 的 glob 命中文件第一次能进 MinIO。

**Architecture:** callbacks 新增 `POST /callbacks/v1/upload-requests`，用差距 #15 的租约凭据鉴权，只签发 `runs/{task_id}/` 前缀下的键。派单载荷保留 `presigned_uploads[]` 作滚动升级与端点不可达时的回退。签名能力从 `activity` 抽成共享包供两处使用。

**Tech Stack:** Go 1.22+、minio-go v7、PostgreSQL 15、OpenAPI 3（契约）。

**设计文档:** `docs/superpowers/specs/2026-07-29-on-demand-presign-design.md`（已批准）

## Global Constraints

- 端点**必须**校验租约凭据（`lease_id` + `lease_generation` + `client_id` + `task_id` + `attempt`）。callbacks 今天无鉴权，而这个端点签发的是写入凭据，不是接收数据——无凭据请求必须拿不到任何 URL。
- 任何签发出的 object key **必须**以 `runs/{task_id}/` 开头。这是安全性质，对全部用例断言。
- 部分拒绝不是错误：合法 key 照签，非法 key 进 `rejected` 并附原因。
- 路径校验：拒绝绝对路径、拒绝含 `..` 的段、拒绝空串；拼接后**再次**确认前缀（防御归一化被绕过）。
- 文件数上限 `UPLOAD_REQUEST_MAX_FILES`（缺省 64）；超限整体 400，**不截断**（截断会让 Agent 以为传全了）。
- 契约只加字段不删字段：`presigned_uploads[]` 保留，新增 `upload_request_url`。本轮**不**标 deprecated——它仍是回退路径的载体。
- 端点不可达（连接失败/5xx/超时）→ 重试 ≤2 次、间隔 3s，然后回退 `presigned_uploads[]`。
- 端点返回 **401 不回退**：租约已非己有，继续上传会污染别人的证据。
- 四种滚动升级组合（新×旧、旧×新、新×新、旧×旧）都不得丢附件。
- Go 错误用 wrapped errors；注释中文；提交信息英文。

**命令速查：**

```bash
export PATH=/tmp/claude-1000/-home-maxin-Code-hermes-ai-devops/c707bee6-56a7-42a6-9968-1c133ec47341/scratchpad/go/bin:$PATH
export GOMODCACHE=/tmp/claude-1000/-home-maxin-Code-hermes-ai-devops/c707bee6-56a7-42a6-9968-1c133ec47341/scratchpad/gomodcache
export GOCACHE=/tmp/claude-1000/-home-maxin-Code-hermes-ai-devops/c707bee6-56a7-42a6-9968-1c133ec47341/scratchpad/gocache
cd runtime && go build ./... && go vet ./... && go test ./...
cd agent   && go build ./... && go vet ./... && go test ./...
cd /home/maxin/Code/hermes_ai_devops && .venv/bin/python -m pytest contracts/tests -q
```

**工具链注意：** 这套 scratchpad 的 `gofmt -w` 在 CJK 注释旁会损坏 ASCII 引号——**不要对含中文注释的文件跑 `gofmt -w`**，用 `gofmt -l` 检查。验证用 `go vet ./...`（`go build` 不编译测试文件）。

---

### Task 1: 把签名能力抽成共享包

**Files:**
- Create: `runtime/internal/presign/presign.go`
- Create: `runtime/internal/presign/presign_test.go`
- Modify: `runtime/internal/activity/presign.go`
- Modify: `runtime/internal/activity/acts.go`（构造 `presign.Config`）

**Interfaces:**
- Consumes: 无
- Produces:
  - `type presign.Config struct { Endpoint, PublicEndpoint, AccessKey, SecretKey, Bucket string; TTL time.Duration }`
  - `func (c Config) Enabled() bool`
  - `type Signer struct{ ... }`
  - `func NewSigner(c Config) (*Signer, error)` — `Enabled()` 为假时返回 `(nil, nil)`
  - `func (s *Signer) PutURL(ctx context.Context, key string) (url string, expiresAt time.Time, err error)`
  - `func (s *Signer) TTL() time.Duration`

理由：`callbacks.Handler` 今天只有 `store/signaler/log/leaseSec`，**完全没有 MinIO 配置**，而签名逻辑挂在 `activity.Config` 的私有方法上。不抽出来，端点写不出来。本任务是**纯重构，不改任何行为**。

- [ ] **Step 1: 写新包的测试**

创建 `runtime/internal/presign/presign_test.go`：

```go
package presign

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"全配齐", Config{Endpoint: "minio:9000", AccessKey: "a", SecretKey: "b"}, true},
		{"缺 endpoint", Config{AccessKey: "a", SecretKey: "b"}, false},
		{"缺 access key", Config{Endpoint: "minio:9000", SecretKey: "b"}, false},
		{"缺 secret key", Config{Endpoint: "minio:9000", AccessKey: "a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 未启用时 NewSigner 返回 (nil, nil):调用方据此判"优雅降级",不是错误(§3.7)。
func TestNewSignerDisabledReturnsNil(t *testing.T) {
	s, err := NewSigner(Config{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if s != nil {
		t.Errorf("signer = %v, want nil", s)
	}
}

// 签名是纯离线操作:用 public endpoint 构造(签名覆盖 Host,事后改写 URL 会失效),
// 即使该 host 在集群内不可达也能签出来。
func TestPutURLUsesPublicEndpoint(t *testing.T) {
	s, err := NewSigner(Config{
		Endpoint: "minio:9000", PublicEndpoint: "http://10.88.118.251:9000",
		AccessKey: "ak", SecretKey: "sk", Bucket: "hermes-evidence", TTL: time.Hour,
	})
	if err != nil || s == nil {
		t.Fatalf("NewSigner: %v (signer=%v)", err, s)
	}
	u, exp, err := s.PutURL(context.Background(), "runs/t1/result.json")
	if err != nil {
		t.Fatalf("PutURL: %v", err)
	}
	if !strings.Contains(u, "10.88.118.251:9000") {
		t.Errorf("URL 应含 public host, got %q", u)
	}
	if !strings.Contains(u, "runs/t1/result.json") {
		t.Errorf("URL 应含 object key, got %q", u)
	}
	if exp.IsZero() || exp.Before(time.Now()) {
		t.Errorf("expiresAt = %v, 应为将来时刻", exp)
	}
}

// PublicEndpoint 为空时退回 Endpoint(仅同 host 可达时正确,与改造前一致)。
func TestPutURLFallsBackToEndpoint(t *testing.T) {
	s, err := NewSigner(Config{
		Endpoint: "minio:9000", AccessKey: "ak", SecretKey: "sk",
		Bucket: "hermes-evidence", TTL: time.Hour,
	})
	if err != nil || s == nil {
		t.Fatalf("NewSigner: %v", err)
	}
	u, _, err := s.PutURL(context.Background(), "runs/t1/a.log")
	if err != nil {
		t.Fatalf("PutURL: %v", err)
	}
	if !strings.Contains(u, "minio:9000") {
		t.Errorf("URL 应含 endpoint host, got %q", u)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/presign/`
Expected: 编译失败 —— 包不存在

- [ ] **Step 3: 写新包**

创建 `runtime/internal/presign/presign.go`。把 `activity/presign.go` 里 `presignEnabled` 与
`presignClient` 的逻辑原样搬过来（含那段解释"为什么必须用 public endpoint 构造"的注释），
不要改变行为：

```go
// Package presign 集中 MinIO 预签名 PUT 的构造与签发(§3.7)。
// 抽成独立包是因为两处需要它:activity(派单时的固定键集)与 callbacks
// (收集时的按需签发,差距 #8),而 callbacks.Handler 本身不持有 MinIO 配置。
package presign

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config 是签名所需的全部配置。
type Config struct {
	Endpoint       string        // 集群内地址,仅在 PublicEndpoint 为空时用于签名
	PublicEndpoint string        // 预签名 URL 的 host,须为 Client 可达
	AccessKey      string
	SecretKey      string
	Bucket         string
	TTL            time.Duration // <=0 时缺省 1h
}

// Enabled:endpoint 或凭据缺失即禁用(优雅降级,非错误,§3.7)。
func (c Config) Enabled() bool {
	return c.Endpoint != "" && c.AccessKey != "" && c.SecretKey != ""
}

// Signer 签发预签名 PUT URL。
type Signer struct {
	cli    *minio.Client
	bucket string
	ttl    time.Duration
}

// NewSigner 构造签发器。未启用时返回 (nil, nil)——调用方据此判定"降级",
// 这不是错误(§3.7:MinIO 缺失时附件留本地,结果照常回流)。
func NewSigner(c Config) (*Signer, error) {
	if !c.Enabled() {
		return nil, nil
	}
	// AWS V4 签名覆盖 Host 头,因此 client 必须用 PublicEndpoint 的 host 构造——
	// 预签名是纯离线操作(不发起网络请求),集群内不可达的 public host 不影响签名;
	// 事后改写 URL host 会使签名失效,不可取。PublicEndpoint 为空时退回 Endpoint
	// (仅同 host 可达时正确)。
	endpoint := c.PublicEndpoint
	if endpoint == "" {
		endpoint = c.Endpoint
	}
	secure := false
	host := endpoint
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		host = u.Host
		secure = u.Scheme == "https"
	}
	cli, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, fmt.Errorf("presign: 构造 minio client 失败: %w", err)
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &Signer{cli: cli, bucket: c.Bucket, ttl: ttl}, nil
}

// TTL 返回签发使用的有效期。
func (s *Signer) TTL() time.Duration { return s.ttl }

// PutURL 为单个 object key 签发 PUT URL,并返回其过期时刻。
// URL 含签名,调用方**不得**落日志(只记 object key)。
func (s *Signer) PutURL(ctx context.Context, key string) (string, time.Time, error) {
	u, err := s.cli.PresignedPutObject(ctx, s.bucket, key, s.ttl)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign: 签发 %s 失败: %w", key, err)
	}
	return u.String(), time.Now().UTC().Add(s.ttl), nil
}
```

- [ ] **Step 4: 让 activity 改用新包**

编辑 `runtime/internal/activity/presign.go`：删除 `presignClient`，把 `presignEnabled` 改为
委托新包，`presignedUploads` 改用 `Signer`。保持**行为与日志文案完全不变**：

```go
// presignEnabled:endpoint 或凭据缺失即禁用(优雅降级,非错误,§3.7)。
func (c Config) presignEnabled() bool { return c.presignConfig().Enabled() }

// presignConfig 把 activity 配置投影成签名包的配置。
func (c Config) presignConfig() presign.Config {
	return presign.Config{
		Endpoint: c.MinIOEndpoint, PublicEndpoint: c.MinIOPublicEndpoint,
		AccessKey: c.MinIOAccessKey, SecretKey: c.MinIOSecretKey,
		Bucket: c.MinIOBucket, TTL: c.MinIOPresignTTL,
	}
}
```

`presignedUploads` 内部把 `presignClient(a.Cfg)` 换成 `presign.NewSigner(a.Cfg.presignConfig())`，
把 `cli.PresignedPutObject(...)` 换成 `signer.PutURL(ctx, key)`，其余（含降级返回空集、
警告文案）一字不改。

注意 `evidenceClient`（`analyze.go` 里读 MinIO 用的那个）**不要动**：它用集群内 endpoint 发真实
网络请求，与签名是两回事。

- [ ] **Step 5: 跑测试**

Run: `cd runtime && go vet ./... && go test ./internal/presign/ ./internal/activity/ -v 2>&1 | tail -20`
Expected: 新包用例全 PASS，`activity` 既有用例全 PASS 且**无一条需要修改**（纯重构的判据）

- [ ] **Step 6: Commit**

```bash
git add runtime/internal/presign/ runtime/internal/activity/
git commit -m "refactor(runtime): extract presign signer into a shared package"
```

---

### Task 2: 只读租约校验

**Files:**
- Modify: `runtime/internal/store/devices.go`（MemStore）
- Modify: `runtime/internal/store/postgres_devices.go`（PGStore）
- Modify: `runtime/internal/store/conformance_test.go`

**Interfaces:**
- Consumes: 无
- Produces: `VerifyLease(ctx context.Context, cred LeaseCredential) (bool, error)` — 两套实现

理由：`RenewLease` 会**续租**（写 `lease_expires_at`）。签发端点需要的是"这个凭据现在是不是该任务的持有者"，不应带续租副作用——签一次 URL 不等于任务还活着。

- [ ] **Step 1: 写失败的 conformance 子测试**

先把 `fullStore` 接口加上：

```go
	VerifyLease(ctx context.Context, cred LeaseCredential) (bool, error)
```

再在 `runConformance` 内追加：

```go
	// 只读租约校验(差距 #8 的签发端点鉴权依据):校验通过不得有任何副作用,
	// 尤其不得像 RenewLease 那样续期——签一次 URL 不等于任务还活着。
	t.Run("VerifyLeaseIsReadOnly", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		cred := LeaseCredential{
			DeviceID: lease.DeviceID, ClientID: lease.ClientID, TaskID: "w:t1:a1",
			Attempt: 1, LeaseID: lease.LeaseID, Generation: lease.Generation,
		}
		before, err := s.GetLeaseExpiry(ctx, "w:t1:a1")
		if err != nil || before == nil {
			t.Fatalf("expiry before: %v %v", before, err)
		}
		ok, err := s.VerifyLease(ctx, cred)
		if err != nil || !ok {
			t.Fatalf("VerifyLease = %v, %v; want true, nil", ok, err)
		}
		after, err := s.GetLeaseExpiry(ctx, "w:t1:a1")
		if err != nil || after == nil {
			t.Fatalf("expiry after: %v %v", after, err)
		}
		if !after.Equal(*before) {
			t.Errorf("校验不得续期: %v → %v", before, after)
		}
	})

	// 凭据任一项失配都必须判否——这是端点唯一的鉴权依据。
	t.Run("VerifyLeaseRejectsMismatch", func(t *testing.T) {
		base := func(l *wf.Lease) LeaseCredential {
			return LeaseCredential{
				DeviceID: l.DeviceID, ClientID: l.ClientID, TaskID: "w:t1:a1",
				Attempt: 1, LeaseID: l.LeaseID, Generation: l.Generation,
			}
		}
		cases := []struct {
			name  string
			mutate func(c *LeaseCredential)
		}{
			{"错 lease_id", func(c *LeaseCredential) { c.LeaseID = "bogus" }},
			{"错 generation", func(c *LeaseCredential) { c.Generation += 1 }},
			{"错 client_id", func(c *LeaseCredential) { c.ClientID = "other" }},
			{"错 task_id", func(c *LeaseCredential) { c.TaskID = "w:other:a1" }},
			{"错 device_id", func(c *LeaseCredential) { c.DeviceID = "no-such-device" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seed(t, s)
				lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
				if err != nil || lease == nil {
					t.Fatalf("acquire: %+v %v", lease, err)
				}
				cred := base(lease)
				tc.mutate(&cred)
				ok, err := s.VerifyLease(ctx, cred)
				if err != nil {
					t.Fatalf("VerifyLease err = %v", err)
				}
				if ok {
					t.Errorf("%s 应判否", tc.name)
				}
			})
		}
	})

	// 已释放的租约不再是持有者(任务结束后不得继续换 URL)。
	t.Run("VerifyLeaseRejectsReleased", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		cred := LeaseCredential{
			DeviceID: lease.DeviceID, ClientID: lease.ClientID, TaskID: "w:t1:a1",
			Attempt: 1, LeaseID: lease.LeaseID, Generation: lease.Generation,
		}
		if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:t1:a1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		ok, err := s.VerifyLease(ctx, cred)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("已释放的租约不得通过校验")
		}
	})
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/store/ -run Conformance/VerifyLease`
Expected: 编译失败 —— `VerifyLease` 未定义

- [ ] **Step 3: MemStore 实现**

`runtime/internal/store/devices.go`，紧邻 `RenewLease` 加：

```go
// VerifyLease 只读校验凭据是否为该任务当前的租约持有者(差距 #8 的签发端点鉴权)。
// 与 RenewLease 的区别:**不续期**——签发一次 URL 不构成"任务仍然活着"的证据,
// 续期只能由心跳做。校验项与 RenewLease 完全一致(device/client/task/lease_id/
// generation 全匹配且未释放),失配返回 (false, nil) 而非错误。
func (s *MemStore) VerifyLease(_ context.Context, cred LeaseCredential) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.devices[cred.DeviceID]
	if !ok || row.Released {
		return false, nil
	}
	if row.ClientID != cred.ClientID || row.LeaseTaskID != cred.TaskID ||
		row.LeaseID != cred.LeaseID || row.LeaseGeneration != cred.Generation {
		return false, nil
	}
	return true, nil
}
```

- [ ] **Step 4: PGStore 实现**

`runtime/internal/store/postgres_devices.go`，紧邻 `RenewLease` 加：

```go
// VerifyLease 见 MemStore 同名方法的语义说明(差距 #8)。纯 SELECT,无副作用。
func (s *PGStore) VerifyLease(ctx context.Context, cred LeaseCredential) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `
		SELECT count(*)
		FROM device_leases l JOIN devices d ON d.device_id = l.device_id
		WHERE l.device_id = $1 AND d.client_id = $2 AND l.task_id = $3
		  AND l.lease_id = $4 AND l.lease_generation = $5
		  AND l.released_at IS NULL`,
		cred.DeviceID, cred.ClientID, cred.TaskID, cred.LeaseID, cred.Generation).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("verify lease %s: %w", cred.TaskID, err)
	}
	return n == 1, nil
}
```

- [ ] **Step 5: 跑测试**

Run: `cd runtime && go test ./internal/store/ -run Conformance/VerifyLease -v 2>&1 | tail -20`
Expected: 两套实现全 PASS（PGStore 走内嵌 Postgres，不跳过）

- [ ] **Step 6: 验证鉴权测试真的会红**

把 `MemStore.VerifyLease` 的凭据比较临时改成 `return true, nil`（模拟鉴权失效），跑：

Run: `cd runtime && go test ./internal/store/ -run Conformance/VerifyLeaseRejects`
Expected: **FAIL**，五个失配用例与已释放用例都报错。改回来确认恢复 PASS，两次输出记进报告。

- [ ] **Step 7: Commit**

```bash
git add runtime/internal/store/
git commit -m "feat(store): add read-only lease verification"
```

---

### Task 3: 契约

**Files:**
- Modify: `contracts/callbacks-api.openapi.yaml`
- Modify: `contracts/client-agent-api.openapi.yaml`
- Modify: `contracts/tests/test_openapi.py`

**Interfaces:**
- Consumes: 无
- Produces: `POST /callbacks/v1/upload-requests` 的请求/响应 schema；`TaskDispatchRequest.upload_request_url`

- [ ] **Step 1: 写失败的契约测试**

`contracts/tests/test_openapi.py` 追加：

```python
def test_upload_requests_endpoint_shape():
    """按需签发端点(差距 #8):请求必须带全套租约凭据,响应区分 uploads 与 rejected。"""
    spec = _load_callbacks_spec()
    op = spec["paths"]["/callbacks/v1/upload-requests"]["post"]
    req = _resolve_local_refs(
        op["requestBody"]["content"]["application/json"]["schema"], spec)
    # 鉴权靠凭据,少一项都不行——这是端点唯一的门禁
    for field in ("task_id", "client_id", "device_id", "attempt",
                  "lease_id", "lease_generation", "files"):
        assert field in req["required"], f"{field} 必须是必填(端点鉴权依据)"
    resp = _resolve_local_refs(
        op["responses"]["200"]["content"]["application/json"]["schema"], spec)
    assert "uploads" in resp["properties"]
    assert "rejected" in resp["properties"], "部分拒绝不是错误,必须单列"
    # 401 是租约失配的唯一出口,Agent 据此决定不回退
    assert "401" in op["responses"]


def test_dispatch_keeps_presigned_uploads_and_adds_endpoint():
    """契约只加不删:upload_request_url 新增,presigned_uploads 保留作回退。"""
    spec = _load_agent_spec()
    props = spec["components"]["schemas"]["TaskDispatchRequest"]["properties"]
    assert "upload_request_url" in props, "按需签发端点地址(差距 #8)"
    assert "presigned_uploads" in props, "回退路径的载体,本轮不得移除"
```

`_load_agent_spec` 若不存在，照 `_load_callbacks_spec` 的写法加一个，读
`contracts/client-agent-api.openapi.yaml`。

- [ ] **Step 2: 跑测试确认失败**

Run: `.venv/bin/python -m pytest contracts/tests/test_openapi.py -q -k upload_requests`
Expected: FAIL —— 路径 `/callbacks/v1/upload-requests` 不存在

- [ ] **Step 3: 写 callbacks 契约**

`contracts/callbacks-api.openapi.yaml` 的 `paths` 下追加：

```yaml
  /callbacks/v1/upload-requests:
    post:
      operationId: requestUploads
      summary: 按需签发附件上传 URL(差距 #8)
      description: |
        Agent 在收集完成后用本端点换取本次实际收集到的文件的预签名 PUT URL。
        与派单时一次性签发相比:URL 在上传前秒级签发,不再有 TTL 过期问题;
        且 collect 的 glob(logs/*.log、dumps/**)命中的文件第一次能被上传。

        **鉴权**:必须携带派单时下发的租约所有权凭据(lease_id/lease_generation)。
        callbacks 其余端点是接收数据,本端点签发的是写入凭据——无凭据请求
        必须拿不到任何 URL。签发的 object key 一律限定在 runs/{task_id}/ 前缀内。
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/UploadRequest' }
      responses:
        '200':
          description: 已签发(可能部分拒绝)
          content:
            application/json:
              schema: { $ref: '#/components/schemas/UploadRequestResult' }
        '400': { description: 请求形态非法(缺字段/files 为空/超上限) }
        '401': { description: 租约凭据校验不通过;Agent 不得回退,应放弃上传 }
        '503': { description: MinIO 未配置或签名失败;Agent 可回退 presigned_uploads }
```

`components.schemas` 下追加：

```yaml
    UploadRequest:
      type: object
      additionalProperties: false
      required: [task_id, client_id, device_id, attempt, lease_id, lease_generation, files]
      properties:
        task_id: { type: string, minLength: 1 }
        client_id: { type: string, minLength: 1 }
        device_id: { type: string, minLength: 1 }
        attempt: { type: integer, minimum: 1 }
        lease_id: { type: string, minLength: 1 }
        lease_generation: { type: integer, minimum: 1 }
        files:
          type: array
          minItems: 1
          description: out_dir 内的相对路径;绝对路径、含 .. 的段一律被拒。
          items: { type: string, minLength: 1 }
    UploadRequestResult:
      type: object
      required: [uploads, rejected]
      properties:
        uploads:
          type: array
          items:
            type: object
            required: [path, object_key, url, expires_at]
            properties:
              path: { type: string }
              object_key: { type: string }
              url: { type: string }
              expires_at: { type: string, format: date-time }
        rejected:
          type: array
          description: 非法路径逐项返回原因;部分拒绝不是错误,其余照签。
          items:
            type: object
            required: [path, reason]
            properties:
              path: { type: string }
              reason: { type: string }
```

- [ ] **Step 4: 写 agent 契约**

`contracts/client-agent-api.openapi.yaml` 的 `TaskDispatchRequest.properties` 追加：

```yaml
        upload_request_url:
          type: string
          description: |
            按需签发端点的完整 URL(差距 #8),形如
            {callback_base_url}/callbacks/v1/upload-requests。
            Agent 收集完成后用它换取本次实际收集到的文件的预签名 PUT URL。
            为空 = Runtime 未启用按需签发,Agent 沿用 presigned_uploads[]。
```

并在既有 `presigned_uploads` 的 description 末尾补一句：

```
            新 Agent 仅在按需签发不可用(upload_request_url 为空或端点不可达)时用它兜底;
            本字段是回退路径的载体,不得移除。
```

- [ ] **Step 5: 跑测试**

Run: `.venv/bin/python -m pytest contracts/tests -q`
Expected: 全部 PASS

- [ ] **Step 6: Commit**

```bash
git add contracts/
git commit -m "feat(contracts): add upload-requests endpoint and dispatch endpoint field"
```

---

### Task 4: Runtime 端点与派单字段

**Files:**
- Create: `runtime/internal/callbacks/uploads.go`
- Create: `runtime/internal/callbacks/uploads_test.go`
- Modify: `runtime/internal/callbacks/handler.go`（Store 接口 + Mux + 新字段）
- Modify: `runtime/internal/activity/dispatch.go`（载荷加 `upload_request_url`）
- Modify: `runtime/cmd/worker/main.go`（装配 Signer 与上限）
- Modify: `runtime/cmd/worker/config.go` + `config_test.go`（`UPLOAD_REQUEST_MAX_FILES`）

**Interfaces:**
- Consumes: Task 1 的 `presign.Signer`、Task 2 的 `store.VerifyLease`、Task 3 的契约
- Produces: `POST /callbacks/v1/upload-requests` 的服务端实现

- [ ] **Step 1: 写失败的 handler 测试**

创建 `runtime/internal/callbacks/uploads_test.go`。用 `store.MemStore` 造一个真实租约，
沿用该包既有测试的 httptest 写法：

```go
package callbacks

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 端点唯一的门禁是租约凭据;凭据不对必须一个 URL 都签不出来。
func TestUploadRequestsRejectsBadLease(t *testing.T) {
	h, cred := newUploadHandler(t)
	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"错 lease_id", func(m map[string]any) { m["lease_id"] = "bogus" }},
		{"错 generation", func(m map[string]any) { m["lease_generation"] = 99 }},
		{"错 client_id", func(m map[string]any) { m["client_id"] = "other" }},
		{"错 task_id", func(m map[string]any) { m["task_id"] = "w:other:a1" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := cred()
			tc.mutate(body)
			rec := post(t, h, body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("code = %d, want 401", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "http") {
				t.Error("401 响应不得包含任何 URL")
			}
		})
	}
}

// 安全性质:签发出的 key 一律在 runs/{task_id}/ 内,路径逃逸进 rejected。
func TestUploadRequestsKeyConfinement(t *testing.T) {
	h, cred := newUploadHandler(t)
	body := cred()
	body["files"] = []string{
		"results/result.json", "dumps/0001.bin",
		"../../etc/passwd", "/etc/shadow", "a/../../b", "",
	}
	rec := post(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var out struct {
		Uploads  []struct{ Path, ObjectKey, URL string }
		Rejected []struct{ Path, Reason string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Uploads) != 2 {
		t.Errorf("uploads = %d, want 2(仅两个合法路径)", len(out.Uploads))
	}
	if len(out.Rejected) != 4 {
		t.Errorf("rejected = %d, want 4", len(out.Rejected))
	}
	prefix := "runs/" + body["task_id"].(string) + "/"
	for _, u := range out.Uploads {
		if !strings.HasPrefix(u.ObjectKey, prefix) {
			t.Errorf("object_key %q 越出前缀 %q", u.ObjectKey, prefix)
		}
	}
}

// 超上限整体拒绝,不截断——截断会让 Agent 以为传全了。
func TestUploadRequestsRejectsTooManyFiles(t *testing.T) {
	h, cred := newUploadHandler(t)
	body := cred()
	files := make([]string, 65)
	for i := range files {
		files[i] = "logs/f.log"
	}
	body["files"] = files
	if rec := post(t, h, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}

// MinIO 未配置 → 503,Agent 据此回退。
func TestUploadRequestsWithoutSignerReturns503(t *testing.T) {
	h, cred := newUploadHandler(t)
	h.Presign = nil
	if rec := post(t, h, cred()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func post(t *testing.T, h *Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/callbacks/v1/upload-requests", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	return rec
}
```

`newUploadHandler(t)` 由你实现：造一个 `store.MemStore`，注册 client+device（照该包既有测试
或 `store` 的 `UpsertClientDevices` 用法），`AcquireDevice` 拿到租约，构造 `Handler`
并设 `h.Presign`（用 Task 1 的 `presign.NewSigner`，配一组假凭据即可——签名是纯离线的，
不需要真 MinIO），`h.UploadMaxFiles = 64`；返回 handler 与一个生成"合法请求体"的闭包。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/callbacks/ -run UploadRequests`
Expected: 编译失败 —— `Handler` 无 `Presign` 字段

- [ ] **Step 3: 扩展 Handler**

`runtime/internal/callbacks/handler.go`：Store 接口加一行

```go
	VerifyLease(ctx context.Context, cred store.LeaseCredential) (bool, error)
```

`Handler` 结构体加两个导出字段（构造后由 `cmd/worker` 装配，与 `Acts` 的字段装配风格一致，
避免改 `New` 的签名波及既有调用点）：

```go
	// Presign 非 nil 时启用按需签发端点(差距 #8);nil = MinIO 未配置,端点返回 503。
	Presign *presign.Signer
	// UploadMaxFiles 是单次请求的文件数上限;<=0 时用 defaultUploadMaxFiles。
	UploadMaxFiles int
```

`Mux()` 加一行：

```go
	mux.HandleFunc("POST /callbacks/v1/upload-requests", h.uploadRequests)
```

- [ ] **Step 4: 实现端点**

创建 `runtime/internal/callbacks/uploads.go`：

```go
package callbacks

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"hermes-devops/runtime/internal/store"
)

// defaultUploadMaxFiles 是单次请求的文件数上限缺省值(差距 #8)。
const defaultUploadMaxFiles = 64

type uploadRequestReq struct {
	TaskID          string   `json:"task_id"`
	ClientID        string   `json:"client_id"`
	DeviceID        string   `json:"device_id"`
	Attempt         int      `json:"attempt"`
	LeaseID         string   `json:"lease_id"`
	LeaseGeneration int      `json:"lease_generation"`
	Files           []string `json:"files"`
}

type uploadItem struct {
	Path      string `json:"path"`
	ObjectKey string `json:"object_key"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

type rejectedItem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// uploadRequests 按需签发附件上传 URL(差距 #8)。
//
// 与本包其他端点的性质不同:它签发的是**写入凭据**而非接收数据,因此必须先校验
// 租约所有权(差距 #15 的凭据)。callbacks 整体今天无鉴权(mTLS 属 Phase 3),
// 若不校验,同网段任何人都能拿猜到的 task_id 换取往证据桶写入的能力。
func (h *Handler) uploadRequests(w http.ResponseWriter, r *http.Request) {
	var req uploadRequestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.TaskID == "" || req.ClientID == "" || req.DeviceID == "" ||
		req.LeaseID == "" || req.Attempt < 1 || req.LeaseGeneration < 1 ||
		len(req.Files) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_upload_request", "invalid upload request payload")
		return
	}
	max := h.UploadMaxFiles
	if max <= 0 {
		max = defaultUploadMaxFiles
	}
	// 超限整体拒绝而非截断:截断会让 Agent 以为传全了。
	if len(req.Files) > max {
		writeErr(w, http.StatusBadRequest, "too_many_files", "files exceeds limit")
		return
	}
	ok, err := h.store.VerifyLease(r.Context(), store.LeaseCredential{
		DeviceID: req.DeviceID, ClientID: req.ClientID, TaskID: req.TaskID,
		Attempt: req.Attempt, LeaseID: req.LeaseID, Generation: req.LeaseGeneration,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !ok {
		// 不泄露任何 URL,也不区分"租约易主"与"任务不存在"——两者对调用方是同一结论。
		h.log.Info().Str("task", req.TaskID).Str("client", req.ClientID).
			Msg("upload request rejected: lease not owned")
		writeErr(w, http.StatusUnauthorized, "lease_not_owned", "lease credential mismatch")
		return
	}
	if h.Presign == nil {
		writeErr(w, http.StatusServiceUnavailable, "presign_disabled", "minio not configured")
		return
	}

	prefix := "runs/" + req.TaskID + "/"
	uploads := []uploadItem{}
	rejected := []rejectedItem{}
	for _, p := range req.Files {
		key, reason := confineKey(prefix, p)
		if reason != "" {
			rejected = append(rejected, rejectedItem{Path: p, Reason: reason})
			continue
		}
		u, exp, err := h.Presign.PutURL(r.Context(), key)
		if err != nil {
			// URL 含签名,永不落日志;只记 object key。
			h.log.Error().Err(err).Str("object_key", key).Msg("presign put failed")
			rejected = append(rejected, rejectedItem{Path: p, Reason: "presign failed"})
			continue
		}
		uploads = append(uploads, uploadItem{
			Path: p, ObjectKey: key, URL: u, ExpiresAt: exp.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploads": uploads, "rejected": rejected})
}

// confineKey 把 out_dir 相对路径拼成 object key,并确认结果不越出 prefix。
// 返回的 reason 非空即表示拒绝。
func confineKey(prefix, rel string) (key, reason string) {
	if rel == "" {
		return "", "empty path"
	}
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return "", "absolute or non-slash path"
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", "path escapes task prefix"
		}
	}
	// path.Clean 之后再验前缀:防御归一化本身被绕过(如 a/../../b)。
	key = path.Clean(prefix + rel)
	if !strings.HasPrefix(key, prefix) {
		return "", "path escapes task prefix"
	}
	return key, ""
}
```

若该包没有 `writeJSON` 辅助，照 `writeErr` 的写法加一个（设 `Content-Type: application/json`，
写入状态码与 body）。

- [ ] **Step 5: 派单载荷加字段**

`runtime/internal/activity/dispatch.go` 的 payload map 加一行（紧邻 `presigned_uploads`）：

```go
		// 按需签发端点(差距 #8);CALLBACK_BASE_URL 为空时也为空,Agent 沿用 presigned_uploads
		"upload_request_url": uploadRequestURL(a.Cfg.CallbackBaseURL),
```

同文件加：

```go
// uploadRequestURL 由回调基址派生按需签发端点地址(差距 #8)。
// 不新增配置项:与 callback_base_url 同一来源,避免两处配置漂移。
func uploadRequestURL(base string) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/callbacks/v1/upload-requests"
}
```

- [ ] **Step 6: 配置与装配**

`runtime/cmd/worker/config.go` 加：

```go
	// 单次按需签发请求的文件数上限(差距 #8)。超限整体拒绝,不截断。
	uploadMaxFiles, err := envInt("UPLOAD_REQUEST_MAX_FILES", 64)
	if err != nil {
		return Config{}, err
	}
```

放进 Activity 配置结构体的同级位置（新增 `UploadMaxFiles int` 字段并赋值）。

`runtime/cmd/worker/config_test.go` 加：

```go
func TestUploadRequestMaxFilesDefault(t *testing.T) {
	cfg, err := loadConfig(lookup(map[string]string{"VARIANTS_CONFIG": "../../ci/variants.yaml"}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Activity.UploadMaxFiles != 64 {
		t.Errorf("UploadMaxFiles = %d, want 64", cfg.Activity.UploadMaxFiles)
	}
}
```

`runtime/cmd/worker/main.go` 在 `cb := callbacks.New(...)` 之后装配：

```go
	// 按需签发(差距 #8):MinIO 未配置时 Presign 为 nil,端点返回 503,Agent 回退。
	if signer, err := presign.NewSigner(presign.Config{
		Endpoint: cfg.Activity.MinIOEndpoint, PublicEndpoint: cfg.Activity.MinIOPublicEndpoint,
		AccessKey: cfg.Activity.MinIOAccessKey, SecretKey: cfg.Activity.MinIOSecretKey,
		Bucket: cfg.Activity.MinIOBucket, TTL: cfg.Activity.MinIOPresignTTL,
	}); err != nil {
		log.Warn().Err(err).Msg("presign signer init failed; upload-requests will return 503")
	} else {
		cb.Presign = signer
	}
	cb.UploadMaxFiles = cfg.Activity.UploadMaxFiles
```

- [ ] **Step 7: 跑测试**

Run: `cd runtime && go vet ./... && go test ./... 2>&1 | grep -v "no test files" | tail -20`
Expected: 全部 PASS

- [ ] **Step 8: 验证前缀约束真的会红**

把 `confineKey` 的最终前缀检查临时删掉（只留 `..` 段检查），跑：

Run: `cd runtime && go test ./internal/callbacks/ -run UploadRequestsKeyConfinement`
Expected: **FAIL**（`a/../../b` 会逃出前缀）。改回来确认恢复 PASS，两次输出记进报告。

- [ ] **Step 9: Commit**

```bash
git add runtime/internal/callbacks/ runtime/internal/activity/dispatch.go runtime/cmd/worker/
git commit -m "feat(runtime): sign upload URLs on demand behind lease verification"
```

---

### Task 5: Agent 收集时换取 URL 并回退

**Files:**
- Modify: `agent/internal/reporter/client.go`（新增 RequestUploads）
- Modify: `agent/internal/server/tasks.go`（dispatch 字段 + 上传流程）
- Modify: `agent/internal/server/dispatch.schema.json`
- Modify: `agent/internal/server/server_test.go`
- Modify: `agent/internal/reporter/client_test.go`

**Interfaces:**
- Consumes: Task 3 的契约、Task 4 的端点
- Produces: Agent 侧按需签发路径与回退

- [ ] **Step 1: 写失败的测试**

`agent/internal/reporter/client_test.go` 追加（httptest 假 Runtime）：

```go
// 401 表示租约已非己有,调用方必须能区分它与"端点挂了"——前者不回退,后者回退。
func TestRequestUploadsDistinguishesUnauthorized(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantUnauth bool
		wantErr    bool
	}{
		{"200 正常", http.StatusOK, false, false},
		{"401 租约失配", http.StatusUnauthorized, true, true},
		{"503 未配置", http.StatusServiceUnavailable, false, true},
		{"500 服务端错", http.StatusInternalServerError, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					_, _ = w.Write([]byte(`{"uploads":[],"rejected":[]}`))
				}
			}))
			defer srv.Close()
			c := NewClient(Config{BaseURL: srv.URL})
			_, err := c.RequestUploads(context.Background(), srv.URL, UploadRequest{
				TaskID: "t1", ClientID: "c1", DeviceID: "d1", Attempt: 1,
				LeaseID: "l1", LeaseGeneration: 1, Files: []string{"a.log"},
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrLeaseNotOwned); got != tc.wantUnauth {
				t.Errorf("ErrLeaseNotOwned = %v, want %v (err=%v)", got, tc.wantUnauth, err)
			}
		})
	}
}
```

`NewClient`/`Config` 用该包既有的构造方式（读文件确认名字）。

`agent/internal/server/server_test.go` 追加三个用例。它们共用一个辅助：在临时 out_dir 里
放三个文件（`device/results/result.json`、`logs/run.log`、`dumps/0001.bin`），起一个假 Runtime，
再用一个记录调用的假 Uploader 观察实际上传了什么。

```go
// fakeUploader 记录每次 Upload 收到的键,便于断言"传了哪些"。
type fakeUploader struct{ gotKeys [][]string }

func (f *fakeUploader) Upload(_ context.Context, p []uploader.PresignedUpload,
	_ map[string]string) []reporter.Attachment {
	keys := make([]string, 0, len(p))
	for _, x := range p {
		keys = append(keys, x.ObjectKey)
	}
	f.gotKeys = append(f.gotKeys, keys)
	return nil
}

// seedOutDir 造出 out_dir 与三个文件,返回目录路径。
func seedOutDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range []string{"device/results/result.json", "logs/run.log", "dumps/0001.bin"} {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// glob 命中的文件(dumps/**、logs/*.log)在按需签发路径下必须能上传——
// 这是关闭 presign.go 那条 CONTRACT-ISSUE 的证据。
func TestUploadAttachmentsOnDemandCoversGlobFiles(t *testing.T) {
	outDir := seedOutDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req reporter.UploadRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		grants := make([]reporter.UploadGrant, 0, len(req.Files))
		for _, p := range req.Files {
			grants = append(grants, reporter.UploadGrant{
				Path: p, ObjectKey: "runs/" + req.TaskID + "/" + p, URL: "http://minio/put"})
		}
		_ = json.NewEncoder(w).Encode(reporter.UploadRequestResult{Uploads: grants})
	}))
	defer srv.Close()

	up := &fakeUploader{}
	s := newTestServer(t, up) // 该文件既有的 server 构造辅助;若无则照既有用例写法构造
	d := Dispatch{TaskID: "t1", UploadRequestURL: srv.URL,
		LeaseID: "l1", LeaseGeneration: 1, DeviceSerial: "d1"}
	s.uploadAttachments(context.Background(), d, outDir)

	if len(up.gotKeys) != 1 {
		t.Fatalf("Upload 调用次数 = %d, want 1", len(up.gotKeys))
	}
	joined := strings.Join(up.gotKeys[0], ",")
	for _, want := range []string{"dumps/0001.bin", "logs/run.log", "device/results/result.json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("按需签发应覆盖 %s, got %v", want, up.gotKeys[0])
		}
	}
}

// 端点不可达 → 回退固定键集,附件不能因此全丢。
func TestUploadAttachmentsFallsBackWhenEndpointDown(t *testing.T) {
	outDir := seedOutDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	up := &fakeUploader{}
	s := newTestServer(t, up)
	d := Dispatch{TaskID: "t1", UploadRequestURL: srv.URL,
		LeaseID: "l1", LeaseGeneration: 1, DeviceSerial: "d1",
		PresignedUploads: fixedSetFor("t1")} // 见下方说明
	s.uploadAttachments(context.Background(), d, outDir)

	if len(up.gotKeys) != 1 {
		t.Fatalf("应回退并上传一次, Upload 调用 = %d", len(up.gotKeys))
	}
	if !strings.Contains(strings.Join(up.gotKeys[0], ","), "runs/t1/result.json") {
		t.Errorf("回退应走固定键集, got %v", up.gotKeys[0])
	}
}

// 401 不回退:租约已非己有,继续上传会污染别人的证据。
func TestUploadAttachmentsDoesNotFallBackOnUnauthorized(t *testing.T) {
	outDir := seedOutDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	up := &fakeUploader{}
	s := newTestServer(t, up)
	d := Dispatch{TaskID: "t1", UploadRequestURL: srv.URL,
		LeaseID: "l1", LeaseGeneration: 1, DeviceSerial: "d1",
		PresignedUploads: fixedSetFor("t1")}
	s.uploadAttachments(context.Background(), d, outDir)

	if len(up.gotKeys) != 0 {
		t.Errorf("401 时不得上传任何东西, got %v", up.gotKeys)
	}
}
```

`fixedSetFor(taskID)` 是你在测试文件里写的小辅助，产出与 `Dispatch.PresignedUploads` 同形
的五条固定键集条目（`runs/{taskID}/result.json` 等，URL 随便填）。`newTestServer` 用该文件
既有的构造方式；若既有用例是内联构造 `&Server{cfg: ...}`，照抄那种写法并把 `Uploader`
换成 `up`。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd agent && go test ./internal/reporter/ -run RequestUploads`
Expected: 编译失败 —— `RequestUploads` / `ErrLeaseNotOwned` 未定义

- [ ] **Step 3: reporter 侧实现**

`agent/internal/reporter/client.go` 追加：

```go
// ErrLeaseNotOwned 标记按需签发端点返回 401:租约已非己有(任务易主或已回收)。
// 调用方**不得**回退到派单时的 URL——继续上传会污染别人的证据。
var ErrLeaseNotOwned = errors.New("reporter: lease not owned")

// UploadRequest 是 POST /callbacks/v1/upload-requests 的请求体(差距 #8)。
type UploadRequest struct {
	TaskID          string   `json:"task_id"`
	ClientID        string   `json:"client_id"`
	DeviceID        string   `json:"device_id"`
	Attempt         int      `json:"attempt"`
	LeaseID         string   `json:"lease_id"`
	LeaseGeneration int      `json:"lease_generation"`
	Files           []string `json:"files"`
}

// UploadGrant 是单个已签发条目。
type UploadGrant struct {
	Path      string `json:"path"`
	ObjectKey string `json:"object_key"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// UploadRequestResult 是端点响应。Rejected 里是被拒的路径与原因(部分拒绝不是错误)。
type UploadRequestResult struct {
	Uploads  []UploadGrant `json:"uploads"`
	Rejected []struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	} `json:"rejected"`
}

// RequestUploads 换取本次实际收集到的文件的预签名 PUT URL(差距 #8)。
// 401 返回 ErrLeaseNotOwned;其余非 2xx 返回普通错误(调用方可回退)。
func (c *Client) RequestUploads(ctx context.Context, endpoint string, req UploadRequest) (*UploadRequestResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("request uploads: encode: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request uploads: build: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("request uploads: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrLeaseNotOwned
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("request uploads: status %d", resp.StatusCode)
	}
	var out UploadRequestResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("request uploads: decode: %w", err)
	}
	return &out, nil
}
```

`c.hc` 用该包既有的 http client 字段名（读文件确认）。

- [ ] **Step 4: server 侧实现**

`agent/internal/server/tasks.go` 的 `Dispatch` 结构体加 `UploadRequestURL string \`json:"upload_request_url"\``，
`dispatch.schema.json` 同步加该字段（`type: string`，非必填）。

把 `uploadAttachments` 改成先试按需、失败回退：

```go
// uploadAttachments 上传收集到的附件。优先按需签发(差距 #8):用 out_dir 内实际
// 存在的文件换 URL,glob 命中的文件(logs/*.log、dumps/**)因此第一次能被上传。
// 端点不可达时回退到派单时的固定键集;401(租约已非己有)不回退。
func (s *Server) uploadAttachments(ctx context.Context, d Dispatch, outDir string) []reporter.Attachment {
	if s.cfg.Uploader == nil {
		return nil
	}
	if d.UploadRequestURL != "" && s.cfg.Reporter != nil {
		atts, err := s.uploadOnDemand(ctx, d, outDir)
		if err == nil {
			return atts
		}
		if errors.Is(err, reporter.ErrLeaseNotOwned) {
			s.logf("task %s: 租约已非己有,放弃上传(不回退)", d.TaskID)
			return nil
		}
		s.logf("task %s: 按需签发失败(%v),回退固定键集", d.TaskID, err)
	}
	return s.uploadFixedSet(ctx, d, outDir)
}
```

`uploadOnDemand`：`collectRelPaths(outDir)` 遍历 out_dir 得到相对路径清单（跳过目录），
带重试 ≤2 次、间隔 3s 调 `RequestUploads`，把 `uploads[]` 转成
`[]uploader.PresignedUpload` 与 `files map[string]string`（key → `filepath.Join(outDir, path)`），
交给既有的 `s.cfg.Uploader.Upload`。

`uploadFixedSet` 就是**今天 `uploadAttachments` 的函数体原样搬过来**，并在
`wellKnownFiles` 的注释上补一句：该映射现在只服务于回退路径（§5.3）。

- [ ] **Step 5: 验证 401 那条真的会红**

把 `uploadAttachments` 里 `errors.Is(err, reporter.ErrLeaseNotOwned)` 那个分支临时删掉
（让 401 也走回退），跑：

Run: `cd agent && go test ./internal/server/ -run DoesNotFallBackOnUnauthorized`
Expected: **FAIL** —— "401 时不得上传任何东西"。改回来确认恢复 PASS，两次输出记进报告。

这一条值得单独验：它是三条里唯一保护"不污染别人证据"的断言，而回退逻辑很容易顺手
写成"所有错误都回退"。

- [ ] **Step 6: 跑测试**

Run: `cd agent && go vet ./... && go test ./... 2>&1 | grep -v "no test files" | tail -15`
Expected: 全部 PASS

- [ ] **Step 7: Commit**

```bash
git add agent/
git commit -m "feat(agent): request upload URLs at collect time with fallback"
```

---

### Task 6: 文档与欠账关闭

**Files:**
- Modify: `docs/device-test-sequence.md`（差距 #8 行）
- Modify: `runtime/internal/activity/presign.go`（CONTRACT-ISSUE 注释）
- Modify: `docs/superpowers/specs/2026-07-21-agent-service-design.md`（CONTRACT-ISSUE 注释）
- Modify: `deploy/README.md`（TTL 注意事项 + 新配置项）
- Modify: `runtime/README.md`（环境变量表）

**Interfaces:**
- Consumes: 前五个任务
- Produces: 无新导出符号

- [ ] **Step 1: 更新差距清单**

`docs/device-test-sequence.md` 的差距 #8 那一行改为：

```markdown
| 8 | 预签名 URL 按需签发(收集时请求) | **已实现**(2026-07-29):callbacks 新增 upload-requests,租约凭据鉴权,收集完成后秒级签发;顺带修复 collect glob(logs/*.log、dumps/**)从未上传的缺陷 | 遗留:派单时的 presigned_uploads 保留作滚动升级与端点不可达时的回退,下线条件见 `docs/superpowers/specs/2026-07-29-on-demand-presign-design.md` §7 |
```

- [ ] **Step 2: 关闭两处 CONTRACT-ISSUE**

`runtime/internal/activity/presign.go` 顶部注释改为：

```go
// EvidenceFiles 是 dispatch 时预签的固定键集。差距 #8 之后它只是**回退路径**:
// 正常路径由 Agent 在收集完成后经 callbacks 的 upload-requests 按需换取 URL,
// glob 命中的文件(logs/*.log、dumps/**)因此已能上传——原 CONTRACT-ISSUE 关闭,
// 详见 docs/superpowers/specs/2026-07-29-on-demand-presign-design.md。
```

`docs/superpowers/specs/2026-07-21-agent-service-design.md` 那条 CONTRACT-ISSUE 末尾补一句：

```
（2026-07-29 关闭：按需签发已落地，见 2026-07-29-on-demand-presign-design.md。）
```

- [ ] **Step 3: 更新部署文档**

`deploy/README.md` 的 `MINIO_PRESIGN_TTL` 那一条，把"必须超过最长任务时长"的告诫改为：

```markdown
- `MINIO_PRESIGN_TTL`（默认 `1h`）— presigned URL lifetime. Since gap #8 the normal path
  signs URLs **after** collection finishes, seconds before upload, so this no longer needs to
  exceed the longest task. It still bounds the dispatch-time fallback set, which is used when
  the agent is older than the endpoint or the endpoint is unreachable mid-run.
- `UPLOAD_REQUEST_MAX_FILES`（默认 `64`）— per-request file cap for `POST /callbacks/v1/upload-requests`.
  Over the cap the whole request is rejected rather than truncated, so a client never believes it
  uploaded everything when it did not.
```

`runtime/README.md` 的 Worker 环境变量表加一行 `UPLOAD_REQUEST_MAX_FILES`。

- [ ] **Step 4: 全量回归**

Run: `cd runtime && go build ./... && go vet ./... && go test ./...`
Run: `cd agent && go build ./... && go vet ./... && go test ./...`
Run: `cd /home/maxin/Code/hermes_ai_devops && .venv/bin/python -m pytest contracts/tests deploy/tests -q`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add docs/ runtime/ deploy/README.md
git commit -m "docs: close gap #8 and the collect-glob CONTRACT-ISSUE"
```

---

## 完成后的手工验收

1. 部署新 Runtime 与新 Agent，跑一次带 `dumps/` 产出的任务，确认 MinIO 里
   `runs/{task_id}/dumps/...` 有对象——这是 glob 缺陷被修复的直接证据。
2. 把 `MINIO_PRESIGN_TTL` 临时调成 `30s` 跑一次长任务：正常路径应仍然成功
   （URL 在收集后才签），证明 TTL 不再是长任务的风险点。
3. 用 `curl` 拿一个**伪造的** `lease_id` 调 upload-requests，确认返回 401 且响应体不含任何 URL。
4. 停掉 worker 后让 Agent 完成一次任务收集，确认回退路径生效、固定键集文件仍然上传。
5. 旧 Agent（未升级）+ 新 Runtime 跑一次，确认附件照常上传（`upload_request_url` 被忽略）。
