package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/xhj015/webhook-gateway/internal/idgen"
	"github.com/xhj015/webhook-gateway/internal/store"
)

// seedFailed 直接落一条 failed 事件（attempts_count 用满 5），用于测 RetryEvent 的入口状态。
func seedFailed(t *testing.T, s *store.Store) (epID, evID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	ep := store.Endpoint{ID: idgen.Endpoint(), Name: "t", URL: "http://localhost:9/hook", Enabled: true, CreatedAt: now}
	if err := s.CreateEndpoint(ctx, ep); err != nil {
		t.Fatal(err)
	}
	ev := store.Event{ID: idgen.Event(), EndpointID: ep.ID, Status: store.StatusFailed,
		Payload: []byte("x"), Headers: "{}", AttemptsCount: 5, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	return ep.ID, ev.ID
}

// driveToFailed 让一条 pending 事件连续失败 5 次，走到 failed 终态，期间产生 5 条 attempt。
// 这是"真实"地构造 failed 状态，而不是直接 INSERT 一行，能顺带验证结算循环。
func driveToFailed(t *testing.T, s *store.Store, evID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	out := store.DeliveryOutcome{Success: false, ResponseStatus: 500, Err: "boom", DurationMS: 1}
	for i := 0; i < 5; i++ {
		if _, err := s.ClaimEvent(ctx, evID, now, 60); err != nil {
			t.Fatal(err)
		}
		ev := mustGet(t, s, evID)
		an, err := s.NextAttemptNo(ctx, evID)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SettleDelivery(ctx, ev, an, out, now); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustGet(t, s, evID); got.Status != store.StatusFailed {
		t.Fatalf("driveToFailed 后应为 failed，实际 %s", got.Status)
	}
}

func TestDisableEndpoint(t *testing.T) {
	s := openTest(t)
	epID, _ := seedPending(t, s)
	ctx := context.Background()
	now := time.Now().Unix()

	// 首次停用：应返回 (true, nil)
	changed, err := s.DisableEndpoint(ctx, epID, now)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("首次停用应返回 true")
	}
	ep, err := s.GetEndpoint(ctx, epID)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Enabled {
		t.Error("停用后 Enabled 应为 false")
	}
	if ep.DisabledAt == nil || *ep.DisabledAt != now {
		t.Errorf("停用后 DisabledAt 应为 %d，实际 %v", now, ep.DisabledAt)
	}

	// 重复停用：应返回 (false, nil)，且状态不变
	again, err := s.DisableEndpoint(ctx, epID, now+1)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("重复停用应返回 false")
	}

	// 停用不存在的端点：返回 (false, nil)，不报错
	miss, err := s.DisableEndpoint(ctx, "ep_不存在的ID", now)
	if err != nil {
		t.Fatal(err)
	}
	if miss {
		t.Error("停用不存在的端点应返回 false")
	}
}

func TestListEndpoints(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().Unix()

	// 造 3 个端点，故意乱序插入
	for _, name := range []string{"a", "b", "c"} {
		ep := store.Endpoint{ID: idgen.Endpoint(), Name: name, URL: "http://localhost:9/" + name, Enabled: true, CreatedAt: now}
		if err := s.CreateEndpoint(ctx, ep); err != nil {
			t.Fatal(err)
		}
	}

	eps, err := s.ListEndpoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 3 {
		t.Fatalf("应列出 3 个端点，实际 %d", len(eps))
	}
	for i := 1; i < len(eps); i++ {
		if eps[i-1].CreatedAt > eps[i].CreatedAt {
			t.Errorf("列表应按 created_at 升序，第 %d 个晚于第 %d 个", i-1, i)
		}
	}
}

