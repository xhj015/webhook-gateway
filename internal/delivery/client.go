// Package delivery 负责把事件的 payload 真正 POST 到目标端点。
//
// 它只做一件事：发一次 HTTP 请求，并【如实】回报结果。
// 成败判定后的分支（delivered / retrying / failed）、重试决策、记账
// 都不归它管——那些是 worker 和 store 的职责。
// 这样切分的理由：投递是"手"，结算（事务）是"账本"，两者失败模式完全不同，
// 混在一个函数里就没法单独测试了。
package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/xhj015/webhook-gateway/internal/store"
)

// Timeout 是单次投递的超时，业务参数定死的 10 秒（docs/01-business.md 参数表）。
const Timeout = 10 * time.Second

// Client 是投递用的 HTTP 客户端。
// 整个进程复用一个实例：连接池、TLS 会话缓存都长在 Client 里，
// 每次投递新建 Client 等于每次都重新建 TCP 连接，白浪费。
type Client struct {
	hc *http.Client
}

// NewClient 组装投递客户端。这里不设 Client.Timeout，
// 超时改由每次请求的 context 控制（见 Deliver）——效果一样，
// 但"这次请求最多等多久"的决定权留给调用链，将来要调整更灵活。
func NewClient() *Client {
	return &Client{hc: &http.Client{}}
}

// Deliver 把 payload 原样 POST 到 targetURL，返回投递结果。
//
// 注意返回值不是 error——"目标挂了 / 超时 / 返回 500"是投递工作的【正常业务分支】，
// 不是程序错误。真正该用 error 表达的只有"我们自己的代码写错了"（如 URL 非法），
// 那种情况也折算成失败结果记进审计，而不是 panic 或向上抛。
//
// 判定标准（业务规则）：2xx 且在 10 秒内响应 = 成功，其余一律失败。
func (c *Client) Deliver(
	ctx context.Context,
	targetURL string,
	payload []byte,
	forwardHeaders map[string]string,
	eventID string,
	attemptNo int,
) store.DeliveryOutcome {
	start := time.Now()

	// 10 秒预算从这一刻起算：包含建连、发请求、等响应全过程。
	// 超时后 Do 会返回错误，且底层连接会被主动掐断。
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return store.DeliveryOutcome{
			Err:        "构造请求失败: " + err.Error(),
			DurationMS: elapsedMS(start),
		}
	}

	// 还原接收时存下的请求头（Content-Type 等，见 api/hook.go 的 collectForwardHeaders）
	for k, v := range forwardHeaders {
		req.Header.Set(k, v)
	}
	// 这两个头由网关覆盖写入（业务规则 B9）——即使调用方伪造过也会被盖掉：
	//   X-Webhook-Event-Id  事件 ID，目标方靠它做幂等去重（至少一次语义的配套）
	//   X-Webhook-Attempt   这是第几次物理投递，排查重复投递时一眼可辨
	req.Header.Set("X-Webhook-Event-Id", eventID)
	req.Header.Set("X-Webhook-Attempt", strconv.Itoa(attemptNo))

	resp, err := c.hc.Do(req)
	if err != nil {
		// 网络错误、DNS 失败、连接被拒、超时……全走这里。
		// ResponseStatus 保持 0：根本没有拿到 HTTP 层的回应。
		return store.DeliveryOutcome{
			Err:        "请求失败: " + err.Error(),
			DurationMS: elapsedMS(start),
		}
	}
	defer resp.Body.Close()

	// 把响应体读掉再丢弃（限制 64 KB 防止目标回传超大 body 撑内存）。
	// 不读完 body 连接就没法复用——HTTP keep-alive 的规矩。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	out := store.DeliveryOutcome{
		ResponseStatus: resp.StatusCode,
		DurationMS:     elapsedMS(start),
		Success:        resp.StatusCode >= 200 && resp.StatusCode < 300,
	}
	if !out.Success {
		out.Err = "目标返回非 2xx: " + strconv.Itoa(resp.StatusCode)
	}
	return out
}

// ParseForwardHeaders 把 events.headers 列里的 JSON 还原成 map。
// 库里的值是我们自己 json.Marshal 写进去的，正常不会解析失败；
// 万一坏了也只按"没有额外头"处理——投递不能因为一个头挂掉。
func ParseForwardHeaders(raw string) map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func elapsedMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
