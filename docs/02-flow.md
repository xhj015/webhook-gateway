# Webhook Gateway —— 完整业务流程与状态机（V0.1）

> 文档编号：docs/02-flow.md
> 定位：回答"一条事件的一生怎么走"。本文档是 M0 的核心交付物。
> 业务规则见 `docs/01-business.md`，字段与索引见 `docs/04-data-model.md`。

---

## 一、一段话版（背下来，随时能讲）

> 一条事件从 `POST /hooks/{id}` 进来，网关先在一个事务里把它写成一行 `pending` 记录、提交之后才返回 202；写入信号只用来唤醒 worker，**事件本身永远只存在数据库里**。worker 从库里捞到期的事件、用一条 `UPDATE ... WHERE status IN ('pending','retrying')` 原子抢占成 `processing`，然后 POST 到目标地址，把这一次的结果写成一条 `attempt`。成功就转 `delivered`；失败就按 `5s → 30s → 2min → 10min → 1h` 推后 `next_attempt_at` 转 `retrying` 等着再捞；5 次用尽转终态 `failed`。进程崩了，`processing` 会在 60 秒租约到期后被回收重投；`failed` 可以人工一键重置回 `pending`，历史记录一条不删。**全程事件只有一处真相——数据库。**

### 1.1 先分清：一次 HTTP 请求 ≠ 一条事件

这两个东西容易混，但**生命周期长度完全不同**，混了后面全乱：

| | 一次 HTTP 请求 | 一条事件 |
|---|---|---|
| 开始 | 客户端把请求发到 socket | `POST /hooks/{id}` 到达网关 |
| 结束 | 响应写回客户端（毫秒 ~ 10 秒） | 状态变成 `delivered` 或 `failed`（几秒 ~ 1 小时 12 分） |
| 有记忆吗 | **没有**。响应一发完，双方都不记得发生过什么 | **有**。全程写在 `events` / `attempts` 里 |
| 谁负责 | 没人。失败了就是失败了 | 网关负责到底 |
| 失败之后 | HTTP 里没有"失败之后"这个概念 | 退避重试最多 5 次，留下 5 条 attempt |
| 存在形态 | 内存里几个变量，函数一返回就消失 | 数据库里的行，进程重启也还在 |

**一条事件的一生里，会发生多次互相不认识的 HTTP 请求：**

1. `POST /hooks/{id}`（调用方 → 网关）—— 事件出生
2. `POST {endpoint.url}`（网关 → 目标）—— 第 1 次投递
3. 同上，最多再 4 次，分布在 5s / 30s / 2min / 10min / 1h 之后
4. （可选）`GET /api/events/{id}` —— 回来查结果
5. （可选）`POST /api/events/{id}/retry` —— 人工救一次

**关键在于第 2 步和第 3 步之间的空白**：那几分钟到一小时里没有任何 HTTP 请求在跑，进程甚至可能重启过，但这条事件**仍然活着**——因为它活在数据库里。**这段"没有请求的时间里发生了什么"，才是本项目真正做的东西。**

HTTP 本身是"尽力而为"的一次性动作：不记得上次发过什么、不会自己重试、也无法确认对方真的收到了。**"事件"这个概念是我们用数据库里的状态机加上去的**——把几次互不认识的 HTTP 请求，黏成一条有始有终的线。这就是项目书那句话的技术含义：把"不可靠的一次 HTTP 请求"，变成"有记录、可追踪、可重试的一次事件投递"。

> 阅读约定：本文档描述的**始终是"事件"的流程**。文中出现的 HTTP 请求是这条流程上的节点，不是流程本身。

---

## 二、一条事件的一生（时间线）

设定：目标是订单系统的 `http://localhost:9000/hooks/pay`，端点 ID `ep_7f3a9c`。

### 2.1 出生（接收）

| 时刻 | 发生了什么 | 库里发生了什么 |
|---|---|---|
| T0 | 创建端点：`POST /api/endpoints` → 拿到 `ep_7f3a9c`，接收地址 `POST /hooks/ep_7f3a9c` | `endpoints` 加一行 |
| T0+1s | 支付平台 POST 来一条事件 | — |
| T0+1.01s | 网关依次校验：端点存在？启用？body ≤ 1MB？ | 只读，不改库 |
| T0+1.02s | **一个事务**：写一行事件（`status=pending`、`next_attempt_at=now`、`attempts_count=0`） | `events` 加一行 `ev_2b81d0` |
| T0+1.03s | 事务提交 → 向唤醒 channel 非阻塞发一个信号 → 返回 `202 {"event_id":"ev_2b81d0"}` | — |

**关键点：202 只代表"我记住了"，不代表"我送到了"。** 调用方拿到 `event_id` 就能随时回来查结果。

### 2.2 投递（worker 的工作循环）

