package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/xhj015/webhook-gateway/internal/api"
	"github.com/xhj015/webhook-gateway/internal/idgen"
	"github.com/xhj015/webhook-gateway/internal/store"
)

// newTestServer 开一个临时库 + 一套真实 handler + 路由，供端到端测接口。
func newTestServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := api.NewHandler(s, logger, nil)
	return s, api.NewRouter(h)
}

func doReq(h http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("解析响应 JSON 失败: %v（body=%s）", err, rec.Body.String())
	}
}

// createEndpointViaAPI 走 POST /api/endpoints 建一个端点，返回它的 id。
func createEndpointViaAPI(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := doReq(h, http.MethodPost, "/api/endpoints", []byte(`{"name":"订单系统","url":"http://localhost:9/hooks/pay"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建端点应 201，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &resp)
	return resp.ID
}

func seedEvent(t *testing.T, s *store.Store, status string, attempts int) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	ep := store.Endpoint{ID: idgen.Endpoint(), Name: "t", URL: "http://localhost:9/hook", Enabled: true, CreatedAt: now}
	if err := s.CreateEndpoint(ctx, ep); err != nil {
		t.Fatal(err)
	}
	ev := store.Event{ID: idgen.Event(), EndpointID: ep.ID, Status: status,
		Payload: []byte(`{"k":"v"}`), Headers: `{"Content-Type":"application/json"}`,
		AttemptsCount: attempts, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	return ev.ID
}

func TestHealthz(t *testing.T) {
	_, h := newTestServer(t)
	if rec := doReq(h, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz 应 200，实际 %d", rec.Code)
	}
}

func TestListEndpointsAPI(t *testing.T) {
	_, h := newTestServer(t)
	createEndpointViaAPI(t, h)
	createEndpointViaAPI(t, h)

	rec := doReq(h, http.MethodGet, "/api/endpoints", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", rec.Code)
	}
	var resp struct {
		Endpoints []struct {
			HookPath string `json:"hook_path"`
		} `json:"endpoints"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Endpoints) != 2 {
		t.Fatalf("应列出 2 个端点，实际 %d", len(resp.Endpoints))
	}
}

func TestDisableEndpointAPI(t *testing.T) {
	_, h := newTestServer(t)
	id := createEndpointViaAPI(t, h)

	// 首次停用 → 200，enabled=false
	rec := doReq(h, http.MethodPost, "/api/endpoints/"+id+"/disable", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("停用应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var ep struct {
		Enabled bool `json:"enabled"`
	}
	decodeJSON(t, rec, &ep)
	if ep.Enabled {
		t.Error("停用后 enabled 应为 false")
	}

	// 重复停用 → 仍然 200（幂等）
	if rec := doReq(h, http.MethodPost, "/api/endpoints/"+id+"/disable", nil); rec.Code != http.StatusOK {
		t.Errorf("重复停用应 200，实际 %d", rec.Code)
	}

	// 停用不存在的端点 → 404
	if rec := doReq(h, http.MethodPost, "/api/endpoints/ep_不存在/disable", nil); rec.Code != http.StatusNotFound {
		t.Errorf("停用不存在的端点应 404，实际 %d", rec.Code)
	}
}

func TestReceiveHookDisabledReturns410(t *testing.T) {
	_, h := newTestServer(t)
	id := createEndpointViaAPI(t, h)

	// 先停用
	if rec := doReq(h, http.MethodPost, "/api/endpoints/"+id+"/disable", nil); rec.Code != http.StatusOK {
		t.Fatalf("停用失败: %d", rec.Code)
	}

	// 再发事件 → 410（B2）
	rec := doReq(h, http.MethodPost, "/hooks/"+id, []byte(`{"x":1}`))
	if rec.Code != http.StatusGone {
		t.Fatalf("停用端点收事件应 410，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}

func TestReceiveHookNotFound(t *testing.T) {
	_, h := newTestServer(t)
	rec := doReq(h, http.MethodPost, "/hooks/ep_不存在", []byte(`{"x":1}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的端点应 404，实际 %d", rec.Code)
	}
}

func TestReceiveHookCreatesEvent(t *testing.T) {
	s, h := newTestServer(t)
	id := createEndpointViaAPI(t, h)

	rec := doReq(h, http.MethodPost, "/hooks/"+id, []byte(`{"orderId":"A-1"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("接收事件应 202，实际 %d", rec.Code)
	}
	var resp struct {
		EventID string `json:"event_id"`
	}
	decodeJSON(t, rec, &resp)
	if resp.EventID == "" {
		t.Fatal("202 响应应带 event_id")
	}

	// 事件应落库为 pending
	ev, err := s.GetEvent(context.Background(), resp.EventID)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != store.StatusPending {
		t.Errorf("落库状态应为 pending，实际 %s", ev.Status)
	}
}

func TestListEventsAPI(t *testing.T) {
	s, h := newTestServer(t)
	failedID := seedEvent(t, s, store.StatusFailed, 5)
	_ = seedEvent(t, s, store.StatusPending, 0)

	// 不过滤 → 2 条
	if rec := doReq(h, http.MethodGet, "/api/events", nil); rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", rec.Code)
	}

	// 按 failed 过滤 → 1 条，且是那条 failed
	rec := doReq(h, http.MethodGet, "/api/events?status=failed", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("过滤列表应 200，实际 %d", rec.Code)
	}
	var resp struct {
		Events []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"events"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Events) != 1 || resp.Events[0].ID != failedID {
		t.Errorf("过滤 failed 应只返回 %s，实际 %+v", failedID, resp.Events)
	}

	// 非法 status → 400
	if rec := doReq(h, http.MethodGet, "/api/events?status=bogus", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("非法 status 应 400，实际 %d", rec.Code)
	}
}

func TestGetEventAPI(t *testing.T) {
	s, h := newTestServer(t)
	id := seedEvent(t, s, store.StatusPending, 0)

	rec := doReq(h, http.MethodGet, "/api/events/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情应 200，实际 %d", rec.Code)
	}
	var detail struct {
		ID         string          `json:"id"`
		PayloadB64 string          `json:"payload_base64"`
		Headers    json.RawMessage `json:"headers"`
	}
	decodeJSON(t, rec, &detail)
	if detail.ID != id {
		t.Errorf("详情 id 不对: %s", detail.ID)
	}
	if detail.PayloadB64 == "" {
		t.Error("详情应带 payload_base64")
	}
	if len(detail.Headers) == 0 {
		t.Error("详情应带 headers")
	}

	// 不存在 → 404
	if rec := doReq(h, http.MethodGet, "/api/events/ev_不存在", nil); rec.Code != http.StatusNotFound {
		t.Errorf("不存在的事件详情应 404，实际 %d", rec.Code)
	}
}

func TestListAttemptsAPI(t *testing.T) {
	s, h := newTestServer(t)
	id := seedEvent(t, s, store.StatusPending, 0)

	// 有事件但无 attempt → 200 空列表
	rec := doReq(h, http.MethodGet, "/api/events/"+id+"/attempts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("投递历史应 200，实际 %d", rec.Code)
	}

	// 事件不存在 → 404
	if rec := doReq(h, http.MethodGet, "/api/events/ev_不存在/attempts", nil); rec.Code != http.StatusNotFound {
		t.Errorf("不存在事件的投递历史应 404，实际 %d", rec.Code)
	}
}

func TestRetryEventAPI(t *testing.T) {
	s, h := newTestServer(t)

	// failed → 200，状态变 pending
	failedID := seedEvent(t, s, store.StatusFailed, 5)
	rec := doReq(h, http.MethodPost, "/api/events/"+failedID+"/retry", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed 重试应 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
	}
	decodeJSON(t, rec, &resp)
	if resp.Status != store.StatusPending {
		t.Errorf("重试后状态应为 pending，实际 %s", resp.Status)
	}

	// delivered → 409（B11）
	deliveredID := seedEvent(t, s, store.StatusDelivered, 1)
	if rec := doReq(h, http.MethodPost, "/api/events/"+deliveredID+"/retry", nil); rec.Code != http.StatusConflict {
		t.Errorf("delivered 重试应 409，实际 %d", rec.Code)
	}

	// 不存在 → 404
	if rec := doReq(h, http.MethodPost, "/api/events/ev_不存在/retry", nil); rec.Code != http.StatusNotFound {
		t.Errorf("不存在事件重试应 404，实际 %d", rec.Code)
	}
}
