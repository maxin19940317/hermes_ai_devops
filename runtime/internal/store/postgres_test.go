package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

// 集成测试:需要真实 Postgres,由 TEST_DATABASE_URL 门控
// (本机无 PG 时跳过;服务器部署后必须跑通)。
func TestPGStoreRegisterIdempotent(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置,跳过 Postgres 集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := OpenPG(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.DB.Close()
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM artifacts WHERE commit_sha = 'ittest01'`); err != nil {
		t.Fatal(err)
	}

	arts := []Artifact{
		{Project: "p", CommitSHA: "ittest01", PipelineID: 1, Variant: "v1", BuildType: "Release",
			URL: "https://x/1", SHA256: "a", Size: 1, ManifestDigest: "d"},
		{Project: "p", CommitSHA: "ittest01", PipelineID: 1, Variant: "v2", BuildType: "Release",
			URL: "https://x/2", SHA256: "b", Size: 2, ManifestDigest: "d"},
	}
	for i := 0; i < 2; i++ { // 两次登记,幂等
		if err := s.RegisterArtifacts(ctx, arts); err != nil {
			t.Fatalf("register #%d: %v", i, err)
		}
	}
	var n int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM artifacts WHERE commit_sha = 'ittest01'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows = %d, want 2(重复登记不得产生新行)", n)
	}
}

// decisions.task_id 有外键(§11):引用不存在的任务时 PG 必须报错。
// MemStore 与其他写路径一致不做外键校验(放行),该差异与 AppendTaskEvent 相同,
// 故只在本 PG 集成用例断言,不进 conformance 套件。
func TestPGStoreDecisionRequiresExistingTask(t *testing.T) {
	s := openTestPG(t)
	err := s.SaveDecision(ctx, wf.DecisionRow{TaskID: "no-such-task", Actor: "rule",
		Output: json.RawMessage(`{"verdict":"PASS"}`)})
	if err == nil {
		t.Fatal("引用不存在的 task_id 应报外键错误")
	}
}
