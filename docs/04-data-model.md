# Webhook Gateway —— 数据模型（V0.1）

> 文档编号：docs/04-data-model.md
> 定位：三张表的字段、索引与并发规则。**这是本项目第一个落定的核心设计决策。**
> 状态流转见 `docs/02-flow.md`，业务规则见 `docs/01-business.md`。

---

## 一、第一原则：数据库是唯一真相来源

> **事件只存在于数据库里。内存中的 channel 只负责"叫醒 worker"，不搬运数据。**

`docs/00-project-charter.md` 原稿写的是"内存 channel + 数据库轮询兜底"。这个方向对，但如果把**事件本身**放进 channel，进程崩溃时队列里的任务就一起没了——直接违背"事件 100% 落库不丢"的第一目标。

正确的形态是三条：

| # | 规则 | 为什么 |
|---|---|---|
| 1 | 接收时先 `INSERT`，**事务提交之后**才往 channel 发信号 | 保证 202 一定对应一条已落库的记录，不存在"回了 202 但库里没有" |
| 2 | channel 类型是 `chan struct{}`，容量 1，**非阻塞发送**（满了就丢） | 它只是闹钟，丢了数据也不会丢；阻塞发送反而会拖慢接收 |
| 3 | worker **不看 channel 的内容**，一律从库里捞：`WHERE status IN ('pending','retrying') AND next_attempt_at <= now` | 数据的来源只有一个，不存在"队列里有一份、库里有一份"的双真相 |

**这个决策的收益**：M3（队列语义）和 M6（崩溃恢复）变成同一个机制的两个侧面——重启后 channel 是空的，但只要启动时触发一次信号、之后每秒 tick 一下，worker 就会把 `pending` 和租约过期的 `processing` 事件全部捡回来继续投。**省掉一整段返工。**

配套的两个细节：**原子抢占**（见第五节）和 **worker 数默认 1**（SQLite 是单写者，V0.1 并发设 1 最稳，但代码按 N 个 worker 的结构写）。

---

## 二、三张表总览

```
┌─────────────────┐          ┌──────────────────────┐          ┌────────────────────┐
│   endpoints     │ 1      N │       events         │ 1      N │      attempts      │
│─────────────────│──────────│──────────────────────│──────────│────────────────────│
│ id        PK    │          │ id            PK     │          │ id           PK    │
│ name            │          │ endpoint_id   FK ────┘          │ event_id     FK ───┘
│ url             │          │ status               │          │ attempt_no         │
│ enabled         │          │ payload / headers    │          │ status             │
│ created_at      │          │ attempts_count       │          │ response_status    │
│ disabled_at     │          │ next_attempt_at      │          │ error              │
└─────────────────┘          │ locked_until         │          │ duration_ms        │
   投递目标                     │ last_error           │          │ started_at         │
   长期存在、可停用             │ created_at/updated_at│          │ finished_at        │
                              │ delivered_at         │          └────────────────────┘
                              └──────────────────────┘            一次投递的审计记录
                                待投递的事 + 它的状态              只追加，写完不改
```

一句话记忆：**端点决定往哪送，事件决定送什么、送到哪一步，尝试记录每一次送的过程。**

---

## 三、建表语句（`internal/store/schema.sql`）

> W3 时用 `//go:embed` 嵌进二进制，启动时执行（幂等，可重复跑）。

