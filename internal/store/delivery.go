// 投递链路的数据库操作（W4 实现）。
//
// 分工约定（docs/项目书评审与执行计划.md 第七节）：
//   - DueEventIDs / NextAttemptNo 是"脚手架"，SQL 在 docs/02-flow.md 里逐字写过，直接给出。
//   - ClaimEvent / SettleDelivery / RecoverExpiredLeases 是三处核心逻辑：
//     原子抢占、状态机结算、租约回收。所有"改 events.status 的 SQL 都带状态前置条件"
//     （docs/02-flow.md 3.5 节红线），这是并发安全的根本保证。
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// retryBackoff[newCount] = 失败后"下一次投递"要等待的秒数。
// 下标是结算后新的 attempts_count（见 docs/02-flow.md 4.2 节退避表）：
//
//	1 → 5s, 2 → 30s, 3 → 2min, 4 → 10min
//
// 下标 5 对应"第 5 次失败"，此时已转 failed，不会用到退避。
var retryBackoff = []int64{
	0,    // 下标 0 不用
	5,    // attempts_count 新值 = 1
	30,   // = 2
	120,  // = 3（2 分钟）
	600,  // = 4（10 分钟）
	3600, // = 5（1 小时，转 failed 时不用）
}

// DeliveryOutcome 是一次投递尝试的结果，字段与 attempts 表的列一一对应，
// 所以定义在 store 包（描述的是账本里一行的形状）。worker 发完 HTTP 把它原样带回来。
type DeliveryOutcome struct {
	Success        bool   // 2xx 且未超时
	ResponseStatus int    // HTTP 状态码；网络错误 / 超时为 0（根本没拿到回应）
	Err            string // 失败原因原文；成功为空串
	DurationMS     int64  // 本次往返耗时（毫秒）
}

// DueEventIDs 捞出到期任务的候选 ID（docs/02-flow.md 2.2 第一步，只读）。
//
// 注意这里【只查 ID 不改状态】——抢占是下一步 ClaimEvent 的独占职责。
// 按 next_attempt_at 升序：最早该投的最先投，天然公平。
// limit 由调用方传，V0.1 的 worker 每轮只捞 1 条。
func (s *Store) DueEventIDs(ctx context.Context, now int64, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM events
		  WHERE status IN (?, ?) AND next_attempt_at <= ?
		  ORDER BY next_attempt_at
		  LIMIT ?`,
		StatusPending, StatusRetrying, now, limit)
	if err != nil {
		return nil, fmt.Errorf("捞取到期事件失败: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读取到期事件 ID 失败: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// NextAttemptNo 计算一条事件的下一次物理投递编号：MAX(attempt_no)+1，没有历史则为 1。
//
// 这个编号一次计算两处用：投递时的 X-Webhook-Attempt 头、结算时 attempts.attempt_no。
// 它只在事件处于 processing 期间被调用（同一事件同一时刻只有一个 worker 在管），
// 所以不存在并发竞争，不需要加锁。
func (s *Store) NextAttemptNo(ctx context.Context, eventID string) (int, error) {
	var maxNo int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(attempt_no), 0) FROM attempts WHERE event_id = ?`, eventID).
		Scan(&maxNo)
	if err != nil {
		return 0, fmt.Errorf("查询事件 %s 的投递编号失败: %w", eventID, err)
	}
	return maxNo + 1, nil
}

