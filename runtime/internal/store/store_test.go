package store

import (
	"testing"
)

func TestMemStoreArtifactKeyIncludesProject(t *testing.T) {
	s := NewMemStore()
	arts := []Artifact{
		{Project: "grp/a", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1",
			BuildType: "Release", URL: "a", SHA256: "sa", Size: 1, ManifestDigest: "ma"},
		{Project: "grp/b", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1",
			BuildType: "Release", URL: "b", SHA256: "sb", Size: 1, ManifestDigest: "mb"},
	}
	if err := s.RegisterArtifacts(ctx, arts); err != nil {
		t.Fatal(err)
	}
	got := s.Artifacts()
	if len(got) != 2 {
		t.Fatalf("artifacts = %+v, want both projects", got)
	}
}

func TestMemStoreArtifactLookupIsProjectAware(t *testing.T) {
	s := NewMemStore()
	arts := []Artifact{
		{Project: "grp/a", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1"},
		{Project: "grp/b", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1"},
	}
	if err := s.RegisterArtifacts(ctx, arts); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListArtifacts(ctx, "grp/a", "abcd1234", 42)
	if err != nil || len(got) != 1 || got[0].Project != "grp/a" {
		t.Fatalf("project list = %+v err=%v, want grp/a only", got, err)
	}
	if n, err := s.NextWorkflowAttempt(ctx, "grp/a", "abcd1234", 42, "v1"); err != nil || n != 1 {
		t.Fatalf("project retry = %d err=%v, want 1", n, err)
	}
}

func TestMemStoreMetricsBaseline(t *testing.T) {
	s := NewMemStore()
	// 保存 7 个 PASSED 指标点(latency_ms 逐次增大)
	vals := []float64{10, 12, 11, 13, 14, 15, 100}
	for i, v := range vals {
		taskID := "t" + string(rune('0'+i))
		if err := s.SaveMetrics(ctx, []MetricPoint{
			{Project: "p", Variant: "v", Suite: "smoke", MetricName: "latency_ms", Value: v, TaskID: taskID},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 最近 5 次 PASSED: 100, 15, 14, 13, 11 → 排序后 11, 13, 14, 15, 100 → 中位数 14
	bl, err := s.Baseline(ctx, "p", "v", "smoke", "latency_ms", 5)
	if err != nil {
		t.Fatal(err)
	}
	if bl == nil {
		t.Fatal("baseline is nil, want median")
	}
	if bl.N != 5 || bl.Median != 14 {
		t.Errorf("baseline = %+v, want N=5 median=14", bl)
	}

	// 样本不足 3:无基线
	bl2, _ := s.Baseline(ctx, "p", "nope", "smoke", "latency_ms", 5)
	if bl2 != nil {
		t.Errorf("baseline = %+v, want nil(insufficient)", bl2)
	}

	// 不同项目不混淆
	bl3, _ := s.Baseline(ctx, "other", "v", "smoke", "latency_ms", 5)
	if bl3 != nil {
		t.Errorf("baseline = %+v, want nil(wrong project)", bl3)
	}
}
