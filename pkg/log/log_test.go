package log

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// captureOutput 临时将日志输出重定向到内存 buffer，执行 fn 后返回其输出。
// 关闭时间戳前缀，便于对内容做精确断言。
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := logger
	logger = log.New(&buf, "", 0)
	defer func() { logger = old }()
	fn()
	return buf.String()
}

// TestInfof 验证 Info 级别输出包含 [INFO] 前缀和格式化后的消息。
func TestInfof(t *testing.T) {
	out := captureOutput(t, func() { Infof("hello %s", "world") })
	want := "[INFO] hello world"
	if !strings.Contains(out, want) {
		t.Errorf("Infof 输出 = %q, 应包含 %q", out, want)
	}
}

// TestWarnf 验证 Warn 级别输出包含 [WARN] 前缀。
func TestWarnf(t *testing.T) {
	out := captureOutput(t, func() { Warnf("disk %d%% full", 90) })
	want := "[WARN] disk 90% full"
	if !strings.Contains(out, want) {
		t.Errorf("Warnf 输出 = %q, 应包含 %q", out, want)
	}
}

// TestErrorf 验证 Error 级别输出包含 [ERROR] 前缀。
func TestErrorf(t *testing.T) {
	out := captureOutput(t, func() { Errorf("boom: %v", "oops") })
	want := "[ERROR] boom: oops"
	if !strings.Contains(out, want) {
		t.Errorf("Errorf 输出 = %q, 应包含 %q", out, want)
	}
}

// TestLogfUnknownLevel 验证未知级别会兜底为 INFO，不产生异常输出。
func TestLogfUnknownLevel(t *testing.T) {
	out := captureOutput(t, func() { logf(Level(99), "fallback") })
	if !strings.Contains(out, "[INFO] fallback") {
		t.Errorf("logf 未知级别输出 = %q, 应包含 [INFO] fallback", out)
	}
}

// TestDefaultLoggerHasTimestamp 验证默认 logger 开启了时间戳（LstdFlags）。
func TestDefaultLoggerHasTimestamp(t *testing.T) {
	if logger.Flags()&log.LstdFlags == 0 {
		t.Error("默认 logger 未开启时间戳（log.LstdFlags）")
	}
}
