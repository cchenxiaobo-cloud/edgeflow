package protocol

import (
	"crypto/rand"
	"encoding/hex"
)

// newID 生成消息唯一 ID（32 位十六进制随机串，等价于 128 位随机数）。
// 用 crypto/rand 保证分布式环境下不冲突；不引入第三方 UUID 库（零依赖原则）。
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
