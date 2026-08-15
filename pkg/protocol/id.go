package protocol

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync/atomic"
	"time"
)

// randRead 是熵源读取函数（包级变量，测试可注入失败路径，P3-1）。
var randRead = rand.Read

// fallbackIDCounter 是 crypto/rand 失败时的兜底计数器：进程内单调递增，
// 与时间戳组合保证同进程内唯一（P3-1）。
var fallbackIDCounter atomic.Uint64

// newID 生成消息唯一 ID（32 位十六进制随机串，等价于 128 位随机数）。
// 首选 crypto/rand（分布式环境下不冲突；零依赖原则，不引入第三方 UUID 库）；
// 熵源失败时（概率极低）回退到「纳秒时间戳 + 进程内递增计数器」——
// 不 panic、不产出全零 ID，ID 格式不变（32 位十六进制），唯一性从
// 密码学随机降级为进程内唯一（跨进程碰撞概率可忽略）。
func newID() string {
	b := make([]byte, 16)
	if _, err := randRead(b); err == nil {
		return hex.EncodeToString(b)
	}
	var fb [16]byte
	binary.BigEndian.PutUint64(fb[0:8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint64(fb[8:16], fallbackIDCounter.Add(1))
	return hex.EncodeToString(fb[:])
}
