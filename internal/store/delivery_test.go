package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xhj015/webhook-gateway/internal/idgen"
	"github.com/xhj015/webhook-gateway/internal/store"
)

// openTest 开一个临时库的 store，测试结束自动关闭。
func openTest(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedPending 建一个端点 + 一条 attempts_count=0 的 pending 事件。
func seedPending(t *testing.T, s *store.Store) (epID, evID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	ep := store.Endpoint{ID: idgen.Endpoint(), Name: "t", URL: "http://localhost:9/hook", Enabled: true, CreatedAt: now}
	if err := s.CreateEndpoint(ctx, ep); err != nil {
		t.Fatal(err)
	}
	ev := store.Event{ID: idgen.Event(), EndpointID: ep.ID, Status: store.StatusPending,
		Payload: []byte("x"), Headers: "{}", AttemptsCount: 0, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	return ep.ID, ev.ID
}

// seedWithCount 与 seedPending 类似，但允许指定初始 attempts_count（用于测"第几次失败"分支）。
func seedWithCount(t *testing.T, s *store.Store, count int) (epID, evID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	ep := store.Endpoint{ID: idgen.Endpoint(), Name: "t", URL: "http://localhost:9/hook", Enabled: true, CreatedAt: now}
	if err := s.CreateEndpoint(ctx, ep); err != nil {
		t.Fatal(err)
	}
	ev := store.Event{ID: idgen.Event(), EndpointID: ep.ID, Status: store.StatusPending,
		Payload: []byte("x"), Headers: "{}", AttemptsCount: count, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	return ep.ID, ev.ID
}

func mustGet(t *testing.T, s *store.Store, id string) store.Event {
	t.Helper()
	ev, err := s.GetEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestClaimEvent：原子抢占应把状态改成 processing 并写入租约；同一事件不能二次抢占。
func TestClaimEvent(t *testing.T) {
	s := openTest(t)
	_, evID := seedPending(t, s)
	ctx := context.Background()
	now := time.Now().Unix()

	claimed, err := s.ClaimEvent(ctx, evID, now, 60)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("首次抢占应该成功")
	}

	got := mustGet(t, s, evID)
	if got.Status != store.StatusProcessing {
		t.Errorf("状态应为 processing，实际 %s", got.Status)
	}
	if got.LockedUntil == nil || *got.LockedUntil != now+60 {
		t.Errorf("locked_until 应为 %d，实际 %v", now+60, got.LockedUntil)
	}

	// processing 的事件不应再出现在"到期可投递"列表里
	ids, err := s.DueEventIDs(ctx, now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("processing 不应被捞到，实际 %v", ids)
	}

	// 再抢一次应失败（已被自己占着）——这是并发安全的关键
	again, err := s.ClaimEvent(ctx, evID, now, 60)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("已被抢占，第二次应抢不到")
	}
}

// TestSettleDeliverySuccess：成功投递应转 delivered，写 1 条 attempt，delivered_at 落库。
func TestSettleDeliverySuccess(t *testing.T) {
	s := openTest(t)
	_, evID := seedPending(t, s)
	ctx := context.Background()
	now := time.Now().Unix()

	if _, err := s.ClaimEvent(ctx, evID, now, 60); err != nil {
		t.Fatal(err)
	}
	attemptNo, err := s.NextAttemptNo(ctx, evID)
	if err != nil {
		t.Fatal(err)
	}
	if attemptNo != 1 {
		t.Fatalf("首次 attempt_no 应为 1，实际 %d", attemptNo)
	}

	out := store.DeliveryOutcome{Success: true, ResponseStatus: 200, DurationMS: 10}
	if err := s.SettleDelivery(ctx, mustGet(t, s, evID), attemptNo, out, now); err != nil {
		t.Fatal(err)
	}

	got := mustGet(t, s, evID)
	if got.Status != store.StatusDelivered {
		t.Errorf("状态应为 delivered，实际 %s", got.Status)
	}
	if got.DeliveredAt == nil {
		t.Error("delivered_at 不应为 nil")
	}
	if got.AttemptsCount != 1 {
		t.Errorf("attempts_count 应为 1，实际 %d", got.AttemptsCount)
	}
}

// TestSettleDeliveryRetryThenFail：失败且次数<5 → retrying（退避到未来）；次数>=5 → failed。
func TestSettleDeliveryRetryThenFail(t *testing.T) {
	s := openTest(t)
	out := store.DeliveryOutcome{Success: false, ResponseStatus: 500, Err: "HTTP 500", DurationMS: 10}

	// 场景一：第 4 次失败（当前 attempts_count=3）→ retrying
	_, evID := seedWithCount(t, s, 3)
	now := time.Now().Unix()
	if _, err := s.ClaimEvent(context.Background(), evID, now, 60); err != nil {
		t.Fatal(err)
	}
	an, _ := s.NextAttemptNo(context.Background(), evID)
	if err := s.SettleDelivery(context.Background(), mustGet(t, s, evID), an, out, now); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, s, evID)
	if got.Status != store.StatusRetrying {
		t.Errorf("次数<5 应为 retrying，实际 %s", got.Status)
	}
	if got.NextAttemptAt <= now {
		t.Errorf("retrying 的 next_attempt_at 应在未来，实际 %d <= %d", got.NextAttemptAt, now)
	}
	if got.AttemptsCount != 4 {
		t.Errorf("attempts_count 应为 4，实际 %d", got.AttemptsCount)
	}

	// 场景二：第 5 次失败（当前 attempts_count=4）→ failed（终态）
	_, evID2 := seedWithCount(t, s, 4)
	now2 := time.Now().Unix()
	if _, err := s.ClaimEvent(context.Background(), evID2, now2, 60); err != nil {
		t.Fatal(err)
	}
	an2, _ := s.NextAttemptNo(context.Background(), evID2)
	if err := s.SettleDelivery(context.Background(), mustGet(t, s, evID2), an2, out, now2); err != nil {
		t.Fatal(err)
	}
	got2 := mustGet(t, s, evID2)
	if got2.Status != store.StatusFailed {
		t.Errorf("次数>=5 应为 failed，实际 %s", got2.Status)
	}
	if got2.AttemptsCount != 5 {
		t.Errorf("attempts_count 应为 5，实际 %d", got2.AttemptsCount)
	}
}

// TestRecoverExpiredLeases：租约过期的 processing 事件应被回收成 pending，且不写 attempt。
func TestRecoverExpiredLeases(t *testing.T) {
	s := openTest(t)
	_, evID := seedPending(t, s)
	ctx := context.Background()
	claimNow := time.Now().Unix()

	if _, err := s.ClaimEvent(ctx, evID, claimNow, 60); err != nil {
		t.Fatal(err)
	}

	// 用一个"未来"的 now 调用，模拟租约已过期（locked_until = claimNow+60 < claimNow+61）
	n, err := s.RecoverExpiredLeases(ctx, claimNow+61)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应回收 1 条，实际 %d", n)
	}

	got := mustGet(t, s, evID)
	if got.Status != store.StatusPending {
		t.Errorf("回收后应为 pending，实际 %s", got.Status)
	}
	if got.LockedUntil != nil {
		t.Error("回收后 locked_until 应为 nil")
	}
}
