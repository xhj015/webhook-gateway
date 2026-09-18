// Command webhook-gw 是 Webhook Gateway 服务的入口。
//
// 启动方式：
//
//	go run ./cmd/webhook-gw -db ./data.db -port 8080
//
// 它是"一个常驻的 HTTP 服务进程"，和 nginx / redis 那种东西是同一类形态：
// 启动后一直跑着，等别人来连，按 Ctrl+C 才停。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/xhj015/webhook-gateway/internal/api"
	"github.com/xhj015/webhook-gateway/internal/store"
)

func main() {
	// 配置只用标准库 flag，不引 viper：
	// V0.1 一共就三个参数，viper 的优先级解析和类型断言反而会增加要学的东西。
	var (
		dbPath = flag.String("db", "./data.db", "SQLite 数据库文件路径")
		port   = flag.Int("port", 8080, "HTTP 监听端口")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger, *dbPath, *port); err != nil {
		logger.Error("服务异常退出", "err", err)
		os.Exit(1)
	}
}

// run 把真正的启动逻辑收在这里，方便以后写测试或加优雅退出。
func run(logger *slog.Logger, dbPath string, port int) error {
	ctx := context.Background()

	// 打开库 = 连接 + 必要时建表（幂等），失败就直接退出，不要带着坏状态硬跑
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	addr := fmt.Sprintf(":%d", port)
	srv := &http.Server{
		Addr:    addr,
		Handler: api.NewRouter(api.NewHandler(st, logger)),

		// 这几个超时是防"慢速攻击"的：一个客户端只连上不干活，
		// 不带超时的服务器会一直占着连接不放。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("webhook-gateway 已启动", "addr", addr, "db", dbPath)

	// 优雅退出（收到信号后先把在途请求处理完再停）留到 M8 工程化那一周再做，
	// 现在要的就是"能跑起来"。
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 服务失败: %w", err)
	}
	return nil
}
