// Package store 是 Client Agent 的本地 SQLite 任务存储(设计文档 §3.2,
// CLAUDE.md §11)。每次状态迁移单事务落盘;进程崩溃重启后由
// LoadInflight 恢复非终态任务与未上报事件/结果,供补报,保证崩溃不丢
// 执行一致性。
//
// 驱动为 modernc.org/sqlite(纯 Go,无 CGO,可交叉编译 Windows);
// 打开时启用 WAL journal mode 与 busy_timeout。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动(database/sql 驱动名 "sqlite")
)

// timeLayout 是落盘时间格式:UTC ISO-8601 毫秒精度(工程约定 §13:
// 时间一律 UTC 存储)。
const timeLayout = "2006-01-02T15:04:05.000Z"

// State 是任务生命周期状态。取值与 CLAUDE.md §9 状态模型对齐,
// 终态集合必须等于 executor.Status 的终态集合(由测试保证一致)。
type State string

const (
	StateQueued      State = "QUEUED"
	StateDispatching State = "DISPATCHING"
	StateAccepted    State = "ACCEPTED"
	StatePreparing   State = "PREPARING"
	StateDownloading State = "DOWNLOADING"
	StateDeploying   State = "DEPLOYING"
	StateRunning     State = "RUNNING"
	StateCollecting  State = "COLLECTING"
	StateCompleted   State = "COMPLETED"
	StateFailed      State = "FAILED"
	StateTimeout     State = "TIMEOUT"
	StateCanceled    State = "CANCELED"
)

// IsTerminal 报告 s 是否为终态:COMPLETED / FAILED / TIMEOUT / CANCELED。
func IsTerminal(s State) bool {
	switch s {
	case StateCompleted, StateFailed, StateTimeout, StateCanceled:
		return true
	}
	return false
}

// 哨兵错误,供调用方 errors.Is 判定。
var (
	// ErrTaskNotFound:指定 task_id 不存在。
	ErrTaskNotFound = errors.New("store: task not found")
	// ErrTerminalState:任务已处终态,拒绝任何迁移。
	ErrTerminalState = errors.New("store: transition from terminal state")
	// ErrStateMismatch:任务当前状态与迁移声明的 from 不一致(并发或重复调用)。
	ErrStateMismatch = errors.New("store: current state does not match transition source")
)

// Task 是 tasks 表的一行。ResultRecorded 是结果上报去重标记
// (tasks.result_recorded 列):终态任务的结果成功上报后置位,重复上报
// 由 Runtime 按 task_id 去重,Agent 侧据此跳过重投(§4)。
type Task struct {
	TaskID         string
	IdempotencyKey string // 幂等键 {workflow_id}:{task_id}:a{attempt},全局唯一
	State          State
	Attempt        int
	DispatchJSON   string // 原始 dispatch 载荷,崩溃恢复后重放执行所需
	OutDir         string
	StartedAt      time.Time
	EndedAt        *time.Time // 仅终态迁移时写入
	ResultRecorded bool
}

// Event 是 events 表的一行。Seq 每任务从 1 单调递增,与
// (idempotency_key, seq) 联合作为 Runtime 去重依据(§4)。
type Event struct {
	TaskID    string
	Seq       int64
	FromState State
	ToState   State
	Ts        time.Time
	Detail    string // 可空;空串落盘为 NULL
	Reported  bool
}

// Inflight 是 LoadInflight 的恢复视图。
type Inflight struct {
	// Tasks 为所有非终态任务(崩溃时执行被中断,需按既有语义处理)。
	Tasks []Task
	// UnreportedEvents 为全部未上报事件(含非终态与终态任务的)。
	UnreportedEvents []Event
	// PendingResults 为已终态但结果未标记上报的任务(需重投 result)。
	PendingResults []Task
}

// Store 包装 *sql.DB,无全局状态;单连接串行化写事务。
type Store struct {
	db *sql.DB
}

// Open 打开(必要时创建)path 处的 SQLite 库,启用 WAL 与 busy_timeout,
// 并建表(幂等)。path 为文件路径;父目录须已存在。
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// 单进程单写者:串行化连接,配合 busy_timeout 避免 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close 关闭底层数据库。WAL 模式下正常关闭会做 checkpoint。
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}
	return nil
}

