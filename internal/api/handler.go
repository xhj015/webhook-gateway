// Package api 是 HTTP 接入层：路由、请求解析、响应编码。
//
// 分层约定：handler 只负责"听懂 HTTP"，业务动作全部交给 store；
// handler 里不写 SQL，store 里不碰 http.ResponseWriter。
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/xhj015/webhook-gateway/internal/store"
)

// Handler 持有 handler 需要的一切依赖。
// 用结构体而不是一堆全局变量：依赖一目了然，测试时能换掉。
type Handler struct {
	store *store.Store
	log   *slog.Logger

	// wake 是唤醒 worker 的回调（实现是 worker.Notifier.Wake）。
	// 存函数而不是具体类型：handler 层不需要知道 worker 包的存在，
	// 依赖方向保持 api → store 单向；测试时塞个空函数即可。
	wake func()
}

// NewHandler 组装一个 Handler。wake 允许传 nil（没有 worker 时静默跳过唤醒）。
func NewHandler(st *store.Store, log *slog.Logger, wake func()) *Handler {
	return &Handler{store: st, log: log, wake: wake}
}

// errorResponse 所有错误响应的统一形状。
// 只给一个字段，调用方判断失败原因时不用猜是哪个 key。
type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON 所有成功响应的统一出口。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 状态码和响应头早就发出去了，这里改不了任何东西，只能记一笔
		slog.Error("写响应体失败", "err", err)
	}
}

// writeError 统一的失败出口。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
