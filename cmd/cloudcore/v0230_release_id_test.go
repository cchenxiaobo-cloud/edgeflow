package main

import (
	"crypto/rand"
	"errors"
	"regexp"
	"testing"
)

// v0.23.0（CHN-16）：newReleaseID 熵源失败注入测试（releaseIDRandRead
// 包级变量注入，与 pkg/protocol/id_test.go 同一先例）。验证「rand 失败
// 即报错」语义：无兜底 ID 产出，错误信息可定位。
func TestNewReleaseIDRandFailure(t *testing.T) {
	orig := releaseIDRandRead
	releaseIDRandRead = func(_ []byte) (int, error) { return 0, errors.New("entropy unavailable") }
	defer func() { releaseIDRandRead = orig }()

	id, err := newReleaseID()
	if err == nil {
		t.Fatalf("newReleaseID should fail when crypto/rand fails, got id=%q (fallback path must not exist, CHN-16)", id)
	}
	if id != "" {
		t.Errorf("id = %q on error, want empty string (no fallback ID)", id)
	}
	if !regexp.MustCompile(`crypto/rand`).MatchString(err.Error()) {
		t.Errorf("err = %v, want message mentioning crypto/rand for diagnosability", err)
	}
}

// 正常路径：熵源恢复后 newReleaseID 回到 32 位十六进制随机 ID。
func TestNewReleaseIDRandomOK(t *testing.T) {
	// 确认注入恢复（防御性：上一测试 defer 恢复后此处应已是真 rand）
	if _, err := releaseIDRandRead(make([]byte, 1)); err != nil {
		t.Skipf("entropy source still broken, skip: %v", err)
	}
	id, err := newReleaseID()
	if err != nil {
		t.Fatalf("newReleaseID error: %v", err)
	}
	if len(id) != 32 {
		t.Errorf("id length = %d, want 32 (128-bit hex)", len(id))
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Errorf("id = %q, want 32-char lowercase hex", id)
	}
	// 参照 crypto/rand 直接读，格式一致性 sanity（不比随机性）
	ref := make([]byte, 16)
	if _, err := rand.Read(ref); err != nil {
		t.Skipf("crypto/rand unavailable: %v", err)
	}
}
