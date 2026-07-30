package trigger

import (
	"os"
	"strings"
	"testing"
)

func TestParseBundleValid(t *testing.T) {
	b, err := ParseBundle(mustJSON(t, validBundle()))
	if err != nil {
		t.Fatalf("valid bundle rejected: %v", err)
	}
	if b.Commit != "abcd1234" || b.PipelineID != 42 || b.PipelineGlobalID != 42001 || b.Version != "1.2.3" ||
		b.Project != "grp/algo-super-sdk" || len(b.Packages) != 2 {
		t.Errorf("parsed = %+v", b)
	}
	var pipelineGlobalID int64 = b.PipelineGlobalID
	if pipelineGlobalID != 42001 {
		t.Errorf("pipeline global ID = %d", pipelineGlobalID)
	}
	if b.Packages[0].Variant != "aarch64_Android_SNPE_2.21" {
		t.Errorf("package[0] = %+v", b.Packages[0])
	}
}

func TestParseBundleInvalid(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"missing pipeline global id": func(m map[string]any) { delete(m, "pipeline_global_id") },
		"missing packages":           func(m map[string]any) { delete(m, "packages") },
		"empty packages":             func(m map[string]any) { m["packages"] = []any{} },
		"bad version":                func(m map[string]any) { m["version"] = "1.2.3-rc.1" },
		"bad commit":                 func(m map[string]any) { m["commit"] = "XYZ" },
		"unknown top field":          func(m map[string]any) { m["extra"] = 1 },
	}
	for name, mutate := range cases {
		m := validBundle()
		mutate(m)
		if _, err := ParseBundle(mustJSON(t, m)); err == nil {
			t.Errorf("%s: 应被拒绝", name)
		}
	}
	if _, err := ParseBundle([]byte("not json")); err == nil {
		t.Error("非 JSON 应被拒绝")
	}
}

func TestBundleSchemaRejectsUncommandableProject(t *testing.T) {
	rejected := []string{
		"grp/bad project",
		"/absolute",
		"grp//double",
		"grp/trailing\n",
		strings.Repeat("a", 257),
	}
	for _, project := range rejected {
		t.Run(project, func(t *testing.T) {
			bundle := validBundle()
			bundle["project"] = project
			if _, err := ParseBundle(mustJSON(t, bundle)); err == nil {
				t.Fatalf("project %q should be rejected", project)
			}
		})
	}
	bundle := validBundle()
	bundle["project"] = "grp/good_project"
	if _, err := ParseBundle(mustJSON(t, bundle)); err != nil {
		t.Fatalf("commandable project rejected: %v", err)
	}
}

// 内嵌 schema 副本与 contracts/ 源文件防漂移(与 agent/internal/manifest 同模式)。
func TestEmbeddedBundleSchemaMatchesContract(t *testing.T) {
	src, err := os.ReadFile("../../../contracts/bundle.schema.json")
	if err != nil {
		t.Skipf("contracts 不在预期相对路径: %v", err)
	}
	if string(src) != string(embeddedBundleSchema) {
		t.Error("内嵌 bundle.schema.json 与 contracts/ 源不一致,请 cp 同步后重跑")
	}
}
