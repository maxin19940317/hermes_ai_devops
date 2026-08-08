package callbacks

import (
	"os"
	"path/filepath"
	"testing"
)

// Runtime 侧消费 result.json 的 Schema 是 contracts/ 的 go:embed 副本。
// Agent 侧(agent/internal/reporter)早有同名防漂移测试,Runtime 侧一直缺失(A8):
// contracts 演进时生产者被强制同步、消费者不会,同一份契约会在两端静默分叉,
// 且分叉方向恰好是 Runtime 用旧 Schema 校验新 result.json。
func TestEmbeddedSchemaMatchesContract(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("../../../contracts", "result.schema.json"))
	if err != nil {
		t.Fatalf("read contracts schema: %v", err)
	}
	if resultSchemaJSON != string(want) {
		t.Fatal("embedded result.schema.json 与 contracts/ 不一致,请重新拷贝(防契约漂移)")
	}
}