func migrate(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS tasks (
  task_id TEXT PRIMARY KEY,
  idempotency_key TEXT UNIQUE NOT NULL,
  state TEXT NOT NULL,
  attempt INTEGER NOT NULL,
  dispatch_json TEXT NOT NULL,
  out_dir TEXT NOT NULL,
  started_at TEXT NOT NULL,
  ended_at TEXT,
  result_recorded INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS events (
  task_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  ts TEXT NOT NULL,
  detail TEXT,
  reported INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (task_id, seq)
);`
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	return nil
}

// CreateTask 以 QUEUED 状态插入新任务,用于幂等派单检查(§4):
// 同 idempotency_key 重复插入返回 wrapped 唯一约束错误,调用方应改用
// LookupByIdempotencyKey 返回既有任务状态。StartedAt 为零值时取当前 UTC。
func (s *Store) CreateTask(ctx context.Context, t Task) error {
	if t.StartedAt.IsZero() {
		t.StartedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO tasks (task_id, idempotency_key, state, attempt, dispatch_json, out_dir, started_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.TaskID, t.IdempotencyKey, string(StateQueued), t.Attempt,
		t.DispatchJSON, t.OutDir, formatTime(t.StartedAt))
	if err != nil {
		return fmt.Errorf("create task %q: %w", t.TaskID, err)
	}
	return nil
}

// GetTask 按 task_id 查询任务;不存在返回 ErrTaskNotFound。
func (s *Store) GetTask(ctx context.Context, taskID string) (Task, error) {
	t, err := s.scanTask(s.db.QueryRowContext(ctx, `
SELECT task_id, idempotency_key, state, attempt, dispatch_json, out_dir,
       started_at, ended_at, result_recorded
FROM tasks WHERE task_id = ?`, taskID))
	if err != nil {
		return Task{}, fmt.Errorf("get task %q: %w", taskID, err)
	}
	return t, nil
}

// LookupByIdempotencyKey 按幂等键查询任务;不存在返回 ErrTaskNotFound。
func (s *Store) LookupByIdempotencyKey(ctx context.Context, key string) (Task, error) {
	t, err := s.scanTask(s.db.QueryRowContext(ctx, `
SELECT task_id, idempotency_key, state, attempt, dispatch_json, out_dir,
       started_at, ended_at, result_recorded
FROM tasks WHERE idempotency_key = ?`, key))
	if err != nil {
		return Task{}, fmt.Errorf("lookup idempotency key %q: %w", key, err)
	}
	return t, nil
}

// Transition 在单事务内完成:校验任务当前状态等于 from(且非终态)→
// 更新 tasks.state(to 为终态时同写 ended_at)→ 追加 events 行
// (seq 取该任务现有最大 seq + 1,从 1 起单调递增)→ 返回新 seq。
// 任务不存在返回 ErrTaskNotFound;当前状态为终态返回 ErrTerminalState;
// 当前状态与 from 不符返回 ErrStateMismatch。三者均不产生任何写入。
func (s *Store) Transition(ctx context.Context, taskID string, from, to State, detail string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transition tx: %w", err)
	}
	defer tx.Rollback()

	var cur string
	err = tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, taskID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrTaskNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read task %q state: %w", taskID, err)
	}
	if IsTerminal(State(cur)) {
		return 0, fmt.Errorf("task %q in %s: %w", taskID, cur, ErrTerminalState)
	}
	if State(cur) != from {
		return 0, fmt.Errorf("task %q state %s, want %s: %w", taskID, cur, from, ErrStateMismatch)
	}

	now := time.Now()
	if IsTerminal(to) {
		_, err = tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?, ended_at = ? WHERE task_id = ?`,
			string(to), formatTime(now), taskID)
	} else {
		_, err = tx.ExecContext(ctx,
			`UPDATE tasks SET state = ? WHERE task_id = ?`, string(to), taskID)
	}
	if err != nil {
		return 0, fmt.Errorf("update task %q state: %w", taskID, err)
	}

	var seq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE task_id = ?`, taskID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("next seq for task %q: %w", taskID, err)
	}
	var detailArg any // 空串落 NULL,保持 detail 可空语义
	if detail != "" {
		detailArg = detail
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO events (task_id, seq, from_state, to_state, ts, detail)
VALUES (?, ?, ?, ?, ?, ?)`,
		taskID, seq, string(from), string(to), formatTime(now), detailArg); err != nil {
		return 0, fmt.Errorf("append event for task %q: %w", taskID, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transition task %q -> %s: %w", taskID, to, err)
	}
	return seq, nil
}

// MarkEventReported 将指定事件标记为已上报,供 reporter 确认后调用。
// 更新 0 行(事件不存在)不算错误,保持幂等。
func (s *Store) MarkEventReported(ctx context.Context, taskID string, seq int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE events SET reported = 1 WHERE task_id = ? AND seq = ?`, taskID, seq); err != nil {
		return fmt.Errorf("mark event reported %q seq=%d: %w", taskID, seq, err)
	}
	return nil
}

// UnreportedEvents 返回全部未上报事件(按 task_id, seq 排序),
// 供 reporter 后台重发循环使用。
func (s *Store) UnreportedEvents(ctx context.Context) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT task_id, seq, from_state, to_state, ts, detail, reported
FROM events WHERE reported = 0 ORDER BY task_id, seq`)
	if err != nil {
		return nil, fmt.Errorf("query unreported events: %w", err)
	}
	defer rows.Close()
	evs, err := scanEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("scan unreported events: %w", err)
	}
	return evs, nil
}

