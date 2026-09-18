// Package idgen 生成带前缀的随机 ID，形如 ep_7f3a9c1b2d4e5f607182930a。
//
// 为什么不用自增整数：URL 里会暴露"系统处理了多少条事件"，而且跨实例迁移容易冲突。
// 为什么不引第三方 UUID 库：标准库 crypto/rand 就够，保持零依赖纪律。
// 前缀（ep_ / ev_）的价值是排查日志时一眼看出这是什么东西。
package idgen

import (
	"crypto/rand"
	"encoding/hex"
)

const prefixEndpoint = "ep_"
const prefixEvent = "ev_"

// Endpoint 生成端点 ID，形如 ep_7f3a9c1b2d4e5f607182930a。
func Endpoint() string { return newID(prefixEndpoint) }

// Event 生成事件 ID，形如 ev_2b81d0a9c3f74e5f60718293。
// 投递时会作为 X-Webhook-Event-Id 头带给目标方，供其幂等去重。
func Event() string { return newID(prefixEvent) }

// newID 返回 prefix + 12 字节随机数的十六进制（12 字节 = 24 个 hex 字符，加前缀共 27 字符）。
//
// 随机源读失败时直接 panic 而不是返回空串：crypto/rand 失效意味着
// 系统熵池出了问题，此时继续跑下去只会产生可预测甚至重复的 ID，
// 宁可让进程当场挂掉，也不要静默生成一堆可能撞车的 ID。
func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("idgen: 读取随机数失败: " + err.Error())
	}
	return prefix + hex.EncodeToString(b)
}
