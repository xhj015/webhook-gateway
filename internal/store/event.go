package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 事件的五个状态。词表定义在 docs/02-flow.md 第三节，数据库里也有 CHECK 约束兜底。
//
// 注意：W3 只用到 StatusPending —— 事件出生时就是它。
// 其余四个状态的流转逻辑属于 M3/M4/M6，到那时再写（并且由你本人写，见项目书评审第七节）。
const (
	StatusPending    = "pending"    // 已落库，等待被 worker 抢占
	StatusProcessing = "processing" // 已被抢占，正在投递中（60 秒租约）
	StatusDelivered  = "delivered"  // 已确认送达（2xx），终态
	StatusRetrying   = "retrying"   // 投递失败但预算没用完，在等退避到期
	StatusFailed     = "failed"     // 重试预算耗尽，需人工介入，终态
)

// Event 是业务对象：一次待投递的 payload，系统为它的最终结果负责。
type Event struct {
	ID            string
	EndpointID    string
	Status        string
	Payload       []byte // 原始 body 字节，原样透传，不解析
	Headers       string // JSON，投递时要还原的请求头
	AttemptsCount int    // 本轮预算已用次数
	NextAttemptAt int64  // 下次可投递时间（Unix 秒）
	LockedUntil   *int64
	LastError     *string
	CreatedAt     int64
	UpdatedAt     int64
	DeliveredAt   *int64
}

// CreateEvent 落库一条新事件（接收链路的第一步，业务规则 B4）。
//
// 这里没有显式 BEGIN/COMMIT：单条 INSERT 本身就是一个事务，
// 语句返回即代表已提交。docs/04-data-model.md 第五节把事务边界收在两处，
// 这是其中之一（另一处是 W4 的投递结算，那个才必须显式开事务）。
func (s *Store) CreateEvent(ctx context.Context, ev Event) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events
		   (id, endpoint_id, status, payload, headers, attempts_count,
		    next_attempt_at, locked_until, last_error, created_at, updated_at, delivered_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, NULL)`,
		ev.ID, ev.EndpointID, ev.Status, ev.Payload, ev.Headers,
		ev.AttemptsCount, ev.NextAttemptAt, ev.CreatedAt, ev.UpdatedAt)
	if err != nil {
		return fmt.Errorf("写入事件失败: %w", err)
	}
	return nil
}

// eventsColumns 是 events 全字段的 SELECT 列表，GetEvent 与 ListEvents 共用，避免两处写差。
const eventsColumns = `id, endpoint_id, status, payload, headers, attempts_count,
		        next_attempt_at, locked_until, last_error, created_at, updated_at, delivered_at`

// rowScanner 是 *sql.Row 与 *sql.Rows 共有的最小接口（都实现了 Scan）。
// 用它可以写一份 scanEvent 同时服务"查单条"和"遍历列表"。
type rowScanner interface{ Scan(dest ...any) error }

// scanEvent 把当前行扫进一个 Event，并处理三个可空列（locked_until / last_error / delivered_at）。
func scanEvent(sc rowScanner) (Event, error) {
	var (
		ev          Event
		lockedUntil sql.NullInt64
		lastError   sql.NullString
		deliveredAt sql.NullInt64
	)
	if err := sc.Scan(&ev.ID, &ev.EndpointID, &ev.Status, &ev.Payload, &ev.Headers, &ev.AttemptsCount,
		&ev.NextAttemptAt, &lockedUntil, &lastError, &ev.CreatedAt, &ev.UpdatedAt, &deliveredAt); err != nil {
		return Event{}, err
	}
	if lockedUntil.Valid {
		v := lockedUntil.Int64
		ev.LockedUntil = &v
	}
	if lastError.Valid {
		v := lastError.String
		ev.LastError = &v
	}
	if deliveredAt.Valid {
		v := deliveredAt.Int64
		ev.DeliveredAt = &v
	}
	return ev, nil
}

// GetEvent 按 ID 查事件。查不到返回 ErrNotFound。
//
// 被接收侧（M5 查询接口）直接复用。
func (s *Store) GetEvent(ctx context.Context, id string) (Event, error) {
	ev, err := scanEvent(s.db.QueryRowContext(ctx,
		`SELECT `+eventsColumns+` FROM events WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("查询事件 %s 失败: %w", id, err)
	}
	return ev, nil
}

// ListEvents 按状态过滤列出事件（创建时间倒序，新的在前），支持分页。
//
// status 传空串表示不过滤；limit 是单页条数，offset 是跳过的条数。
// 说明：这里按全字段返回（含 payload）。V0.1 数据量小，够用；
// 列表接口在 api 层会把 payload / headers 摘掉，只暴露摘要，避免把 1 MB 的大 payload 塞进响应。
func (s *Store) ListEvents(ctx context.Context, status string, limit, offset int) ([]Event, error) {
	query := `SELECT ` + eventsColumns + ` FROM events`
	var args []any
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询事件列表失败: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("读取事件列表失败: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// RetryEvent 人工手动重试——状态机第 7 条边（failed → pending）。
//
// 与抢占 / 结算 / 回收一样，这是一条带状态前置条件的【原子 UPDATE】：
//
//	WHERE id = ? AND status = 'failed'
//
// 守卫天然排除 delivered（正是业务规则 B11 要拦的）以及一切非终态。
// 语义（业务规则 B12 / B13）：
//   - 重置一整轮预算：attempts_count 清零、next_attempt_at = now（立即投递）
//   - 不删任何历史 Attempt：第 6 次物理投递就是第 6 条记录，审计链不断
//
// 返回 (true, nil) 表示真的把它拉回了 pending；
// 返回 (false, nil) 表示它当前不是 failed（可能 delivered，或还在 pending/processing/retrying），
// 由调用方回查一次来决定回 404（不存在）还是 409（状态不允许）。
func (s *Store) RetryEvent(ctx context.Context, id string, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE events
		    SET status = 'pending',
		        attempts_count = 0,
		        next_attempt_at = ?,
		        locked_until = NULL,
		        updated_at = ?
		  WHERE id = ?
		    AND status = 'failed'`,
		now, now, id)
	if err != nil {
		return false, fmt.Errorf("手动重试事件 %s 失败: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
