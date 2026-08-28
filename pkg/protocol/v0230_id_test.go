package protocol

import (
	"errors"
	"strings"
	"testing"
	"unicode"
)

// errTestEntropy 是测试注入的熵源错误。
var errTestEntropy = errors.New("entropy unavailable")

// TestNewIDFailsOnRandError（v0.23.0，CHN-16 主线裁决）：删除时间戳降级后，
// 熵源失败必须显式报错——与 OPC-UA nonce（PRT-06）、发布 ID（newReleaseID）
// 统一「rand 失败即报错」策略。取代码路线：错误非空、不产出 ID。
func TestNewIDFailsOnRandError(t *testing.T) {
	orig := randRead
	randRead = func(_ []byte) (int, error) { return 0, errTestEntropy }
	defer func() { randRead = orig }()

	id, err := newID()
	if err == nil {
		t.Fatalf("熵源失败应返回错误，got id=%q err=nil", id)
	}
	if id != "" {
		t.Fatalf("熵源失败不得产出 ID，got %q", id)
	}
	if !strings.Contains(err.Error(), "crypto/rand") {
		t.Fatalf("错误信息应指明熵源，got: %v", err)
	}
}

// TestNewIDRandomUnique（承接原 TestNewIDRandomUnique 语义）：正常路径
// 保持 32 位十六进制格式且不重复；randRead 注入恢复后不受影响。
func TestNewIDRandomUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id, err := newID()
		if err != nil {
			t.Fatalf("正常路径不应报错: %v", err)
		}
		if len(id) != 32 {
			t.Fatalf("ID 长度 = %d，期望 32", len(id))
		}
		for _, c := range id {
			if !unicode.Is(unicode.Hex_Digit, c) {
				t.Fatalf("ID 含非十六进制字符 %q", c)
			}
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("随机 ID 重复: %s", id)
		}
		seen[id] = struct{}{}
	}
}

// TestNewMessageRandFailurePropagates：NewMessage 对 newID 错误逐字透传
// （签名本就返回 error，零调用方结构改动）。
func TestNewMessageRandFailurePropagates(t *testing.T) {
	orig := randRead
	randRead = func(_ []byte) (int, error) { return 0, errTestEntropy }
	defer func() { randRead = orig }()

	msg, err := NewMessage(TypeRegister, "edge-1", "cloud-1", map[string]string{"k": "v"})
	if err == nil {
		t.Fatalf("NewMessage 应透传熵源错误，got msg=%+v err=nil", msg)
	}
	if msg != nil {
		t.Fatalf("失败路径不得返回半成品消息，got %+v", msg)
	}
}