func TestListEvents(t *testing.T) {
	s := openTest(t)
	_, evPending := seedPending(t, s)
	_, evFailed := seedFailed(t, s)
	ctx := context.Background()

	// 不过滤：应返回 2 条
	all, err := s.ListEvents(ctx, "", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("不过滤应返回 2 条，实际 %d", len(all))
	}

	// 按状态过滤
	failed, err := s.ListEvents(ctx, store.StatusFailed, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].ID != evFailed {
		t.Errorf("过滤 failed 应只返回 %s，实际 %v", evFailed, failed)
	}

	// 分页：limit 1 只返回 1 条
	page, err := s.ListEvents(ctx, "", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 {
		t.Errorf("limit=1 应返回 1 条，实际 %d", len(page))
	}

	// 列表按 created_at 倒序（新的在前）。两条 created_at 相同则顺序任意，不强行断言顺序。
	_ = evPending
}

func TestListAttempts(t *testing.T) {
	s := openTest(t)
	_, evID := seedPending(t, s)
	ctx := context.Background()
	now := time.Now().Unix()

	// 成功投递一次 → 1 条 attempt
	if _, err := s.ClaimEvent(ctx, evID, now, 60); err != nil {
		t.Fatal(err)
	}
	an, _ := s.NextAttemptNo(ctx, evID)
	out := store.DeliveryOutcome{Success: true, ResponseStatus: 200, DurationMS: 10}
	if err := s.SettleDelivery(ctx, mustGet(t, s, evID), an, out, now); err != nil {
		t.Fatal(err)
	}

	atts, err := s.ListAttempts(ctx, evID)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 {
		t.Fatalf("应有 1 条 attempt，实际 %d", len(atts))
	}
	if atts[0].AttemptNo != 1 || atts[0].Status != "success" {
		t.Errorf("attempt 内容不对: %+v", atts[0])
	}
	if atts[0].ResponseStatus == nil || *atts[0].ResponseStatus != 200 {
		t.Errorf("response_status 应为 200，实际 %v", atts[0].ResponseStatus)
	}

	// 不存在的 event 返回空列表（不报错）
	empty, err := s.ListAttempts(ctx, "ev_不存在")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("不存在的事件应返回空列表，实际 %d", len(empty))
	}
}

func TestRetryEvent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// 场景一：failed → 重试成功 → pending，且 attempts_count 清零
	_, evID := seedFailed(t, s)
	changed, err := s.RetryEvent(ctx, evID, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("failed 事件重试应返回 true")
	}
	got := mustGet(t, s, evID)
	if got.Status != store.StatusPending {
		t.Errorf("重试后应为 pending，实际 %s", got.Status)
	}
	if got.AttemptsCount != 0 {
		t.Errorf("重试后 attempts_count 应清零，实际 %d", got.AttemptsCount)
	}

	// 场景二：非 failed（delivered）→ 重试失败（返回 false），状态不变
	_, evDelivered := seedPending(t, s)
	now := time.Now().Unix()
	if _, err := s.ClaimEvent(ctx, evDelivered, now, 60); err != nil {
		t.Fatal(err)
	}
	an, _ := s.NextAttemptNo(ctx, evDelivered)
	if err := s.SettleDelivery(ctx, mustGet(t, s, evDelivered), an,
		store.DeliveryOutcome{Success: true, ResponseStatus: 200, DurationMS: 1}, now); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, evDelivered); got.Status != store.StatusDelivered {
		t.Fatalf("前置：应为 delivered，实际 %s", got.Status)
	}
	again, err := s.RetryEvent(ctx, evDelivered, now)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("delivered 事件重试应返回 false")
	}
	if got := mustGet(t, s, evDelivered); got.Status != store.StatusDelivered {
		t.Errorf("delivered 事件状态不应被改动，实际 %s", got.Status)
	}

	// 场景三：不存在 → 返回 false
	miss, err := s.RetryEvent(ctx, "ev_不存在", now)
	if err != nil {
		t.Fatal(err)
	}
	if miss {
		t.Error("不存在的事件重试应返回 false")
	}
}

// TestRetryKeepsHistory：手动重试不删历史 Attempt——B13 的核心验收。
// 走真实结算循环让事件失败 5 次（产生 5 条 attempt），再重试，历史应完整保留，且下一次物理投递编号是 6。
func TestRetryKeepsHistory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_, evID := seedPending(t, s)

	driveToFailed(t, s, evID)

	atts, err := s.ListAttempts(ctx, evID)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 5 {
		t.Fatalf("失败 5 次后应有 5 条 attempt，实际 %d", len(atts))
	}

	// 重试
	if changed, err := s.RetryEvent(ctx, evID, time.Now().Unix()); err != nil || !changed {
		t.Fatalf("重试应成功，changed=%v err=%v", changed, err)
	}

	// 历史还在
	after, err := s.ListAttempts(ctx, evID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 5 {
		t.Errorf("重试后历史 attempt 不应被删，实际 %d", len(after))
	}

	// 下一次物理投递编号是 6（attempt_no 永不清零）
	next, err := s.NextAttemptNo(ctx, evID)
	if err != nil {
		t.Fatal(err)
	}
	if next != 6 {
		t.Errorf("重试后 attempt_no 应继续编号到 6，实际 %d", next)
	}
}
