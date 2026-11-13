# Webhook Gateway

把「不可靠的一次 HTTP 请求」，变成「有记录、可追踪、可重试的一次事件投递」。

一个轻量级 Webhook 接收、转发与可靠投递服务：接收事件 → 落库 → 异步投递 → 失败自动重试 → 查询与手动重试。单二进制 + SQLite，零外部依赖，不引入消息队列中间件。

- 语言：Go（`net/http` + `chi`）
- 存储：SQLite（`modernc.org/sqlite`，纯 Go，无 CGO）
- 投递语义：**至少一次**（保证不丢，不保证不重复，靠 `X-Webhook-Event-Id` 让目标方幂等去重）

---

## 一、10 分钟跑起来

### 方式 A：本地 Go 运行

```bash
# 需要 Go 1.27+
go run ./cmd/webhook-gw -db ./data.db -port 8080
```

看到 `webhook-gateway 已启动 addr=":8080"` 即成功。

### 方式 B：Docker

```bash
docker build -t webhook-gw .
docker run --rm -p 8080:8080 -v "$PWD/data:/data" webhook-gw
```

> 数据库文件落在挂载的 `/data` 目录里，重启不丢。

---

## 二、一分钟主链路演示

开两个终端：终端 A 起一个「目标服务」（收事件），终端 B 操作网关。

**终端 A —— 用一段 Go 起个目标：** 或者用任意能收 POST 并回 200 的服务。最简单的方式是起一个监听 9000 端口的 HTTP 服务。

**终端 B —— 操作网关：**

```bash
# 1. 创建投递目标（目标地址指向终端 A 的 9000 端口）
curl -X POST http://localhost:8080/api/endpoints \
  -H "Content-Type: application/json" \
  -d '{"name":"订单系统","url":"http://localhost:9000/hooks/pay"}'
# → 记下返回里的 id，下面用它

# 2. 发一条事件
curl -X POST http://localhost:8080/hooks/<端点id> \
  -H "Content-Type: application/json" \
  -d '{"orderId":"A-1"}'
# → 202 {"event_id":"ev_...","status":"pending"}

# 3. 等一秒，查它走到哪一步
curl http://localhost:8080/api/events/<事件id>
# → status 应为 "delivered"

# 4. 查投递历史
curl http://localhost:8080/api/events/<事件id>/attempts
```

---

## 三、接口清单

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/healthz` | 存活探针 |
| POST | `/api/endpoints` | 创建投递目标 |
| GET | `/api/endpoints` | 列出投递目标 |
| POST | `/api/endpoints/{id}/disable` | 停用投递目标 |
| POST | `/hooks/{endpoint_id}` | 事件入口 |
| GET | `/api/events` | 事件列表（可按 `status` 过滤、分页） |
| GET | `/api/events/{id}` | 事件详情 |
| GET | `/api/events/{id}/attempts` | 投递历史 |
| POST | `/api/events/{id}/retry` | 手动重试 |

完整契约（请求 / 响应 / 状态码 / 错误形状）见 [`docs/05-api.md`](docs/05-api.md)。

---

## 四、核心机制（一段话）

事件从 `POST /hooks/{id}` 进来，网关先把它落库成 `pending` 再回 202（**202 只代表"记住了"，不代表"送到了"**）。后台 worker 从库里捞到期事件、用一条 `UPDATE ... WHERE status IN ('pending','retrying')` 原子抢占成 `processing`，POST 到目标地址，把结果写成一条 `attempt` 审计。成功转 `delivered`；失败按 `5s → 30s → 2min → 10min → 1h` 推后 `next_attempt_at` 转 `retrying` 等重试；5 次用尽转 `failed`。进程崩了，`processing` 在 60 秒租约到期后被回收重投；`failed` 可人工一键拉回 `pending`，历史一条不删。**全程事件只有一处真相——数据库。**

详细流程、状态机、设计取舍见 [`docs/02-flow.md`](docs/02-flow.md)。

---

## 五、项目文档

| 文档 | 内容 |
|---|---|
| [`docs/00-project-charter.md`](docs/00-project-charter.md) | 项目总纲 |
| [`docs/01-business.md`](docs/01-business.md) | 业务逻辑与业务规则 |
| [`docs/02-flow.md`](docs/02-flow.md) | 完整流程与状态机 |
| [`docs/03-scope.md`](docs/03-scope.md) | 功能边界与 Backlog |
| [`docs/04-data-model.md`](docs/04-data-model.md) | 数据模型 |
| [`docs/05-api.md`](docs/05-api.md) | API 契约 |
| [`docs/06-git-workflow.md`](docs/06-git-workflow.md) | Git 工作流 |
| [`docs/08-retrospective.md`](docs/08-retrospective.md) | 每周复盘 |

---

## 六、开发

```bash
go build ./...        # 构建
go vet ./...          # 静态检查
go test ./...         # 测试
go test -race ./...   # 竞态检测
```

目录结构：

```
cmd/webhook-gw/       入口
internal/api/         路由与 handler（HTTP 接入层）
internal/store/       数据库访问（唯一真相来源）
internal/worker/      后台投递循环
internal/delivery/    投递 HTTP 客户端
internal/idgen/       ID 生成
```

## License

个人学习项目。
