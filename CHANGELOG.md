# Changelog

本项目遵循 [Conventional Commits](docs/06-git-workflow.md)，提交历史本身就是变更日志。本文件按版本汇总。

## [v0.1.0] - 2026-11-13

首个完整版本：接收 → 落库 → 异步投递 → 自动重试 → 查询与手动重试，五环节闭环。

### 新增功能（feat）

- 接收链路：`POST /hooks/{id}` 先落库再回 202，边界校验 404 / 410 / 413
- 投递链路：后台 worker 原子抢占 + 10 秒超时投递，透传 payload 与请求头，附加 `X-Webhook-Event-Id` / `X-Webhook-Attempt` 头
- 自动重试：递增退避 5s→30s→2min→10min→1h，最多 5 次，耗尽转 `failed`
- 崩溃恢复：processing 租约 60 秒过期回收，重启不丢不卡
- 端点管理：创建、列表、停用（停用只影响新事件）
- 事件查询：列表（状态过滤 / 分页）、详情、投递历史
- 手动重试：`failed` → 一键拉回 `pending`，历史 Attempt 不删
- 优雅退出：收到信号后在途请求收尾后再停机

### 测试（test）

- 状态机四用例：原子抢占 / 结算成功 / 结算重试与耗尽 / 租约回收
- store 查询与状态操作测试：停用 / 列表 / 手动重试（含历史保留）
- API handler 端到端测试：410 / 停用 / 查询 / 重试边界

### 文档（docs）

- 五份设计文档齐备：业务（01）/ 流程（02）/ 范围与 Backlog（03）/ 数据模型（04）/ API（05）
- README：10 分钟跑起来 + 一分钟主链路演示
- 每周复盘模板 `08-retrospective.md`

### 工程化（chore）

- Dockerfile 多阶段构建，非 root 运行
- SQLite WAL + 三 PRAGMA + `user_version` 迁移
- 结构化日志（`log/slog`）
