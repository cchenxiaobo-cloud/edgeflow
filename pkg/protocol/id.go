package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// randRead 是熵源读取函数（包级变量，测试可注入失败路径，P3-1 先例保留）。
var randRead = rand.Read

// newID 生成消息唯一 ID（32 位十六进制随机串，等价于 128 位随机数）。
// 首选 crypto/rand（分布式环境下不冲突；零依赖原则，不引入第三方 UUID 库）。
//
// v0.23.0（CHN-16，主线裁决）：删除「纳秒时间戳 + 进程内计数器」降级路径，
// 熵源失败即报错——msg.ID 承载云边信封的去重（dedup_keys）与请求-响应关联
// （CorrelationID 配对）语义，静默降级为低熵可预测 ID 会放大碰撞与伪造面；
// 与 OPC-UA nonce（B 路，PRT-06）、发布 ID（C 路，newReleaseID）统一为
// 「rand 失败即报错」策略。调用方对错误显式处理（NewMessage 直接透传）。
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := randRead(b); err != nil {
		return "", fmt.Errorf("protocol: crypto/rand 读取失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}
