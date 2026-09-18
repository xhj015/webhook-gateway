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

// boolToInt：SQLite 没有布尔类型，Go 侧用 true/false，落库时转成 1/0。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