| 时刻 | 发生了什么 | 库里发生了什么 |
|---|---|---|
| T0+1.05s | worker 被唤醒，捞到期任务：`WHERE status IN ('pending','retrying') AND next_attempt_at <= now ORDER BY next_attempt_at LIMIT 1` | 只读 |
| T0+1.06s | **原子抢占**：`UPDATE events SET status='processing', locked_until=now+60s WHERE id=? AND status IN ('pending','retrying')`，`RowsAffected()==1` 才算抢到 | `status → processing` |
| T0+1.07s | 用原始 payload + 透传头（含 `X-Webhook-Event-Id`）POST 到目标，10 秒超时 | — |
| T0+1.25s | 拿到回应，**一个事务**：写 Attempt + 结算事件状态 | `attempts` 加一行，`events` 状态定稿 |

### 2.3 四种结局

| 结局 | 触发条件 | 库里最终长什么样 |
|---|---|---|
| **A 送达** | 目标返回 2xx | `events.status=delivered`；1 条 `attempts(status=success, response_status=200, duration_ms=180)` |
| **B 重试** | 非 2xx / 超时 / 网络错误，且本次数 < 5 | `events.status=retrying`，`next_attempt_at=now+退避`；1 条 `attempts(status=failed, error=...)`。到点后回到 2.2，共循环 5 次 |
| **C 耗尽** | 第 5 次仍失败 | `events.status=failed`，`last_error` 保留最后一次原因；共 5 条 attempt。**事件和记录都还在，不是消失，是进了死信区** |
| **D 崩溃** | 投递进行中进程被 kill | `events` 卡在 `processing`，`locked_until` 指向 T+60s。重启后扫到租约过期 → 回收重投（见第四节） |

### 2.4 人工介入

`failed` 的事件不会自己复活，但随时可以人工救：

```
POST /api/events/ev_2b81d0/retry
  → 重置 attempts_count=0、next_attempt_at=now、status=pending
  → worker 立刻捞起来投
```

第 6 次物理投递会记成 `attempt_no=6`——**历史一条都没删**。谁能看一眼 attempts 列表就知道"人工救过几次、每次为什么失败"。

---

## 三、状态机

### 3.1 五个状态

| 状态 | 含义 | 是终态吗 | 谁在等 |
|---|---|---|---|
| `pending` | 已落库，等待被 worker 抢占（包含首次投递和崩溃回收后的重投） | 否 | worker |
| `processing` | 已被某个 worker 抢占，正在投递中（有 60 秒租约） | 否 | 投递结果 |
| `retrying` | 投递失败但预算没用完，在等退避到期 | 否 | 时间 |
| `delivered` | 已确认送达（2xx） | **是** | — |
| `failed` | 重试预算耗尽，需人工介入 | **是** | 人 |

### 3.2 七条转移（状态机全部内容）

| # | 从 → 到 | 触发条件 | 副作用 |
|---|---|---|---|
| 1 | `pending` → `processing` | worker 抢占（首次投递） | `locked_until = now+60s` |
| 2 | `retrying` → `processing` | `next_attempt_at` 到期，worker 抢占 | `locked_until = now+60s` |
| 3 | `processing` → `delivered` | 目标 2xx | 写 attempt(success)、`delivered_at=now`、`attempts_count+1` |
| 4 | `processing` → `retrying` | 投递失败且 `attempts_count+1 < 5` | 写 attempt(failed)、`next_attempt_at = now+退避[次数]`、`attempts_count+1` |
| 5 | `processing` → `failed` | 投递失败且 `attempts_count+1 >= 5` | 写 attempt(failed)、`last_error=...`、`attempts_count+1` |
| 6 | `processing` → `pending` | 租约过期（`locked_until < now`），启动或每秒 tick 时扫到 | `locked_until=NULL`、`next_attempt_at=now`，不写 attempt、不加计数 |
| 7 | `failed` → `pending` | 人工手动重试 | `attempts_count=0`、`next_attempt_at=now`，**不删任何历史** |

### 3.3 状态图

```
                    ①worker 抢占                ③2xx 成功
        ┌──────────┐ ─────────────► ┌────────────┐ ──────────► ┌───────────┐
        │ pending  │                │ processing │             │ delivered │
        └──────────┘ ◄───────────── └────────────┘             └───────────┘
              ▲       ⑥租约超时回收        │                        （终态）
              │                           │ ④失败且未耗尽
              │                           ▼
              │                     ┌───────────┐
              │      ②退避到期       │ retrying  │
              │   抢占（回到 processing）└───────────┘
              │                           │ ⑤5 次耗尽
              │                           ▼
              │     ⑦手动重试       ┌───────────┐
              └──────────────────── │  failed   │
                                    └───────────┘ （终态，等人工）
```

### 3.4 三条不变量（写代码和写测试时都要盯住）

1. **任何时刻，一条事件只处于一个状态**，且只能沿上表七条边走——出现第八条边就是 bug。
2. **`delivered` 和 `failed` 是吸收态**：除了 `failed` 能被人工拉回 `pending`，状态机里没有任何一条边从终态出发。
3. **一次转移必然伴随一次 `updated_at` 更新**，方便出问题时按时间线还原。

---

## 四、三条关键流程（实现时逐句对照）

### 4.1 接收流程：为什么"先提交再回 202"

