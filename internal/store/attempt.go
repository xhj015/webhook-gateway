// attempt 的读写。与 events / endpoints 不同，attempt 是【只追加】的审计记录：
// 一次投递发生了就是发生了，成功失败都记下来，之后只读不改（docs/01-business.md 第一节）。
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Attempt 是一次物理投递的审计记录，字段与 attempts 表的列一一对应。
type Attempt struct {
	ID             int64   // 纯内部自增序号，不出现在任何 URL 里
	EventID        string  // 属于哪条事件
	AttemptNo      int     // 该事件第几次物理投递，从 1 开始，永不清零
	Status         string  // success / failed
	ResponseStatus *int64  // HTTP 状态码；网络错误/超时时为 nil
	Error          *string // 失败原因原文；成功时为 nil
	DurationMS     int64   // 本次往返耗时（毫秒）
	StartedAt      int64   // Unix 秒
	FinishedAt     int64   // Unix 秒
}

// ListAttempts 返回某事件的全部投递尝试，按 attempt_no 升序（第 1 次在前）。
// 这正是 M5 查询接口 `GET /api/events/{id}/attempts` 背后的数据来源。
func (s *Store) ListAttempts(ctx context.Context, eventID string) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, event_id, attempt_no, status, response_status, error, duration_ms, started_at, finished_at
		   FROM attempts WHERE event_id = ? ORDER BY attempt_no`, eventID)
	if err != nil {
		return nil, fmt.Errorf("查询事件 %s 的投递历史失败: %w", eventID, err)
	}
	defer rows.Close()

	var out []Attempt
	for rows.Next() {
		var (
			a          Attempt
			respStatus sql.NullInt64
			errMsg     sql.NullString
		)
		if err := rows.Scan(&a.ID, &a.EventID, &a.AttemptNo, &a.Status, &respStatus, &errMsg,
			&a.DurationMS, &a.StartedAt, &a.FinishedAt); err != nil {
			return nil, fmt.Errorf("读取事件 %s 的投递历史失败: %w", eventID, err)
		}
		if respStatus.Valid {
			v := respStatus.Int64
			a.ResponseStatus = &v
		}
		if errMsg.Valid {
			v := errMsg.String
			a.Error = &v
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
