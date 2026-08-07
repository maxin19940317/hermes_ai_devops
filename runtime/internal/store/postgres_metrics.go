package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// SaveMetrics 批量写入指标点(PG 实现)。
func (s *PGStore) SaveMetrics(ctx context.Context, points []MetricPoint) error {
	if len(points) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save metrics begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO metrics (project, variant, suite, metric_name, value, task_id)
		 VALUES ($1, $2, $3, $4, $5, $6)`)
	if err != nil {
		return fmt.Errorf("prepare save metrics: %w", err)
	}
	defer stmt.Close()

	for _, p := range points {
		if _, err := stmt.ExecContext(ctx,
			p.Project, p.Variant, p.Suite, p.MetricName, p.Value, p.TaskID,
		); err != nil {
			return fmt.Errorf("save metric %s/%s: %w", p.Variant, p.MetricName, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save metrics: %w", err)
	}
	return nil
}

// MetricsForVariant 返回指定变体最近 limit 条指标点(created_at 倒序,跨
// project/suite/metric)。供飞书指令 metrics 展示该变体性能概况。
func (s *PGStore) MetricsForVariant(
	ctx context.Context, variant string, limit int,
) ([]MetricPoint, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT project, variant, suite, metric_name, value, task_id
		FROM metrics WHERE variant = $1
		ORDER BY created_at DESC, id DESC LIMIT $2`,
		variant, limit)
	if err != nil {
		return nil, fmt.Errorf("metrics for variant %s: %w", variant, err)
	}
	defer rows.Close()
	out := []MetricPoint{}
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.Project, &p.Variant, &p.Suite, &p.MetricName,
			&p.Value, &p.TaskID); err != nil {
			return nil, fmt.Errorf("metrics for variant %s: scan: %w", variant, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics for variant %s: %w", variant, err)
	}
	return out, nil
}

// Baseline 返回指定键最近 n 条记录的中位数(PG 实现)。
// N < 3 → nil(基线不可信)。
func (s *PGStore) Baseline(
	ctx context.Context, project, variant, suite, metricName string, n int,
) (*MetricBaseline, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT value FROM metrics
		 WHERE project=$1 AND variant=$2 AND suite=$3 AND metric_name=$4
		 ORDER BY created_at DESC LIMIT $5`,
		project, variant, suite, metricName, n,
	)
	if err != nil {
		return nil, fmt.Errorf("baseline query: %w", err)
	}
	defer rows.Close()

	var vals []float64
	for rows.Next() {
		var v sql.NullFloat64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("baseline scan: %w", err)
		}
		if v.Valid {
			vals = append(vals, v.Float64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("baseline rows: %w", err)
	}

	if len(vals) < 3 {
		return nil, nil
	}

	// 中位数:升序排序取中间
	sort.Float64s(vals)
	return &MetricBaseline{Median: vals[len(vals)/2], N: len(vals)}, nil
}
