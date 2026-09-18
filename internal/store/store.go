// Package store 是数据库访问层：建表、迁移，以及三张表的读写。
//
// 第一原则（docs/04-data-model.md 第一节）：数据库是唯一真相来源。
// 所以这里的函数都是"从库里拿数据 / 往库里写数据"，不缓存、不维护内存副本。
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // 纯 Go 实现的 SQLite 驱动，不需要 CGO / gcc
)

// schemaSQL 在编译期把 schema.sql 的内容嵌进二进制。
// 好处：程序只有一个文件，运行时不需要在旁边放一个 .sql 文件。
//
//go:embed schema.sql
var schemaSQL string

// schemaVersion 与 schema.sql 的内容一一对应。
// 以后改表结构：把新语句追加进 schema.sql，再把这里 +1。
const schemaVersion = 1

// ErrNotFound 表示按主键查不到记录。
// 让调用方能用 errors.Is(err, store.ErrNotFound) 判断，而不是去比对错误字符串。
var ErrNotFound = errors.New("store: 记录不存在")

// Store 持有连接池。并发安全，整个进程共用一个即可。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库文件，并把表结构升到当前版本。
func Open(ctx context.Context, path string) (*Store, error) {
	// 三个 PRAGMA：
	//   journal_mode(WAL)  —— 读写不互相阻塞（默认的 rollback journal 会让读卡住写）
	//   busy_timeout(5000) —— 被别人占着写锁时最多等 5 秒，而不是立刻报 "database is locked"
	//   foreign_keys(1)    —— 打开外键约束。SQLite 默认是关的，不显式打开等于外键白写
	dsn := "file:" + path + "?" +
		"_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", path, err)
	}

	// SQLite 同一时刻只允许一个写者。V0.1 的 worker 数默认 1，
	// 把连接池压成 1 条最省心：不用担心 "database is locked"。
	// 等真要开 N 个 worker 时再来调这里（见 docs/01-business.md 参数表）。
	db.SetMaxOpenConns(1)

	// 立刻探一次连接：路径写错、文件没权限，应该在启动时就报出来，
	// 而不是等第一个请求打进来才发现。
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库 %s 失败: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭连接池。进程退出前调用。
func (s *Store) Close() error { return s.db.Close() }

// Ping 用于健康检查。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// migrate 把数据库表结构对齐到 schemaVersion（docs/04-data-model.md 第八节）。
//
// 为什么需要它：改字段时最省事的做法是"删库重来"，但那样数据就没了。
// 这里用 SQLite 自带的 PRAGMA user_version 记一个版本号，启动时比对：
//
//	版本相同 → 什么都不做
//	版本偏小 → 执行升级脚本，然后写上新版本号
//	版本偏大 → 报错退出（库比程序新，说明用户回退了程序版本，不能瞎动数据）
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("读取数据库版本失败: %w", err)
	}

	switch {
	case version == schemaVersion:
		return nil
	case version > schemaVersion:
		return fmt.Errorf(
			"数据库版本 (%d) 比程序期望的 (%d) 更新，拒绝启动以免损坏数据",
			version, schemaVersion)
	}

	// schema.sql 里全是 CREATE ... IF NOT EXISTS，重复执行是安全的。
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("执行建表脚本失败: %w", err)
	}

	// PRAGMA 不支持用 ? 传参，只能拼字符串。schemaVersion 是本包内的常量，
	// 不接受外部输入，所以这里没有注入风险。
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("写入数据库版本号失败: %w", err)
	}
	return nil
}
