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
	"os/signal"
	"syscall"
	"time"

	"github.com/xhj015/webhook-gateway/internal/api"
	"github.com/xhj015/webhook-gateway/internal/delivery"
	"github.com/xhj015/webhook-gateway/internal/store"
	"github.com/xhj015/webhook-gateway/internal/worker"
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
	// ctx 生命周期 = 进程生命周期：收到 Ctrl+C / SIGTERM 时被取消。
	// worker 的 Run 里 select 监听 ctx.Done()，取消即退出；HTTP 侧靠下面 srv.Shutdown 停。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 打开库 = 连接 + 必要时建表（幂等），失败就直接退出，不要带着坏状态硬跑
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// 后台 worker 与 HTTP 服务跑在同一个进程里，但互不依赖——
	// worker 的每一步都只跟数据库打交道，HTTP handler 只负责往库里写。
	// 两者唯一的接触点是 wake 信号（落库成功后喊一嗓子），这正是
	// "数据库是唯一真相来源"在进程结构上的体现。
	notifier := worker.NewNotifier()
	go worker.New(st, delivery.NewClient(), logger, notifier.C()).Run(ctx)
	logger.Info("后台 worker 已启动", "tick_interval", "1s")

	addr := fmt.Sprintf(":%d", port)
	srv := &http.Server{
		Addr:    addr,
		Handler: api.NewRouter(api.NewHandler(st, logger, notifier.Wake)),

		// 这几个超时是防"慢速攻击"的：一个客户端只连上不干活，
		// 不带超时的服务器会一直占着连接不放。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 优雅退出：ListenAndServe 会阻塞，放到独立 goroutine 里，
	// 主 goroutine 用 select 同时等"服务自己出错"和"收到退出信号"。
	// 收到信号后先给在途请求 5 秒的收尾窗口，再关服务器；worker 因 ctx 取消同步退出。
	errCh := make(chan error, 1)
	go func() {
		logger.Info("webhook-gateway 已启动", "addr", addr, "db", dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务失败: %w", err)
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅停机")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("优雅停机失败: %w", err)
		}
		logger.Info("HTTP 服务已停机")
		return nil
	}
}
