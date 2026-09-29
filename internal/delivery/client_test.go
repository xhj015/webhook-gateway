package delivery_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xhj015/webhook-gateway/internal/delivery"
)

// TestDeliverSuccess：2xx 必须判成功，且 payload / 透传头 / 两个 X-Webhook 头都要对。
func TestDeliverSuccess(t *testing.T) {
	var (
		gotPath        string
		gotEventID     string
		gotAttempt     string
		gotContentType string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotEventID = r.Header.Get("X-Webhook-Event-Id")
		gotAttempt = r.Header.Get("X-Webhook-Attempt")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := delivery.NewClient()
	out := c.Deliver(context.Background(),
		srv.URL+"/hooks/pay",
		[]byte(`{"orderId":"A-1"}`),
		map[string]string{"Content-Type": "application/json"},
		"ev_test0000000000000000001", 3)

	if !out.Success {
		t.Fatalf("2xx 应该判成功，实际: %+v", out)
	}
	if out.ResponseStatus != http.StatusOK {
		t.Errorf("ResponseStatus 应为 200，实际 %d", out.ResponseStatus)
	}
	if gotPath != "/hooks/pay" {
		t.Errorf("请求路径不对: %q", gotPath)
	}
	if gotEventID != "ev_test0000000000000000001" {
		t.Errorf("X-Webhook-Event-Id 不对: %q", gotEventID)
	}
	if gotAttempt != "3" {
		t.Errorf("X-Webhook-Attempt 不对: %q", gotAttempt)
	}
	if gotContentType != "application/json" {
		t.Errorf("透传头 Content-Type 丢了: %q", gotContentType)
	}
	if string(gotBody) != `{"orderId":"A-1"}` {
		t.Errorf("payload 被改动: %q", gotBody)
	}
}

// TestDeliverNon2xx：5xx 必须判失败，且状态码要如实带回（结算分支要用它记账）。
func TestDeliverNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	out := delivery.NewClient().Deliver(context.Background(),
		srv.URL, []byte("x"), nil, "ev_test", 1)

	if out.Success {
		t.Fatal("500 不应该判成功")
	}
	if out.ResponseStatus != http.StatusInternalServerError {
		t.Errorf("ResponseStatus 应为 500，实际 %d", out.ResponseStatus)
	}
	if !strings.Contains(out.Err, "非 2xx") {
		t.Errorf("失败原因应说明非 2xx，实际: %q", out.Err)
	}
}

// TestDeliverNetworkError：连不上的情况（这里用已取消的 context 模拟），
// ResponseStatus 必须是 0 —— 根本没有 HTTP 回应，不能瞎填一个状态码。
func TestDeliverNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消，模拟"请求根本没发出去"

	out := delivery.NewClient().Deliver(ctx,
		srv.URL, []byte("x"), nil, "ev_test", 1)

	if out.Success {
		t.Fatal("网络错误不应该判成功")
	}
	if out.ResponseStatus != 0 {
		t.Errorf("没有 HTTP 回应时 ResponseStatus 应为 0，实际 %d", out.ResponseStatus)
	}
	if out.Err == "" {
		t.Error("失败原因不能是空的")
	}
}

// TestParseForwardHeaders：正常 JSON 解析 + 坏数据兜底（坏头不能影响投递）。
func TestParseForwardHeaders(t *testing.T) {
	got := delivery.ParseForwardHeaders(`{"Content-Type":"application/json","X-Custom":"a, b"}`)
	if got["Content-Type"] != "application/json" || got["X-Custom"] != "a, b" {
		t.Errorf("解析结果不对: %+v", got)
	}

	broken := delivery.ParseForwardHeaders(`这不是JSON`)
	if len(broken) != 0 {
		t.Errorf("坏数据应返回空 map，实际: %+v", broken)
	}
}
