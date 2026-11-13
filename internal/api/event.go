// M5 查询与手动重试的 handler：事件列表 / 详情 / 投递历史 / 手动重试。
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xhj015/webhook-gateway/internal/store"
)

// eventSummary 是列表用的"轻量"事件表示：不含 payload / headers。
// 为什么不带 payload：一条事件最多 1 MB，列表里带上会把响应体撑爆；详情走 getEvent 单独取。
type eventSummary struct {
	ID            string  `json:"id"`
	EndpointID    string  `json:"endpoint_id"`
	Status        string  `json:"status"`
	AttemptsCount int     `json:"attempts_count"`
	NextAttemptAt int64   `json:"next_attempt_at"`
	LastError     *string `json:"last_error,omitempty"`
	CreatedAt     int64   `json:"created_at"`
	UpdatedAt     int64   `json:"updated_at"`
	DeliveredAt   *int64  `json:"delivered_at,omitempty"`
}

// eventDetail 是详情用的完整表示：在摘要基础上带上 payload（base64）与 headers（原样 JSON）。
type eventDetail struct {
	eventSummary
	PayloadB64 string          `json:"payload_base64"`
	Headers    json.RawMessage `json:"headers"`
}

// attemptResponse 是一条投递尝试的对外表示。
type attemptResponse struct {
	AttemptNo      int     `json:"attempt_no"`
	Status         string  `json:"status"`
	ResponseStatus *int64  `json:"response_status"`
	Error          *string `json:"error,omitempty"`
	DurationMS     int64   `json:"duration_ms"`
	StartedAt      int64   `json:"started_at"`
	FinishedAt     int64   `json:"finished_at"`
}

func toEventSummary(ev store.Event) eventSummary {
	return eventSummary{
		ID:            ev.ID,
		EndpointID:    ev.EndpointID,
		Status:        ev.Status,
		AttemptsCount: ev.AttemptsCount,
		NextAttemptAt: ev.NextAttemptAt,
		LastError:     ev.LastError,
		CreatedAt:     ev.CreatedAt,
		UpdatedAt:     ev.UpdatedAt,
		DeliveredAt:   ev.DeliveredAt,
	}
}

func validEventStatus(s string) bool {
	switch s {
	case store.StatusPending, store.StatusProcessing, store.StatusRetrying, store.StatusDelivered, store.StatusFailed:
		return true
	}
	return false
}

// listEvents 处理 GET /api/events：按状态过滤、分页列出事件（新的在前）。
//
//	查询参数：status（可选，五个状态之一）、limit（默认 20，1~100）、offset（默认 0）。
func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	status := q.Get("status")
	if status != "" && !validEventStatus(status) {
		writeError(w, http.StatusBadRequest, "status 必须是 pending/processing/retrying/delivered/failed 之一")
		return
	}

	limit := 20
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "limit 必须是 1~100 的整数")
			return
		}
		limit = n
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset 必须是非负整数")
			return
		}
		offset = n
	}

	evs, err := h.store.ListEvents(r.Context(), status, limit, offset)
	if err != nil {
		h.log.Error("查询事件列表失败", "err", err)
		writeError(w, http.StatusInternalServerError, "查询事件列表失败")
		return
	}

	out := make([]eventSummary, 0, len(evs))
	for _, ev := range evs {
		out = append(out, toEventSummary(ev))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// getEvent 处理 GET /api/events/{id}：单条事件的完整详情。
//
// payload 用 base64 返回：payload 是原始字节（BLOB），可能是非 UTF-8 内容，
// 直接当字符串塞进 JSON 会损坏数据，base64 是唯一无损的表示。
func (h *Handler) getEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	ev, err := h.store.GetEvent(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "事件不存在: "+id)
		return
	}
	if err != nil {
		h.log.Error("查询事件失败", "err", err, "event_id", id)
		writeError(w, http.StatusInternalServerError, "查询事件失败")
		return
	}

	// headers 是接收时存下的 JSON 字符串，原样塞回响应体（不解析，保持透明管道）
	headers := json.RawMessage(ev.Headers)
	if len(headers) == 0 {
		headers = json.RawMessage("{}")
	}

	writeJSON(w, http.StatusOK, eventDetail{
		eventSummary: toEventSummary(ev),
		PayloadB64:   base64.StdEncoding.EncodeToString(ev.Payload),
		Headers:      headers,
	})
}

// listAttempts 处理 GET /api/events/{id}/attempts：某事件的全部投递尝试（审计记录）。
func (h *Handler) listAttempts(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// 先确认事件存在，给 404 而不是一个容易误导人的空列表
	if _, err := h.store.GetEvent(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "事件不存在: "+id)
		return
	} else if err != nil {
		h.log.Error("查询事件失败", "err", err, "event_id", id)
		writeError(w, http.StatusInternalServerError, "查询事件失败")
		return
	}

	atts, err := h.store.ListAttempts(r.Context(), id)
	if err != nil {
		h.log.Error("查询投递历史失败", "err", err, "event_id", id)
		writeError(w, http.StatusInternalServerError, "查询投递历史失败")
		return
	}

	out := make([]attemptResponse, 0, len(atts))
	for _, a := range atts {
		out = append(out, attemptResponse{
			AttemptNo:      a.AttemptNo,
			Status:         a.Status,
			ResponseStatus: a.ResponseStatus,
			Error:          a.Error,
			DurationMS:     a.DurationMS,
			StartedAt:      a.StartedAt,
			FinishedAt:     a.FinishedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempts": out})
}

// retryEvent 处理 POST /api/events/{id}/retry：人工手动重试一条 failed 事件。
//
// 业务规则 B11 / B12 / B13：
//   - 只对 failed 开放；对 delivered 返回 409（不重投已成功的事件）
//   - 语义是"重置一整轮预算"：attempts_count 清零、立即投递
//   - 不删任何历史 Attempt：第 6 次物理投递就是第 6 条记录
//
// 实现要点：直接用带守卫的 UPDATE 尝试转状态（failed → pending），
// 判断和修改在一条 SQL 里完成（docs/02-flow.md 3.5 防线 1）。
// 没转成（RowsAffected=0）再回查原因——而不是先查状态再改（check-then-act 竞态）。
func (h *Handler) retryEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	changed, err := h.store.RetryEvent(ctx, id, time.Now().Unix())
	if err != nil {
		h.log.Error("手动重试失败", "err", err, "event_id", id)
		writeError(w, http.StatusInternalServerError, "手动重试失败")
		return
	}
	if !changed {
		// 没转成：要么不存在，要么当前不是 failed。回查一次区分两种情况。
		ev, err := h.store.GetEvent(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "事件不存在: "+id)
			return
		}
		if err != nil {
			h.log.Error("查询事件失败", "err", err, "event_id", id)
			writeError(w, http.StatusInternalServerError, "查询事件失败")
			return
		}
		// B11：对 delivered 以及一切非 failed 状态，一律 409
		writeError(w, http.StatusConflict, "只有 failed 状态的事件可以手动重试，当前状态: "+ev.Status)
		return
	}

	// 拉回 pending 后喊醒 worker，让它立刻把这条捞起来投
	if h.wake != nil {
		h.wake()
	}

	ev, err := h.store.GetEvent(ctx, id)
	if err != nil {
		h.log.Error("查询事件失败", "err", err, "event_id", id)
		writeError(w, http.StatusInternalServerError, "查询事件失败")
		return
	}
	h.log.Info("事件已手动重试", "event_id", id)
	writeJSON(w, http.StatusOK, toEventSummary(ev))
}
