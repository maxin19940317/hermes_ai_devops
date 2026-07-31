package store

import (
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/lib/pq"
)

// cardActionsSchemaShape 捕获五张卡片动作表(card_action_inbox / card_actions /
// card_action_messages / card_action_snapshots / audit_log)的列、约束、索引,
// 用于比较 fresh schema.sql 与 upgraded(迁移后)库是否完全一致(§idempotent /
// §matches-fresh-schema 两条测试共用)。
type cardActionsSchemaShape struct {
	Columns     []string
	Constraints []string
	Indexes     []string
}

var cardActionsTables = []string{
	"card_action_inbox",
	"card_actions",
	"card_action_messages",
	"card_action_snapshots",
	"audit_log",
}

// applyFile 读取并执行给定路径下的 SQL 文件,失败即 Fatal。
func applyFile(t *testing.T, s *PGStore, path string) {
	t.Helper()
	if err := applyFileErr(s, path); err != nil {
		t.Fatalf("apply %s: %v", path, err)
	}
}

// applyFileErr 与 applyFile 相同,但把错误交还给调用方判断
// (TestCardActionsMigrationRequiresWorkflowRuns 需要断言错误内容,而不是 Fatal)。
func applyFileErr(s *PGStore, path string) error {
	sqlBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	_, err = s.DB.ExecContext(ctx, string(sqlBytes))
	return err
}

// applySchemaWithoutCardActions 剥掉 openIsolatedMigrationPG 在 OpenPG 内已经
// 自动应用的 schema.sql 里五张卡片动作表,模拟"上一轮生产迁移尚未跑"的库状态。
// workflow_runs 等其余表不受影响,留给迁移文件的前置检查确认存在。
func applySchemaWithoutCardActions(t *testing.T, s *PGStore) {
	t.Helper()
	drop := "DROP TABLE IF EXISTS " + strings.Join(cardActionsTables, ", ") + " CASCADE"
	if _, err := s.DB.ExecContext(ctx, drop); err != nil {
		t.Fatalf("drop card action tables to simulate pre-migration schema: %v", err)
	}
}

func captureCardActionsShape(t *testing.T, s *PGStore) cardActionsSchemaShape {
	t.Helper()
	db := s.DB
	var shape cardActionsSchemaShape

	// openIsolatedMigrationPG 每次都在一个随机命名的 schema 里跑(见该函数),
	// fresh 与 upgraded 各自拿到不同的 schema 名。pg_indexes.indexdef 会把
	// "ON <schema>.<table>" 的 schema 前缀烤进 DDL 文本,原样比较会因为 schema
	// 名不同而永远不相等,所以这里先拿到当前 schema 名,后面从 indexdef 里剥掉它。
	var currentSchema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&currentSchema); err != nil {
		t.Fatal(err)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT table_name, column_name, data_type, udt_name, is_nullable, COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = ANY($1)
		ORDER BY table_name, ordinal_position`, pq.Array(cardActionsTables))
	if err != nil {
		t.Fatal(err)
	}
	shape.Columns = scanShapeRows(t, rows, func(rows *sql.Rows) string {
		var table, name, dataType, udtName, nullable, defaultValue string
		if err := rows.Scan(&table, &name, &dataType, &udtName, &nullable, &defaultValue); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s.%s|%s|%s|%s|%s", table, name, dataType, udtName, nullable, defaultValue)
	})

	rows, err = db.QueryContext(ctx, `
		SELECT c.conrelid::regclass::text, c.conname, c.contype::text,
		       pg_get_constraintdef(c.oid, true)
		FROM pg_constraint c
		WHERE c.conrelid = ANY (ARRAY[
			to_regclass('card_action_inbox'),
			to_regclass('card_actions'),
			to_regclass('card_action_messages'),
			to_regclass('card_action_snapshots'),
			to_regclass('audit_log')
		])
		ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	shape.Constraints = scanShapeRows(t, rows, func(rows *sql.Rows) string {
		var tableName, name, constraintType, definition string
		if err := rows.Scan(&tableName, &name, &constraintType, &definition); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s|%s|%s|%s", tableName, name, constraintType, definition)
	})

	rows, err = db.QueryContext(ctx, `
		SELECT tablename, indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND tablename = ANY($1)
		ORDER BY tablename, indexname`, pq.Array(cardActionsTables))
	if err != nil {
		t.Fatal(err)
	}
	shape.Indexes = scanShapeRows(t, rows, func(rows *sql.Rows) string {
		var tableName, name, definition string
		if err := rows.Scan(&tableName, &name, &definition); err != nil {
			t.Fatal(err)
		}
		definition = strings.ReplaceAll(definition, currentSchema+".", "")
		return fmt.Sprintf("%s|%s|%s", tableName, name, definition)
	})

	return shape
}

