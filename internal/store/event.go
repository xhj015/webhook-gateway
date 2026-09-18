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

// GetEvent 按 ID 查事件。查不到返回 ErrNotFound。
//
// W3 只被测试用到。等 M5（W11）做查询接口时，它会被 handler 直接复用。
func (s *Store) GetEvent(ctx context.Context, id string) (Event, error) {
	var (
		ev          Event
		lockedUntil sql.NullInt64
		lastError   sql.NullString
		deliveredAt sql.NullInt64
	)

	err := s.db.QueryRowContext(ctx,
		`SELECT id, endpoint_id, status, payload, headers, attempts_count,
		        next_attempt_at, locked_until, last_error, created_at, updated_at, delivered_at
		   FROM events WHERE id = ?`, id).
		Scan(&ev.ID, &ev.EndpointID, &ev.Status, &ev.Payload, &ev.Headers, &ev.AttemptsCount,
			&ev.NextAttemptAt, &lockedUntil, &lastError, &ev.CreatedAt, &ev.UpdatedAt, &deliveredAt)

	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("查询事件 %s 失败: %w", id, err)
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
