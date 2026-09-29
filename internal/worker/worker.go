// Package worker 是后台投递进程的心脏：循环把库里的到期事件捞出来投掉。
//
// 分工（docs/项目书评审与执行计划.md 第七节）：
//   - Notifier / Run 是脚手架：唤醒信号、ticker、退出——写法固定，直接给。
//   - drain 里的"取任务 → 抢占 → 投递 → 结算"序列是学习靶心，TODO(本人写)。
//
// 两条铁律（docs/02-flow.md 第五节的取舍）：
//  1. 数据库是唯一真相来源：worker 不维护任何内存队列，
//     每一轮都从库里现捞——所以重启不丢任务、不丢退避进度。
//  2. channel 只做唤醒不做搬运：信号丢了没关系，ticker 兜底。
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/xhj015/webhook-gateway/internal/delivery"
	"github.com/xhj015/webhook-gateway/internal/store"
)

const (
	tickInterval = 1 * time.Second // 兜底扫描周期：唤醒信号全丢了，最多延迟 1 秒也能捞到
	leaseSeconds = 60              // 投递租约：10 秒投递超时的 6 倍，余量足够（docs 4.3）
	batchLimit   = 1               // 每轮捞几条；单 worker 时 1 条即可
)

// Notifier 是唤醒信号的信箱。
//
// channel 里装的不是事件，是空结构体——数据全在库里，
// 信号只是喊一嗓子"该干活了"。容量 1 + 非阻塞发送：
// 信箱满了说明 worker 马上就会醒，这一嗓子丢了也无所谓（信号可丢，数据不可丢）。
type Notifier struct {
	ch chan struct{}
}

// NewNotifier 建一个信箱。容量写死 1：多个信号叠在一起没有意义，醒来一次全扫。
func NewNotifier() *Notifier {
	return &Notifier{ch: make(chan struct{}, 1)}
}

// Wake 非阻塞发一个信号。
// 永远不要在持锁或事务里调用它——它不该有阻塞别人的能力。
func (n *Notifier) Wake() {
	select {
	case n.ch <- struct{}{}:
	default: // 满了就丢
	}
}

// C 暴露只读信号通道给 Worker。接收方拿到的类型是 <-chan，只能收不能发。
func (n *Notifier) C() <-chan struct{} { return n.ch }

// Worker 是投递执行者。一个进程可以起 N 个（V0.1 默认 1），
// 多个 worker 之间靠 ClaimEvent 的原子抢占保证同一事件不被重复投。
type Worker struct {
	st     *store.Store
	client *delivery.Client
	log    *slog.Logger
	wake   <-chan struct{}
}

// New 组装 Worker。wake 接的是 Notifier.C()，把"谁喊的"和"干活的人"解耦。
func New(st *store.Store, client *delivery.Client, log *slog.Logger, wake <-chan struct{}) *Worker {
	return &Worker{st: st, client: client, log: log, wake: wake}
}

// Run 是 worker 的主循环：一个死循环 select 等三种唤醒源，醒来就干一轮。
//
//	ctx 取消 → 退出（优雅退出 M8 才接系统信号，现阶段传 context.Background()）
//	ticker 到点 → 干一轮（兜底：信号丢失 / retrying 到期都靠它扫出来）
//	wake 收到信号 → 干一轮（新事件落库后的"加塞"，延迟最低）
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	// 启动先扫一次：上次进程崩溃时留在库里的任务，重启后要能立刻被捞起（docs 4.3）
	w.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker 已退出")
			return
		case <-ticker.C:
			w.tick(ctx)
		case <-w.wake:
			w.tick(ctx)
		}
	}
}

// tick 一轮工作：先回收过期租约，再把到期任务全部处理完才返回。
// 处理完才返回（而不是投一条就回去等信号）的原因：
// 连续来 100 条事件时，"清空库存再休息"比"每条都等 1 秒 ticker"快得多。
func (w *Worker) tick(ctx context.Context) {
	recovered, err := w.st.RecoverExpiredLeases(ctx, time.Now().Unix())
	if err != nil {
		w.log.Error("租约回收失败", "err", err)
	} else if recovered > 0 {
		w.log.Warn("回收了超时租约（上一次投递没能结算）", "count", recovered)
	}

	w.drain(ctx)
}

// drain 把当前所有到期任务处理完才返回。
//
// 流程严格对应 docs/02-flow.md 2.2 的表格（①②③④⑤⑥⑦）：
//  ① 捞候选 → ② 原子抢占 → ③ 读全量字段 → ④ 算投递编号
//  → ⑤ 真正投递 → ⑥ 事务结算 → ⑦ 记日志。
//
// ③ 必须排在 ② 成功之后：先抢占、再读全量，否则两个 worker 可能读到同一份旧数据。
// 任意一步的数据库错误都 return 退出本轮（drain 由 tick 反复调用，下一轮 ticker 会再来）；
// 已抢占但未结算的事件会停在 processing，租约过期后被自动回收重投，不会丢。
func (w *Worker) drain(ctx context.Context) {
	for {
		now := time.Now().Unix()

		// ① 捞候选：到期且可调度的事件 ID（只读，不改状态）
		ids, err := w.st.DueEventIDs(ctx, now, batchLimit)
		if err != nil {
			w.log.Error("捞到期事件失败", "err", err)
			return
		}
		if len(ids) == 0 {
			return // 库里没有到期任务了，回去休息
		}

		// ② 原子抢占：被别人抢走是正常竞争，看下一条
		claimed, err := w.st.ClaimEvent(ctx, ids[0], now, leaseSeconds)
		if err != nil {
			w.log.Error("抢占事件失败", "event_id", ids[0], "err", err)
			return
		}
		if !claimed {
			continue
		}

		// ③ 抢到之后才读全量字段（含 payload、headers、attempts_count）
		ev, err := w.st.GetEvent(ctx, ids[0])
		if err != nil {
			w.log.Error("读事件失败", "event_id", ids[0], "err", err)
			return
		}
		ep, err := w.st.GetEndpoint(ctx, ev.EndpointID)
		if err != nil {
			w.log.Error("读端点失败", "event_id", ev.ID, "endpoint_id", ev.EndpointID, "err", err)
			return
		}

		// ④ 本次物理投递编号（历史最大 + 1，永不清零）
		attemptNo, err := w.st.NextAttemptNo(ctx, ev.ID)
		if err != nil {
			w.log.Error("计算 attempt_no 失败", "event_id", ev.ID, "err", err)
			return
		}

		// ⑤ 真正投递：delivery.Client 只发请求、如实回报，不负责判定分支
		out := w.client.Deliver(ctx, ep.URL, ev.Payload,
			delivery.ParseForwardHeaders(ev.Headers), ev.ID, attemptNo)

		// ⑥ 结算：事务内写 attempt + 更新事件状态
		if err := w.st.SettleDelivery(ctx, ev, attemptNo, out, time.Now().Unix()); err != nil {
			w.log.Error("结算投递结果失败", "event_id", ev.ID, "err", err)
			return
		}

		// ⑦ 打一条结构化日志，方便按 event_id 串起整条生命周期
		w.log.Info("事件结算完成",
			"event_id", ev.ID,
			"attempt_no", attemptNo,
			"success", out.Success,
			"response_status", out.ResponseStatus,
			"duration_ms", out.DurationMS,
		)
	}
}