// ClaimEvent 原子抢占一条事件——状态机第 1/2 条边（pending|retrying → processing）。
//
// 这是三处核心逻辑之一（核心一：原子抢占）。
// 验收标准（每一条都会被 W14 的测试检查）：
//  1. 必须是【一条 UPDATE】完成抢占，禁止先 SELECT 确认状态再 UPDATE。
//     两步之间别人可能插进来，这种 check-then-act 竞态 SELECT 拦不住（docs 3.5 防线 1）。
//     UPDATE 要做的赋值：status = 'processing'、locked_until = now + leaseSeconds、
//     updated_at = now（不变量 3：每次转移必更新 updated_at）。
//  2. WHERE 必须带状态前置条件：id = ? AND status IN ('pending','retrying')。
//     这条守卫天然排除 delivered / failed，也排除正被别的 worker 占用的行。
//  3. 用 RowsAffected 判断成败（防线 2）：等于 1 才算抢到；
//     等于 0 说明被别人抢先或状态已变——这是正常竞争，返回 (false, nil)，不是错误。
func (s *Store) ClaimEvent(ctx context.Context, id string, now int64, leaseSeconds int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE events
		    SET status = 'processing',
		        locked_until = ?,
		        updated_at = ?
		  WHERE id = ?
		    AND status IN ('pending','retrying')`,
		now+leaseSeconds, now, id)
	if err != nil {
		return false, fmt.Errorf("抢占事件 %s 失败: %w", id, err)
	}
	n, _ := res.RowsAffected()
	// 唯一两种 outcome：(true, nil) 抢到了 / (false, nil) 没抢到；error 只留给真故障
	return n == 1, nil
}

// SettleDelivery 投递结算——状态机第 3/4/5 条边（processing → delivered / retrying / failed）。
//
// 这是三处核心逻辑之二（核心二：状态机结算）。
//
// 调用时机：worker 已经拿到 HTTP 结果 out；ev 是投递【前】读出的事件快照
// （ev.AttemptsCount 是结算前的预算计数，分支判断要用它）。
//
// 验收标准：
//  1. 必须显式开事务（BeginTx + defer Rollback + Commit）。
//     "写 attempts 审计行"和"更新 events 状态"必须同生共死：
//     只写 attempt 不改状态 → 事件永远卡在 processing；只改状态不写 attempt → 审计缺行。
//     （docs/04-data-model.md 第五节：事务边界就两处，这是第二处。）
//  2. 事务里先 INSERT 一行 attempts：
//     event_id = ev.ID、attempt_no = attemptNo（调用方已按 MAX+1 算好）、
//     status 按 out.Success 取 'success'/'failed'、response_status = out.ResponseStatus
//     （网络错误时是 0，落库应为 NULL）、error 列成功时写 NULL / 失败时写 out.Err、
//     duration_ms = out.DurationMS、started_at / finished_at 用 now 和耗时反推。
//  3. 再 UPDATE events，WHERE 必须带 status = 'processing' 守卫（防线 1），
//     且 RowsAffected != 1 时回滚整个事务并返回 nil——说明事件已被回收或被别人结算，
//     它的命运已有人接管，我们的审计行不能单独留下。
//  4. 按结局三分支（docs/02-flow.md 3.2 第 3/4/5 条边）：
//     - out.Success：status='delivered'，delivered_at=now，attempts_count+1
//     - 失败且 newCount < 5（重试预算没耗尽）：status='retrying'，
//     next_attempt_at = now + 退避[newCount]，attempts_count+1，last_error=out.Err
//     - 失败且预算耗尽：status='failed'，attempts_count+1，last_error=out.Err
//  5. 所有分支都要更新 updated_at（不变量 3）。
func (s *Store) SettleDelivery(ctx context.Context, ev Event, attemptNo int, out DeliveryOutcome, now int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback() // 提交成功后这里变 no-op

	// ---- 第一步：写审计记录（只追加，先落 audit 再结算状态）----
	status := "failed"
	if out.Success {
		status = "success"
	}
	// response_status：0 表示根本没拿到回应（网络错误/超时），落库应为 NULL
	var respStatus sql.NullInt64
	if out.ResponseStatus != 0 {
		respStatus = sql.NullInt64{Valid: true, Int64: int64(out.ResponseStatus)}
	}
	startedAt := now - out.DurationMS/1000 // 用 now 和耗时反推起点
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attempts
		   (event_id, attempt_no, status, response_status, error, duration_ms, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, attemptNo, status, respStatus, nullStr(out.Err),
		out.DurationMS, startedAt, now); err != nil {
		return fmt.Errorf("写入 attempt 失败: %w", err)
	}

	// ---- 第二步：结算事件状态（必须带 status='processing' 守卫）----
	newCount := ev.AttemptsCount + 1
	var newStatus string
	nextAttemptAt := now // delivered / failed 时 next_attempt_at 无意义，给个 now 兜底
	deliveredAt := sql.NullInt64{}
	lastError := nullStr(out.Err)

	switch {
	case out.Success:
		// 第 3 条边：2xx → delivered
		newStatus = StatusDelivered
		d := now
		deliveredAt = sql.NullInt64{Valid: true, Int64: d}
	case newCount < 5:
		// 第 4 条边：失败但预算没耗尽 → retrying，按退避表推后
		newStatus = StatusRetrying
		nextAttemptAt = now + retryBackoff[newCount]
	default:
		// 第 5 条边：第 5 次仍失败 → failed（终态）
		newStatus = StatusFailed
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE events
		    SET status = ?,
		        attempts_count = ?,
		        delivered_at = ?,
		        next_attempt_at = ?,
		        last_error = ?,
		        locked_until = NULL,
		        updated_at = ?
		  WHERE id = ?
		    AND status = 'processing'`,
		newStatus, newCount, deliveredAt, nextAttemptAt, lastError, now, ev.ID)
	if err != nil {
		return fmt.Errorf("结算事件状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// 事件已被回收或已被别人结算——它的命运已有人接管，
		// 按验收标准回滚整个事务并返回 nil（不单独留下这条 audit）。
		_ = tx.Rollback()
		return nil
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// RecoverExpiredLeases 租约超时回收——状态机第 6 条边（processing → pending）。
//
// 这是三处核心逻辑之三（核心三：崩溃回收）。
//
// 场景：worker 在投递途中崩溃（或被 kill），事件卡在 processing，
// locked_until 是它承诺的"最多占用到几点"。过期了就说明占着位置的人已经不在了。
//
// 验收标准：
//  1. 一条 UPDATE 搞定：status 改回 'pending'，locked_until 置 NULL，
//     next_attempt_at = now，updated_at = now；
//     WHERE status = 'processing' AND locked_until < now。
//     守卫保证"正在正常投递的事件"绝不会被误回收。
//  2. 【不写 attempt、不加 attempts_count】——崩溃点落在"目标到底收到没有"
//     不可判定的地带，按未完成处理，重投后由目标方用 X-Webhook-Event-Id 幂等去重
//     （docs/02-flow.md 4.3，这就是"至少一次"语义的代价）。
//  3. 返回本次回收的条数（RowsAffected），worker 会按条数打日志。
func (s *Store) RecoverExpiredLeases(ctx context.Context, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE events
		    SET status = 'pending',
		        locked_until = NULL,
		        next_attempt_at = ?,
		        updated_at = ?
		  WHERE status = 'processing'
		    AND locked_until IS NOT NULL
		    AND locked_until < ?`,
		now, now, now)
	if err != nil {
		return 0, fmt.Errorf("回收租约失败: %w", err)
	}
	return res.RowsAffected()
}

// nullStr 把 Go 空串转成 SQL NULL（last_error 为空时不应写成空串）。
func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{Valid: true, String: s}
}