```
HTTP 请求
  ├─ 校验端点（存在 / 启用 / body 大小）  ── 失败就 404 / 410 / 413，不进库
  ├─ BEGIN → INSERT events(pending) → COMMIT     ← 数据落地，唯一真相
  ├─ 向 channel 发信号（chan struct{}, 容量 1，满了就丢，非阻塞）
  └─ 返回 202 + event_id                          ← 只承诺"我记住了"
```

**channel 里装的不是事件，只是一个 `struct{}{}`。** 因为数据已经在库里了，信号丢了（channel 满）也没关系——worker 每次 tick 都会去库里扫。这个选择直接让 M3（队列语义）和 M6（崩溃恢复）变成同一个机制。

### 4.2 投递与退避：`next_attempt_at` 是唯一的调度器

worker 不维护任何内存计时器，**"什么时候该重试"完全由库里一个字段表达**：

| 第几次失败 | `attempts_count` 变成 | 写入的 `next_attempt_at` | 实际等待 |
|---|---|---|---|
| 1 | 1 | now + 5s | 5 秒 |
| 2 | 2 | now + 30s | 30 秒 |
| 3 | 3 | now + 2min | 2 分 |
| 4 | 4 | now + 10min | 10 分 |
| 5 | 5 | —（转 `failed`） | 总跨度约 1 小时 12 分 |

好处：**重启不丢退避进度**。内存方案里重启后所有计时器归零，库方案里 `next_attempt_at` 就在那儿，重启后照旧。

### 4.3 崩溃恢复：租约 + 回收

| 场景 | 现象 | 恢复动作 |
|---|---|---|
| 事件在 `pending` 时崩溃 | 重启后没人碰它 | 启动即触发一次扫描，捞 `pending` 且到期的投 |
| 事件在 `processing` 时崩溃 | 卡住，谁也不知道投没投出去 | 扫 `processing AND locked_until < now` → 按第 6 条边回收成 `pending` |
| 事件在 `retrying` 时崩溃 | `next_attempt_at` 还是个未来时间 | 到点自然被捞，无需特殊处理 |

**为什么回收时不写 attempt、不加 `attempts_count`？** 因为崩溃点落在"目标已收到"和"目标没收到"之间，**事实不可知**。既然不可知，就按"没完成"处理并重投，重复由接收方用 `X-Webhook-Event-Id` 幂等去重——这正是"至少一次"的代价，也是它的实现方式。

**为什么租约要 60 秒？** 投递超时是 10 秒，租约必须显著大于它。否则 worker 还在投，另一个 worker（或重启后的自己）就把任务抢走重投了。60 秒是 10 秒的 6 倍，余量足够。

---

## 五、三处设计取舍（被别人问到时这样答）

| 问题 | 选择 | 理由 | 被否决的方案 |
|---|---|---|---|
| `retrying` 要不要真的落库？ | **落库**（5 个状态） | 查询时能一眼筛出"正在等重试"的事件，运维价值直接；代价只是抢占 SQL 用 `IN ('pending','retrying')` | 只用 4 个状态、`retrying` 靠"`pending` 且 `attempts_count>0` 且未到期"派生——省一个状态，但每次查询都要现算，且没法直接过滤 |
| 崩溃回收回哪个状态？ | **`pending`** | `pending` 的语义就是"排队等投递"，回收 = 重回队列头部；`retrying` 专指"失败过、在等退避"，不要混用 | 回 `retrying`——概念上也能圆，但会出现"`retrying` 且 `attempts_count=0"` 这种自相矛盾的行 |
| 重试计数怎么数？ | **两个计数器**：`events.attempts_count`（本轮预算，人工重试清零）与 `attempts.attempt_no`（第几次物理投递，永不清零） | 一个回答"还剩几次机会"，一个回答"这是第几次投递"，问题不同，不能合并 | 只留一个——人工重试后要么覆盖历史编号，要么算不清剩余额度 |

**还有一个更根本的取舍**（写在 `04-data-model.md`）：**数据库是唯一真相来源，channel 只做唤醒。** 如果 channel 里装事件本身，进程崩溃时队列里的任务就没了，直接违背"事件 100% 落库不丢"。

---

## 六、这张流程里，我学到了什么

| 技术点 | 在这条流程的哪个位置被用到 |
|---|---|
| `net/http` 服务端 | 接收 handler、返回 202 / 404 / 410 / 413 |
| `net/http` 客户端 + `context` | worker 发 POST，10 秒超时靠 `context.WithTimeout` |
| goroutine + channel | worker 循环、`chan struct{}` 唤醒信号、`select` 收优雅退出信号 |
| SQLite 事务 | 接收写库、投递结果结算（attempt + event 必须同事务） |
| 乐观并发控制 | 原子抢占 `UPDATE ... WHERE status=...` + `RowsAffected()` 判定 |
| 幂等与至少一次 | `X-Webhook-Event-Id` 透传、租约回收后的重投 |
| 结构化日志 | 每次状态转移打一条 `slog`，出问题按 `event_id` 串起来 |

**必须自己写、不能让 AI 代写的三处**（项目书评审第七节）：worker 取任务的并发逻辑、状态机流转、退避重试与超时回收的边界判断。
