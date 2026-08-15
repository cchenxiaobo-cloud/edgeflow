package protocol

import (
	"errors"
	"testing"
	"unicode"
)

// TestNewIDFallbackOnRandError 验证 P3-1：crypto/rand 失败时 newID 回退到
// 「时间戳 + 计数器」——不 panic、不产出全零 ID，格式保持 32 位十六进制，
// 且回退路径下 ID 进程内唯一。
func TestNewIDFallbackOnRandError(t *testing.T) {
	orig := randRead
	randRead = func(_ []byte) (int, error) { return 0, errors.New("entropy unavailable") }
	defer func() { randRead = orig }()

	seen := make(map[string]struct{})
	for i := 0; i < 2000; i++ {
		id := newID()
		if len(id) != 32 {
			t.Fatalf("回退 ID 长度 = %d，期望 32", len(id))
		}
		for _, c := range id {
			if !unicode.Is(unicode.Hex_Digit, c) {
				t.Fatalf("回退 ID 含非十六进制字符 %q", c)
			}
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("回退 ID 重复: %s", id)
		}
		seen[id] = struct{}{}
	}
}

// TestNewIDRandomUnique 验证正常路径（crypto/rand 可用）下 newID 保持
// 32 位十六进制格式且不重复；顺带确认 randRead 注入恢复后正常路径不受影响。
func TestNewIDRandomUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := newID()
		if len(id) != 32 {
			t.Fatalf("ID 长度 = %d，期望 32", len(id))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("随机 ID 重复: %s", id)
		}
		seen[id] = struct{}{}
	}
}
