# Webhook Gateway —— API 设计（V0.1）

> 文档编号：docs/05-api.md
> 定位：全部 HTTP 接口的契约。请求、响应、状态码、错误形状都在这里。
> 业务规则见 `docs/01-business.md`，状态机见 `docs/02-flow.md`。

---

## 一、通用约定

| 项 | 约定 |
|---|---|
| 传输格式 | JSON（`application/json; charset=utf-8`） |
| 时间字段 | Unix 秒（整数） |
| 成功响应 | 直接返回资源对象，或 `{"<集合名>": [...]}` |
| 错误响应 | 统一形状 `{"error": "<原因>"}`，只有一个字段 |
| ID 格式 | 端点 `ep_` + 24 位 hex；事件 `ev_` + 24 位 hex（共 27 字符） |

**错误响应只有一个字段 `error`**：调用方判断失败原因时不用猜是哪个 key。

---

## 二、接口总览

| 方法 | 路径 | 用途 | 关键状态码 |
|---|---|---|---|
| GET | `/healthz` | 存活探针（顺便摸数据库） | 200 / 503 |
| POST | `/api/endpoints` | 创建投递目标 | 201 / 400 |
| GET | `/api/endpoints` | 列出全部投递目标 | 200 |
| POST | `/api/endpoints/{id}/disable` | 停用投递目标 | 200 / 404 |
| POST | `/hooks/{endpoint_id}` | 事件入口 | 202 / 404 / 410 / 413 / 500 |
| GET | `/api/events` | 事件列表（过滤 / 分页） | 200 / 400 |
| GET | `/api/events/{id}` | 事件详情（含 payload） | 200 / 404 |
| GET | `/api/events/{id}/attempts` | 投递历史 | 200 / 404 |
| POST | `/api/events/{id}/retry` | 手动重试 | 200 / 404 / 409 |

---

## 三、接口明细

### 3.1 存活探针

```
GET /healthz
```

- `200` → `{"status":"ok"}`
- `503` → 数据库不可用（进程活着 ≠ 能干活）

### 3.2 创建端点

```
POST /api/endpoints
{"name": "订单系统", "url": "http://localhost:9000/hooks/pay"}
```

校验：`name` 非空；`url` 必须是完整的 `http://` 或 `https://` 地址。

- `201` → 返回端点对象（含 `id` 和 `hook_path`）
- `400` → 请求体不是合法 JSON / `name` 为空 / `url` 不合法

响应示例：

```json
{
  "id": "ep_7f3a9c1b2d4e5f607182930a",
  "name": "订单系统",
  "url": "http://localhost:9000/hooks/pay",
  "enabled": true,
  "hook_path": "/hooks/ep_7f3a9c1b2d4e5f607182930a",
  "created_at": 1750000000
}
```

### 3.3 列出端点

```
GET /api/endpoints
```

- `200` → `{"endpoints": [ {...}, ... ]}`（按创建时间升序）

### 3.4 停用端点

```
POST /api/endpoints/{id}/disable
```

**幂等**：对已停用的端点再停一次仍返回 200。

- `200` → 返回端点对象（`enabled: false`）
- `404` → 端点不存在

> 业务规则 B10：停用只影响新事件，在途事件继续投完。

### 3.5 事件入口（接收）

```
POST /hooks/{endpoint_id}
<任意 payload，最多 1 MB>
```

这是整条链路的入口。校验顺序固定：端点不存在 → 404，端点已停用 → 410，payload 超 1 MB → 413。

- `202` → `{"event_id": "ev_...", "status": "pending"}`（**只代表"记住了"，不代表"送到了"**）
- `404` → 端点不存在（B1）
- `410` → 端点已停用（B2）
- `413` → payload 超过 1 MB（B3）
- `500` → 落库失败（B4：绝不回 202）

### 3.6 事件列表

```
GET /api/events?status=failed&limit=20&offset=0
```

| 参数 | 说明 | 默认 |
|---|---|---|
| `status` | 五状态之一，省略则不过滤 | 无 |
| `limit` | 单页条数（1~100） | 20 |
| `offset` | 跳过的条数 | 0 |

- `200` → `{"events": [ {...}, ... ]}`（创建时间倒序，**不含 payload / headers**）
- `400` → `status` 非法 / `limit`、`offset` 非法

摘要字段：`id`、`endpoint_id`、`status`、`attempts_count`、`next_attempt_at`、`last_error`、`created_at`、`updated_at`、`delivered_at`。

### 3.7 事件详情

```
GET /api/events/{id}
```

- `200` → 完整事件，在摘要基础上额外带 `payload_base64`（base64 编码）与 `headers`（原样 JSON）
- `404` → 事件不存在

> payload 是原始字节（BLOB），可能是非 UTF-8 内容，故用 base64 无损表示，不当字符串直接塞 JSON。

### 3.8 投递历史

```
GET /api/events/{id}/attempts
```

- `200` → `{"attempts": [ {...}, ... ]}`（按 `attempt_no` 升序，第 1 次在前）
- `404` → 事件不存在

attempt 字段：`attempt_no`、`status`（success/failed）、`response_status`（网络错误为 null）、`error`、`duration_ms`、`started_at`、`finished_at`。

### 3.9 手动重试

```
POST /api/events/{id}/retry
```

- `200` → 重试成功，返回事件摘要（此时 `status` 已变 `pending`，`attempts_count` 清零）
- `404` → 事件不存在
- `409` → 当前状态不是 `failed`（例如 `delivered`，业务规则 B11）

> 语义（B12/B13）：重置一整轮预算、立即投递，**不删任何历史 Attempt**——第 6 次物理投递就是第 6 条记录。

---

## 四、状态码一览

| 状态码 | 含义 | 出现场景 |
|---|---|---|
| 200 | 成功 | 查询、停用、手动重试 |
| 201 | 创建成功 | 创建端点 |
| 202 | 已受理 | 接收事件（语义锚点） |
| 400 | 请求错误 | 参数 / JSON 不合法 |
| 404 | 不存在 | 端点 / 事件不存在 |
| 409 | 冲突 | 手动重试非 failed 事件 |
| 410 | 已停用 | 停用端点收新事件 |
| 413 | 过大 | payload 超 1 MB |
| 500 | 服务器内部错误 | 数据库读写失败 |
| 503 | 服务不可用 | 健康检查时数据库不可用 |

---

## 五、一条事件的一生怎么用 API 串起来

```bash
# 1. 创建端点
curl -X POST http://localhost:8080/api/endpoints \
  -H "Content-Type: application/json" \
  -d '{"name":"订单系统","url":"http://localhost:9000/hooks/pay"}'

# 2. 发一条事件（拿到 event_id）
curl -X POST http://localhost:8080/hooks/ep_xxx -d '{"orderId":"A-1"}'

# 3. 查它走到哪一步
curl http://localhost:8080/api/events/ev_xxx

# 4. 查投递历史（审计）
curl http://localhost:8080/api/events/ev_xxx/attempts

# 5. 失败了就手动救一次
curl -X POST http://localhost:8080/api/events/ev_xxx/retry
```

完整演示脚本见 README。
