package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const contractsDir = "../../../contracts"

// stripDocNoise 递归移除 description 与 null 值键。契约 openapi.yaml 的
// sha256 描述是 flow 映射内的未加引号逗号文本,YAML 解析为
// "整包校验和" + 一个 null 值的垃圾键——只影响文档文本,不影响校验语义,
// 故漂移比较聚焦结构性约束(type/required/pattern/enum 等)。
func stripDocNoise(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			if k == "description" || x == nil {
				continue
			}
			out[k] = stripDocNoise(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = stripDocNoise(x)
		}
		return out
	}
	return v
}

// 防契约漂移:嵌入的 dispatch.schema.json 必须与
// contracts/client-agent-api.openapi.yaml 的 TaskDispatchRequest 组件语义一致
// (同 manifest 包模式;契约侧为 YAML 内联 schema,按 JSON 归一化后比较)。
func TestEmbeddedSchemaMatchesContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(contractsDir, "client-agent-api.openapi.yaml"))
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse contract yaml: %v", err)
	}
	schemas, ok := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if !ok {
		t.Fatal("contract: components.schemas 结构不符")
	}
	contract, ok := schemas["TaskDispatchRequest"]
	if !ok {
		t.Fatal("contract: 缺少 TaskDispatchRequest 组件")
	}
	cj, err := json.Marshal(contract) // YAML 值(int 等)经 JSON 归一化
	if err != nil {
		t.Fatalf("marshal contract schema: %v", err)
	}
	var want any
	if err := json.Unmarshal(cj, &want); err != nil {
		t.Fatalf("normalize contract schema: %v", err)
	}

	var embedded map[string]any
	if err := json.Unmarshal(EmbeddedDispatchSchema, &embedded); err != nil {
		t.Fatalf("parse embedded schema: %v", err)
	}
	// 嵌入副本允许携带文档化元字段,不参与一致性比较
	for _, k := range []string{"$schema", "$id", "title", "description"} {
		delete(embedded, k)
	}
	gj, _ := json.Marshal(embedded)
	var got any
	if err := json.Unmarshal(gj, &got); err != nil {
		t.Fatalf("normalize embedded schema: %v", err)
	}

	if !reflect.DeepEqual(stripDocNoise(want), stripDocNoise(got)) {
		t.Fatalf("embedded dispatch.schema.json 与契约 TaskDispatchRequest 不一致(防契约漂移):\ncontract: %s\nembedded: %s", cj, gj)
	}
}

// validDispatch 返回一份过 Schema 的最小合法载荷(表驱动用)。
func validDispatch() map[string]any {
	return map[string]any{
		"task_id":         "t-1",
		"idempotency_key": "wf-1:t-1:a1",
		"attempt":         1,
		"artifact": map[string]any{
			"url":    "https://gitlab.example.com/api/v4/projects/1/packages/generic/pkg/1.0/pkg.tar.gz",
			"sha256": strings.Repeat("a", 64),
			"auth":   map[string]any{"type": "bearer", "token": "tok"},
		},
		"manifest_digest":   strings.Repeat("b", 64),
		"device_serial":     "SERIAL123",
		"callback_base_url": "http://runtime:18091",
	}
}

func TestValidateDispatchAcceptsValid(t *testing.T) {
	cases := map[string]func(map[string]any){
		"最小合法载荷": func(d map[string]any) {},
		"job_token 认证": func(d map[string]any) {
			d["artifact"].(map[string]any)["auth"] = map[string]any{"type": "job_token", "token": "t"}
		},
		"含 presigned_uploads": func(d map[string]any) {
			d["presigned_uploads"] = []any{
				map[string]any{"object_key": "runs/t-1/stdout.log", "url": "http://minio:9000/b/runs/t-1/stdout.log?sig=x"},
				map[string]any{"object_key": "runs/t-1/result.json", "url": "http://minio:9000/b/runs/t-1/result.json?sig=x", "expires_at": "2026-07-21T10:00:00Z"},
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := validDispatch()
			mutate(d)
			body, _ := json.Marshal(d)
			if err := ValidateDispatch(body); err != nil {
				t.Errorf("expected valid, got %v", err)
			}
		})
	}
}

func TestValidateDispatchRejectsInvalid(t *testing.T) {
	cases := map[string]func(map[string]any){
		"缺 task_id":           func(d map[string]any) { delete(d, "task_id") },
		"缺 idempotency_key":   func(d map[string]any) { delete(d, "idempotency_key") },
		"缺 artifact":          func(d map[string]any) { delete(d, "artifact") },
		"缺 manifest_digest":   func(d map[string]any) { delete(d, "manifest_digest") },
		"缺 device_serial":     func(d map[string]any) { delete(d, "device_serial") },
		"缺 callback_base_url": func(d map[string]any) { delete(d, "callback_base_url") },
		"attempt 为 0":         func(d map[string]any) { d["attempt"] = 0 },
		"attempt 非整数":         func(d map[string]any) { d["attempt"] = 1.5 },
		"sha256 非 64 位小写 hex": func(d map[string]any) {
			d["artifact"].(map[string]any)["sha256"] = strings.Repeat("A", 64)
		},
		"manifest_digest 非 64 位 hex": func(d map[string]any) { d["manifest_digest"] = "xyz" },
		"auth.type 越枚举": func(d map[string]any) {
			d["artifact"].(map[string]any)["auth"] = map[string]any{"type": "private_token", "token": "t"}
		},
		"auth 缺 token": func(d map[string]any) {
			d["artifact"].(map[string]any)["auth"] = map[string]any{"type": "bearer"}
		},
		"顶层多余字段":        func(d map[string]any) { d["extra"] = 1 },
		"artifact 多余字段": func(d map[string]any) { d["artifact"].(map[string]any)["extra"] = 1 },
		"presigned_uploads 缺 url": func(d map[string]any) {
			d["presigned_uploads"] = []any{map[string]any{"object_key": "runs/t-1/a.txt"}}
		},
		"task_id 超长":         func(d map[string]any) { d["task_id"] = strings.Repeat("x", 129) },
		"idempotency_key 超长": func(d map[string]any) { d["idempotency_key"] = strings.Repeat("x", 257) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := validDispatch()
			mutate(d)
			body, _ := json.Marshal(d)
			if err := ValidateDispatch(body); err == nil {
				t.Error("expected schema rejection, got nil error")
			}
		})
	}
	if err := ValidateDispatch([]byte("{not json")); err == nil {
		t.Error("非 JSON 应被拒绝")
	}
}
