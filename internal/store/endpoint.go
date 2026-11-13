package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Endpoint 是投递目标：一条 URL，长期存在，可停用但不可删除（V0.1）。
// 对应文档：docs/04-data-model.md 第二节。
type Endpoint struct {
	ID         string
	Name       string
	URL        string
	Enabled    bool
	CreatedAt  int64
	DisabledAt *int64 // nil 表示未停用
}

// CreateEndpoint 插入一个端点。ID 由调用方用 idgen 生成后传进来。
func (s *Store) CreateEndpoint(ctx context.Context, ep Endpoint) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO endpoints (id, name, url, enabled, created_at, disabled_at)
		 VALUES (?, ?, ?, ?, ?, NULL)`,
		ep.ID, ep.Name, ep.URL, boolToInt(ep.Enabled), ep.CreatedAt)
	if err != nil {
		return fmt.Errorf("创建端点失败: %w", err)
	}
	return nil
}

// GetEndpoint 按 ID 查端点。查不到返回 ErrNotFound。
func (s *Store) GetEndpoint(ctx context.Context, id string) (Endpoint, error) {
	var (
		ep         Endpoint
		enabled    int
		disabledAt sql.NullInt64 // 可空列不能直接扫进 *int64，要用 sql.Null*
	)

	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, url, enabled, created_at, disabled_at FROM endpoints WHERE id = ?`, id).
		Scan(&ep.ID, &ep.Name, &ep.URL, &enabled, &ep.CreatedAt, &disabledAt)

	if errors.Is(err, sql.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	if err != nil {
		return Endpoint{}, fmt.Errorf("查询端点 %s 失败: %w", id, err)
	}

	ep.Enabled = enabled != 0
	if disabledAt.Valid {
		v := disabledAt.Int64
		ep.DisabledAt = &v
	}
	return ep, nil
}

// ListEndpoints 返回全部端点，按创建时间升序（最早创建的排前面）。
func (s *Store) ListEndpoints(ctx context.Context) ([]Endpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, url, enabled, created_at, disabled_at FROM endpoints ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("查询端点列表失败: %w", err)
	}
	defer rows.Close()

	var out []Endpoint
	for rows.Next() {
		var (
			ep         Endpoint
			enabled    int
			disabledAt sql.NullInt64
		)
		if err := rows.Scan(&ep.ID, &ep.Name, &ep.URL, &enabled, &ep.CreatedAt, &disabledAt); err != nil {
			return nil, fmt.Errorf("读取端点列表失败: %w", err)
		}
		ep.Enabled = enabled != 0
		if disabledAt.Valid {
			v := disabledAt.Int64
			ep.DisabledAt = &v
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// DisableEndpoint 停用一个端点（业务规则 B2 / B10）。
//
// 语义：停用只影响【新事件】，已经落库的在途事件照常投完——所以这里只改 endpoints 一行，
// 不动 events 里的任何记录。
//
// 用带守卫的 UPDATE（WHERE enabled = 1）保证幂等：
//
//	返回 (true, nil)  —— 这次真的把它从启用改成了停用
//	返回 (false, nil) —— 它本来就已经停用，重复停用无副作用
//
// 至于"端点根本不存在"，调用方应先走 GetEndpoint 拿到 404，再调本函数。
func (s *Store) DisableEndpoint(ctx context.Context, id string, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE endpoints SET enabled = 0, disabled_at = ? WHERE id = ? AND enabled = 1`,
		now, id)
	if err != nil {
		return false, fmt.Errorf("停用端点 %s 失败: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// boolToInt：SQLite 没有布尔类型，Go 侧用 true/false，落库时转成 1/0。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