```sql
-- 1) 端点：投递目标
CREATE TABLE IF NOT EXISTS endpoints (
    id          TEXT    PRIMARY KEY,          -- 形如 ep_7f3a9c1b2d4e5f607182930a
    name        TEXT    NOT NULL,             -- 人类可读名字，仅用于识别
    url         TEXT    NOT NULL,             -- 投递目标地址
    enabled     INTEGER NOT NULL DEFAULT 1,   -- 1=启用 0=停用（SQLite 无布尔类型）
    created_at  INTEGER NOT NULL,             -- Unix 秒
    disabled_at INTEGER                       -- 停用时刻；NULL 表示未停用
);

-- 2) 事件：业务对象，有状态
CREATE TABLE IF NOT EXISTS events (
    id              TEXT    PRIMARY KEY,      -- 形如 ev_2b81d0a9c3f74e5f60718293
    endpoint_id     TEXT    NOT NULL REFERENCES endpoints(id),
    status          TEXT    NOT NULL
                    CHECK (status IN ('pending','processing','delivered','retrying','failed')),
    payload         BLOB    NOT NULL,         -- 原始 body 字节，原样透传
    headers         TEXT    NOT NULL DEFAULT '{}',  -- JSON；投递时透传的请求头
    attempts_count  INTEGER NOT NULL DEFAULT 0,     -- 本轮预算已用次数（人工重试清零）
    next_attempt_at INTEGER NOT NULL,         -- 下次可投递时间；退避唯一落点
    locked_until    INTEGER,                  -- processing 租约到期时间；其他状态为 NULL
    last_error      TEXT,                     -- 最近一次失败原因，便于一眼查
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    delivered_at    INTEGER                   -- 送达时刻；NULL 表示未送达
);

-- worker 捞任务：WHERE status IN (...) AND next_attempt_at <= now ORDER BY next_attempt_at
CREATE INDEX IF NOT EXISTS idx_events_due
    ON events(status, next_attempt_at);

-- 崩溃回收：WHERE status='processing' AND locked_until < now
CREATE INDEX IF NOT EXISTS idx_events_lease
    ON events(status, locked_until);

-- 事件列表按端点分页：WHERE endpoint_id=? ORDER BY created_at DESC
CREATE INDEX IF NOT EXISTS idx_events_endpoint
    ON events(endpoint_id, created_at DESC);

-- 3) 投递尝试：审计记录，只追加
CREATE TABLE IF NOT EXISTS attempts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,  -- 纯内部序号，不出现在 URL 里
    event_id        TEXT    NOT NULL REFERENCES events(id),
    attempt_no      INTEGER NOT NULL,          -- 该事件第几次物理投递，从 1 开始，永不清零
    status          TEXT    NOT NULL
                    CHECK (status IN ('success','failed')),
    response_status INTEGER,                   -- HTTP 状态码；网络错误/超时为 NULL
    error           TEXT,                      -- 失败原因原文
    duration_ms     INTEGER NOT NULL,          -- 本次往返耗时
    started_at      INTEGER NOT NULL,
    finished_at     INTEGER NOT NULL,
    UNIQUE (event_id, attempt_no)              -- 自带 (event_id, attempt_no) 索引，无需另建
);
```

---

## 四、字段为什么要这么设计

### 4.1 `events.payload` 存 BLOB 原文，不存解析后的 JSON

网关是**透明管道**：它不理解业务语义，只负责把字节送过去。

- 存 `BLOB` 原文 → 目标收到的东西和发来的一模一样，包括格式错误、多余空格、非 UTF-8 字节。
- 若解析成 JSON 再重新序列化 → 键顺序、空格、数字精度都可能被改变，**网关悄悄改了用户的数据**，这是不可接受的。

### 4.2 `events.headers` 存 JSON 字符串

投递时要还原客户端的请求头（至少 `Content-Type` 和自定义头），否则目标方可能解析不了 body。用 `TEXT` 存 JSON 而不是拆成子表：**V0.1 不需要按 header 查询**，拆表只增加复杂度。

### 4.3 三个时间字段各管一件事

| 字段 | 回答什么问题 | 谁写 |
|---|---|---|
| `next_attempt_at` | "什么时候该再试一次" | 接收时 = now；退避时 = now + 间隔 |
| `locked_until` | "这条被谁占到什么时候" | 抢占时 = now + 60s；结算、回收时清空 |
| `delivered_at` | "什么时候送到的" | 成功时 = now |

**时间戳全部由 Go 侧生成并作为参数传入**（`time.Now().Unix()`），不依赖 SQLite 的时间函数——可测、可读、换库不受影响。

