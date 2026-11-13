package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xhj015/webhook-gateway/internal/idgen"
	"github.com/xhj015/webhook-gateway/internal/store"
)

// maxPayloadBytes 是单条事件 payload 的上限，业务规则 B3 定的 1 MB。
const maxPayloadBytes = 1 << 20

// receiveHookResponse 是 202 的响应体。
// 业务规则 B5：必须带 event_id，调用方靠它对账（查投递结果、必要时手动重试）。
type receiveHookResponse struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"`
}

// receiveHook 处理 POST /hooks/{endpointID}——事件的出生地（docs/02-flow.md 2.1 节）。
//
// 接收侧校验三步（业务规则 B1 / B2 / B3），顺序不能变：
//
//	① 端点不存在   → 404
//	② 端点已停用   → 410（B2：停用的端点拒收新事件）
//	③ body 超 1 MB → 413
//
// 校验全过后才是真正的"接收"：
//
//	④ 落库（pending）→ 失败 500，绝不能返回 202
//	⑤ 唤醒 worker    → 事务提交之后才发信号
//	⑥ 返回 202 + event_id
//
// 核心是第 ④ 步永远排在最后一步之前：**202 只代表"我记住了"，不代表"我送到了"**。
func (h *Handler) receiveHook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	endpointID := chi.URLParam(r, "endpointID")

	// ① 校验端点（B1）
	ep, err := h.store.GetEndpoint(ctx, endpointID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "端点不存在: "+endpointID)
		return
	}
	if err != nil {
		h.log.Error("查询端点失败", "err", err, "endpoint_id", endpointID)
		writeError(w, http.StatusInternalServerError, "查询端点失败")
		return
	}

	// ② 端点已停用则拒收（B2 → 410）。
	// 停用只影响"新事件"：已经落库的在途事件照常投完（B10）。
	// 这里只在入口拦这一下，不改库、不影响在途事件。
	if !ep.Enabled {
		writeError(w, http.StatusGone, "端点已停用: "+endpointID)
		return
	}

	// ③ 读原始 body。用 MaxBytesReader 而不是直接 io.ReadAll：
	// 后者会把任意大小的请求体读进内存，一条 1 GB 的请求就能把进程撑死。
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPayloadBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload 超过 1 MB 上限")
			return
		}
		writeError(w, http.StatusBadRequest, "读取请求体失败")
		return
	}

	// ④ 落库（B4）。注意 body 是 []byte 原文，不做任何解析——
	// 网关是透明管道，不允许改动用户的字节（docs/04-data-model.md 4.1 节）。
	now := time.Now().Unix()
	ev := store.Event{
		ID:            idgen.Event(),
		EndpointID:    ep.ID,
		Status:        store.StatusPending,
		Payload:       body,
		Headers:       collectForwardHeaders(r.Header),
		AttemptsCount: 0, // 本轮预算一次没用
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := h.store.CreateEvent(ctx, ev); err != nil {
		// 库都没写进去，就绝对不能回 202——那等于承诺了一件不存在的事
		h.log.Error("事件落库失败", "err", err, "endpoint_id", ep.ID)
		writeError(w, http.StatusInternalServerError, "写入数据库失败")
		return
	}

	// ⑤ 事务已提交，现在才可以发唤醒信号（W4 接上）。
	// 顺序要点：信号必须晚于落库。反过来等于让 worker 去捞一条还不存在的记录。
	// 信号本身可丢（Notifier 容量 1，满了自动丢弃）——worker 每秒都有兜底扫描。
	if h.wake != nil {
		h.wake()
	}

	h.log.Info("事件已接收",
		"event_id", ev.ID,
		"endpoint_id", ep.ID,
		"bytes", len(body),
	)

	// ⑥ 202 Accepted = 请求已受理，但处理还没完成。这个状态码是整条链路的语义锚点。
	writeJSON(w, http.StatusAccepted, receiveHookResponse{EventID: ev.ID, Status: ev.Status})
}

// hopByHopHeaders 是描述"这一段连接"的请求头，转发到下一段没有意义。
// 依据 RFC 9110 的 hop-by-hop 头定义（旧编号 RFC 2616）。
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// collectForwardHeaders 把客户端请求头序列化成 JSON，存进 events.headers。
//
// 为什么现在就要做：W4 投递时要还原这批头，否则目标方可能看不懂 body
// （比如丢了 Content-Type，对方就不知道这是一段 JSON）。
//
// 剔除两类：
//   - hop-by-hop 头：它们只对当前这条连接有意义
//   - Host / Content-Length：Go 的 http.Client 会按新请求自己重算，手工带上反而冲突
//
// 注意：投递时代码还会覆盖上 X-Webhook-Event-Id / X-Webhook-Attempt 两个头（B9），
// 免得调用方伪造。
func collectForwardHeaders(h http.Header) string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if hopByHopHeaders[k] || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		out[k] = strings.Join(v, ", ")
	}

	b, err := json.Marshal(out)
	if err != nil {
		// map[string]string 不可能序列化失败，这里只是形式上的兜底
		return "{}"
	}
	return string(b)
}
