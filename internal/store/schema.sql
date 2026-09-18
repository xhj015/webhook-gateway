-- Webhook Gateway V0.1 表结构
-- 对应文档：docs/04-data-model.md 第三节
--
-- 全部语句都是 IF NOT EXISTS，可以反复执行（幂等）。
-- 改表结构的步骤：把新语句追加到本文件末尾，再把 store.go 里的 schemaVersion +1。

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
