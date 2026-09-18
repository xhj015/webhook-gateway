package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// NewRouter 装配全部路由。接口清单见 docs/01-business.md 与后续的 docs/05-api.md。
//
// W3 只开三个口子：
//
//	GET  /healthz              存活探针
//	POST /api/endpoints        创建投递目标（没有它就没法演示接收）
//	POST /hooks/{endpointID}   事件入口 → 落库 → 202
//
// 其余接口按执行计划在各里程碑里逐个加上。
func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(h.accessLog)

	r.Get("/healthz", h.health)

	r.Route("/api", func(r chi.Router) {
		r.Post("/endpoints", h.createEndpoint)
	})

	r.Post("/hooks/{endpointID}", h.receiveHook)

	return r
}

// health 存活探针：进程活着 ≠ 能干活，所以要顺便摸一下数据库。
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Ping(r.Context()); err != nil {
		h.log.Error("健康检查失败", "err", err)
		writeError(w, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// accessLog 是 chi 意义上的"中间件"：包在真正的 handler 外面，
// 在请求进来前和响应写完之后各插一脚。这里只做一件事——记一条结构化访问日志。
//
// 为什么不用 chi 自带的 middleware.Logger：它打的是给人看的文本行，
// 而 slog 打的是 key=value，以后要按 event_id / status 过滤日志时省事得多。
func (h *Handler) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		h.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// statusRecorder 记下"实际写出去的状态码"。
// 因为 http.ResponseWriter 本身不提供读回状态码的方法，只能自己包一层记下来。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
