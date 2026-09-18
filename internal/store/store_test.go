package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xhj015/webhook-gateway/internal/idgen"
	"github.com/xhj015/webhook-gateway/internal/store"
)

// TestMigrateIsIdempotent：对同一个库连续 Open 两次，不应该报错，
// 也不应该把版本号搞乱（迁移必须是可重复执行的）。
func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	s1, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("第一次打开失败: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	s2, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("第二次打开失败（说明迁移不幂等）: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
}

// TestCreateEventAndReadBack：W3 垂直切片的数据半边——
// 写进库里的事件，必须能原样读回来，而且 Byte 级别不能变。
func TestCreateEventAndReadBack(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	now := time.Now().Unix()

	ep := store.Endpoint{
		ID:        idgen.Endpoint(),
		Name:      "订单系统",
		URL:       "http://localhost:9000/hooks/pay",
		Enabled:   true,
		CreatedAt: now,
	}
	if err := s.CreateEndpoint(ctx, ep); err != nil {
		t.Fatalf("创建端点失败: %v", err)
	}

	// 故意带上前导空格、中文和一段非规范 JSON，验证"原样透传"：
	// 网关不允许在存储环节改动用户的字节。
	rawPayload := []byte("  {\"金额\": 100, \"orderId\":\"A-1\"}  ")

	ev := store.Event{
		ID:            idgen.Event(),
		EndpointID:    ep.ID,
		Status:        store.StatusPending,
		Payload:       rawPayload,
		Headers:       `{"Content-Type":"application/json"}`,
		AttemptsCount: 0,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreateEvent(ctx, ev); err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}

	got, err := s.GetEvent(ctx, ev.ID)
	if err != nil {
		t.Fatalf("读回事件失败: %v", err)
	}

	if string(got.Payload) != string(rawPayload) {
		t.Errorf("payload 被改动了\n写入: %q\n读回: %q", rawPayload, got.Payload)
	}
	if got.Status != store.StatusPending {
		t.Errorf("状态不对：想要 %q，实际 %q", store.StatusPending, got.Status)
	}
	if got.AttemptsCount != 0 {
		t.Errorf("新事件的 attempts_count 应该是 0，实际 %d", got.AttemptsCount)
	}
	if got.EndpointID != ep.ID {
		t.Errorf("endpoint_id 不对：想要 %q，实际 %q", ep.ID, got.EndpointID)
	}

	// ID 格式：前缀 + 24 位十六进制
	if !strings.HasPrefix(got.ID, "ev_") || len(got.ID) != 27 {
		t.Errorf("事件 ID 格式不对: %q", got.ID)
	}

	// 查不到的 ID 必须给出 ErrNotFound，而不是别的什么错误
	if _, err := s.GetEvent(ctx, "ev_不存在的ID"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("查不到时应该返回 ErrNotFound，实际: %v", err)
	}

	// 端点也一样能读回来
	gotEP, err := s.GetEndpoint(ctx, ep.ID)
	if err != nil {
		t.Fatalf("读回端点失败: %v", err)
	}
	if !gotEP.Enabled || gotEP.URL != ep.URL {
		t.Errorf("端点内容不对: %+v", gotEP)
	}
	if _, err := s.GetEndpoint(ctx, "ep_不存在的ID"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("查不到端点时应该返回 ErrNotFound，实际: %v", err)
	}
}