### 4.4 两个计数器，绝不能合并

| 计数器 | 含义 | 清零吗 |
|---|---|---|
| `events.attempts_count` | **本轮**重试预算已用几次（决定"还剩几次机会"） | 人工重试时清零 |
| `attempts.attempt_no` | 该事件**历史上**第几次物理投递（决定"这是第几次"） | 永不清零 |

合并成一个的后果：人工重试后要么历史编号被覆盖（审计断了），要么算不清剩余额度（重试逻辑出错）。**这是两个不同的问题，所以要两个字段。**

### 4.5 主键为什么用带前缀的随机字符串

| 方案 | 优点 | 缺点 |
|---|---|---|
| `INTEGER PRIMARY KEY AUTOINCREMENT` | 写法最省事、天然有序 | URL 里暴露系统处理了多少条事件（能猜规模）；导入/合并数据时容易冲突 |
| **`TEXT` + 前缀 + 随机 hex**（选用） | 不暴露规模、不可猜；天然适合当接收方的幂等去重键；多实例/迁移不冲突 | 占空间略大；插入非单调递增 |

ID 形如 `ep_7f3a9c1b2d4e5f607182930a` / `ev_2b81d0a9c3f74e5f60718293`（前缀 + 12 字节随机 hex，共 27 个字符）。**用标准库 `crypto/rand` 五行代码生成，不引入第三方 UUID 库**——保持零依赖纪律。`ep_` / `ev_` 前缀让人一眼看出这是什么东西，排查日志时很省事。

> `docs/02-flow.md` 的时间线表格里为了排版写成了 `ep_7f3a9c` 这样的缩写，实际长度是 27 个字符。

> 补充：SQLite 里 `TEXT PRIMARY KEY` 的表仍然有隐藏的 `rowid` 作为 B-tree 排序键，**索引性能不受影响**，所以这个选择没有代价上的顾虑。

---

## 五、并发安全：原子抢占与租约

### 5.1 抢占为什么必须是一条 UPDATE

**错误写法**（先查再改）：

```sql
SELECT id FROM events WHERE status='pending' AND next_attempt_at<=? LIMIT 1;  -- 两个 worker 都查到同一条
UPDATE events SET status='processing' WHERE id=?;                            -- 两个都改了 → 重复投递
```

**正确写法**（一条语句完成判断和修改）：

```sql
UPDATE events
   SET status = 'processing',
       locked_until = ?,
       updated_at = ?
 WHERE id = ?
   AND status IN ('pending','retrying');   -- 守卫条件写在 WHERE 里
-- Go 侧看 结果.RowsAffected() == 1 才算抢到；== 0 说明被别人抢先，回去捞下一条
```

数据库保证 `UPDATE` 的原子性，**"判断 + 修改"变成一个不可分割的动作**，从根上消除竞态。`RowsAffected()` 就是这场竞争的裁判——这是 SQLite 版本里最优雅的一处，比加锁简单得多。

> 这一句 `RowsAffected() == 1` 同时承担着**拦截非法状态转换**的职责。完整的四层防线（WHERE 守卫 / 行数判定 / CHECK 约束 / 测试）和各动作的 WHERE 前置条件清单，见 `docs/02-flow.md` 第 3.5 节。

### 5.2 租约 60 秒的取值逻辑

```
投递超时 10s  <  租约 60s   ← 必须满足这个关系
```

如果租约小于投递超时，会出现：worker A 还在投第 1 次，租约就过期了，回收逻辑把事件拉回 `pending`，worker B 又投一次——**同一条事件被两个 worker 同时投递**，比"晚一点恢复"糟糕得多。60 秒 = 10 秒的 6 倍，余量充足。

### 5.3 事务边界只有两处