// scanShapeRows 消费 rows 直到结束(或出错)并返回每行的字符串表示,
// 供 captureCardActionsShape 的三个查询共用而不必各自重复 Next/Scan/Close 样板。
func scanShapeRows(t *testing.T, rows *sql.Rows, format func(*sql.Rows) string) []string {
	t.Helper()
	var out []string
	for rows.Next() {
		out = append(out, format(rows))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestCardActionsMigrationIsIdempotent:迁移连跑两次结果相同。
// openIsolatedMigrationPG 内部的 OpenPG 已经把当前 schema.sql(含五张卡片动作表)
// 应用了一遍,所以这里验证的是"在表已存在的库上重复执行迁移文件不产生任何变化"——
// 这正是生产环境的真实场景:schema.sql 与迁移文件必须对同一张表长期共存不冲突。
func TestCardActionsMigrationIsIdempotent(t *testing.T) {
	s := openIsolatedMigrationPG(t)
	applyFile(t, s, "../../../deploy/postgres/migrations/2026-08-01-card-actions.sql")
	first := captureCardActionsShape(t, s)
	applyFile(t, s, "../../../deploy/postgres/migrations/2026-08-01-card-actions.sql")
	if !reflect.DeepEqual(first, captureCardActionsShape(t, s)) {
		t.Fatal("迁移不幂等")
	}
}

// TestCardActionsMigrationMatchesFreshSchema:fresh schema.sql 与 upgraded 库
// 的最终约束必须一致,否则新库与老库行为不同。
func TestCardActionsMigrationMatchesFreshSchema(t *testing.T) {
	fresh := openIsolatedMigrationPG(t)
	applyFile(t, fresh, "schema.sql")

	upgraded := openIsolatedMigrationPG(t)
	applySchemaWithoutCardActions(t, upgraded) // 剥掉五张表的建表语句
	applyFile(t, upgraded, "../../../deploy/postgres/migrations/2026-08-01-card-actions.sql")

	if !reflect.DeepEqual(captureCardActionsShape(t, fresh), captureCardActionsShape(t, upgraded)) {
		t.Fatal("fresh 与 upgraded 的约束不一致")
	}
}

// TestCardActionsMigrationRequiresWorkflowRuns:card_actions 的 FK 依赖
// workflow_runs,未完成上一轮生产迁移时必须明确失败,而不是静默建出一张没有 FK
// 的表。openIsolatedMigrationPG 已经通过 OpenPG 自动建好了 workflow_runs,这里
// 显式 DROP 掉它来复现"上一轮迁移未跑"的库状态。
func TestCardActionsMigrationRequiresWorkflowRuns(t *testing.T) {
	s := openIsolatedMigrationPG(t)
	if _, err := s.DB.ExecContext(ctx, `DROP TABLE workflow_runs CASCADE`); err != nil {
		t.Fatalf("drop workflow_runs to simulate missing precondition: %v", err)
	}
	err := applyFileErr(s, "../../../deploy/postgres/migrations/2026-08-01-card-actions.sql")
	if err == nil {
		t.Fatal("缺 workflow_runs 时迁移必须失败")
	}
	if !strings.Contains(err.Error(), "workflow_runs") {
		t.Fatalf("错误信息应指明缺 workflow_runs: %v", err)
	}
}