// ResultRecorded 报告指定任务的结果是否已标记上报(去重检查,§4)。
func (s *Store) ResultRecorded(ctx context.Context, taskID string) (bool, error) {
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return false, err
	}
	return t.ResultRecorded, nil
}

// MarkResultRecorded 将任务结果标记为已上报;任务不存在返回
// ErrTaskNotFound。重复标记幂等。
func (s *Store) MarkResultRecorded(ctx context.Context, taskID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET result_recorded = 1 WHERE task_id = ?`, taskID)
	if err != nil {
		return fmt.Errorf("mark result recorded %q: %w", taskID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark result recorded %q: %w", taskID, err)
	}
	if n == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// LoadInflight 返回崩溃恢复视图:全部非终态任务、全部未上报事件、
// 以及已终态但结果未标记上报的任务。启动时据此恢复执行状态并补报
// 事件/结果(§3.2、§4)。
func (s *Store) LoadInflight(ctx context.Context) (Inflight, error) {
	var out Inflight

	rows, err := s.db.QueryContext(ctx, `
SELECT task_id, idempotency_key, state, attempt, dispatch_json, out_dir,
       started_at, ended_at, result_recorded
FROM tasks WHERE state NOT IN ('COMPLETED', 'FAILED', 'TIMEOUT', 'CANCELED')
ORDER BY started_at`)
	if err != nil {
		return Inflight{}, fmt.Errorf("load inflight tasks: %w", err)
	}
	out.Tasks, err = scanTasks(rows)
	if err != nil {
		return Inflight{}, fmt.Errorf("scan inflight tasks: %w", err)
	}

	out.UnreportedEvents, err = s.UnreportedEvents(ctx)
	if err != nil {
		return Inflight{}, err
	}

	rows2, err := s.db.QueryContext(ctx, `
SELECT task_id, idempotency_key, state, attempt, dispatch_json, out_dir,
       started_at, ended_at, result_recorded
FROM tasks WHERE state IN ('COMPLETED', 'FAILED', 'TIMEOUT', 'CANCELED')
  AND result_recorded = 0
ORDER BY started_at`)
	if err != nil {
		return Inflight{}, fmt.Errorf("load pending results: %w", err)
	}
	out.PendingResults, err = scanTasks(rows2)
	if err != nil {
		return Inflight{}, fmt.Errorf("scan pending results: %w", err)
	}
	return out, nil
}

// scanTask 扫描单个任务行;sql.ErrNoRows 归一为 ErrTaskNotFound。
func (s *Store) scanTask(row *sql.Row) (Task, error) {
	var (
		t         Task
		state     string
		startedAt string
		endedAt   sql.NullString
		recorded  int
	)
	err := row.Scan(&t.TaskID, &t.IdempotencyKey, &state, &t.Attempt,
		&t.DispatchJSON, &t.OutDir, &startedAt, &endedAt, &recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrTaskNotFound
	}
	if err != nil {
		return Task{}, err
	}
	t.State = State(state)
	t.StartedAt, err = parseTime(startedAt)
	if err != nil {
		return Task{}, fmt.Errorf("parse started_at: %w", err)
	}
	if endedAt.Valid {
		end, err := parseTime(endedAt.String)
		if err != nil {
			return Task{}, fmt.Errorf("parse ended_at: %w", err)
		}
		t.EndedAt = &end
	}
	t.ResultRecorded = recorded != 0
	return t, nil
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var (
			t         Task
			state     string
			startedAt string
			endedAt   sql.NullString
			recorded  int
		)
		if err := rows.Scan(&t.TaskID, &t.IdempotencyKey, &state, &t.Attempt,
			&t.DispatchJSON, &t.OutDir, &startedAt, &endedAt, &recorded); err != nil {
			return nil, err
		}
		t.State = State(state)
		var err error
		t.StartedAt, err = parseTime(startedAt)
		if err != nil {
			return nil, fmt.Errorf("parse started_at: %w", err)
		}
		if endedAt.Valid {
			end, err := parseTime(endedAt.String)
			if err != nil {
				return nil, fmt.Errorf("parse ended_at: %w", err)
			}
			t.EndedAt = &end
		}
		t.ResultRecorded = recorded != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var (
			e      Event
			from   string
			to     string
			ts     string
			detail sql.NullString
			rep    int
		)
		if err := rows.Scan(&e.TaskID, &e.Seq, &from, &to, &ts, &detail, &rep); err != nil {
			return nil, err
		}
		e.FromState = State(from)
		e.ToState = State(to)
		var err error
		e.Ts, err = parseTime(ts)
		if err != nil {
			return nil, fmt.Errorf("parse event ts: %w", err)
		}
		e.Detail = detail.String
		e.Reported = rep != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}