| 事务 | 包含的操作 | 理由 |
|---|---|---|
| 接收落库 | `INSERT events` | 单语句本身就是事务，无需显式 BEGIN |
| 投递结算 | `INSERT attempts` + `UPDATE events` | **必须同事务**：要么"记录了这次投递 + 状态也更新了"，要么都不发生。否则会出现状态是 `delivered` 但查不到成功记录的矛盾数据 |

---

## 六、索引与它服务的查询

**规则：每个索引背后必须有一条真实存在的查询，没有对应查询的索引一律删掉。**

| 索引 | 服务的查询 | 出现频率 |
|---|---|---|
| `idx_events_due (status, next_attempt_at)` | worker 捞到期任务 | 每次 tick |
| `idx_events_lease (status, locked_until)` | 崩溃回收扫描 | 启动时 + 每次 tick |
| `idx_events_endpoint (endpoint_id, created_at DESC)` | 事件列表按端点分页 | 按需查询 |
| `attempts` 的 `UNIQUE(event_id, attempt_no)` | 查某事件的投递历史（自带的索引） | 按需查询 |

不建索引的两个地方，理由也一样：`endpoints` 只有几十行，全表扫描比走索引还快；`events.status` 单独建索引无用——**选择度太低**（5 个取值），必须配合时间字段才有意义。

---

## 七、状态字段与状态机的一致性

代码里的状态机（`02-flow.md` 第 3 节）必须和表里的字段约束严格对齐：

| 状态 | `next_attempt_at` | `locked_until` | 能否被 worker 抢占 | 能否被手动重试 |
|---|---|---|---|---|
| `pending` | 有值 | NULL | ✅ | ❌ |
| `retrying` | 有值（未来） | NULL | ✅（到点后） | ❌ |
| `processing` | 有值（已过期） | 有值（未来） | ❌ | ❌ |
| `delivered` | 有值 | NULL | ❌ | ❌（返回 409） |
| `failed` | 有值 | NULL | ❌ | ✅ |

`CHECK (status IN (...))` 是**数据库层的兜底**：即使代码写错了状态字符串，也写不进去，不会产生一个谁都解释不了的行。

代价要说清楚：SQLite 改 `CHECK` 约束需要重建表。**V0.1 的五个状态已经定死，值得用这个代价换"不可能写入非法状态"的保险。**

---

## 八、表结构变更怎么办（迁移策略）

新手最容易犯的错：改字段时直接删库重来，数据就没了。V0.1 用**最简方案**，约 20 行代码：

```
启动时读 PRAGMA user_version
  ├─ 版本号 == 当前期望值 → 什么都不做
  ├─ 版本号 < 期望值 → 按顺序执行升级脚本 → 更新 user_version
  └─ 版本号 > 期望值 → 报错退出（数据库比程序新，不能瞎动）
```

**不引入 `goose` / `golang-migrate`**——V0.1 用不上，多一个依赖多一份要学的东西。`CREATE TABLE IF NOT EXISTS` 保证首次启动幂等，`user_version` 负责后续有条件升级。

---

## 九、被否决的方案（记录理由，避免以后重走）

| 方案 | 否决原因 |
|---|---|
| channel 里装事件数据 | 进程崩溃时队列里的任务丢失，违背"100% 落库不丢" |
| 用内存定时器做退避 | 重启后计时器归零，退避进度丢失；`next_attempt_at` 天然抗重启 |
| 先 `SELECT` 再 `UPDATE` 抢占 | 存在竞态窗口，两个 worker 会抢到同一条事件 |
| `payload` 存解析后的 JSON | 网关会悄悄改动用户数据，破坏"透明管道"定位 |
| 合并 `attempts_count` 与 `attempt_no` | 人工重试后必然出现"审计断链"或"额度算错"二选一 |
| 主键用自增整数 | URL 暴露系统规模，且跨实例迁移易冲突 |
| 引入 goose / golang-migrate | V0.1 的迁移需求是 `user_version` 一个 PRAGMA 就能覆盖的量级 |
| `retrying` 不落库（靠派生） | 没法直接按状态过滤，每次查询都要现算；见 `02-flow.md` 第五节 |
