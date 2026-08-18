package log

import (
	"bytes"
	"log"
	"strings"
	"sync"
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

// TestSetLevelDefaultOutputsAll 验证默认级别（LevelInfo）时全部输出。
func TestSetLevelDefaultOutputsAll(t *testing.T) {
	SetLevel(LevelInfo)
	out := captureOutput(t, func() {
		Infof("info msg")
		Warnf("warn msg")
		Errorf("error msg")
	})
	if !strings.Contains(out, "[INFO] info msg") {
		t.Errorf("默认级别应输出 Info: %q", out)
	}
	if !strings.Contains(out, "[WARN] warn msg") {
		t.Errorf("默认级别应输出 Warn: %q", out)
	}
	if !strings.Contains(out, "[ERROR] error msg") {
		t.Errorf("默认级别应输出 Error: %q", out)
	}
}

// TestSetLevelWarnFiltersDebugInfo 验证 SetLevel(LevelWarn) 后 Debug/Info 被过滤。
func TestSetLevelWarnFiltersDebugInfo(t *testing.T) {
	SetLevel(LevelWarn)
	defer SetLevel(LevelInfo) // 恢复默认，避免影响后续测试
	out := captureOutput(t, func() {
		Debugf("debug msg")
		Infof("info msg")
		Warnf("warn msg")
		Errorf("error msg")
	})
	if strings.Contains(out, "[DEBUG]") {
		t.Errorf("LevelWarn 应过滤 Debug: %q", out)
	}
	if strings.Contains(out, "[INFO]") {
		t.Errorf("LevelWarn 应过滤 Info: %q", out)
	}
	if !strings.Contains(out, "[WARN] warn msg") {
		t.Errorf("LevelWarn 应输出 Warn: %q", out)
	}
	if !strings.Contains(out, "[ERROR] error msg") {
		t.Errorf("LevelWarn 应输出 Error: %q", out)
	}
	if got := GetLevel(); got != LevelWarn {
		t.Errorf("GetLevel() = %v, 期望 %v", got, LevelWarn)
	}
}

// TestSetLevelErrorFiltersWarn 验证 SetLevel(LevelError) 后 Warn 被过滤。
func TestSetLevelErrorFiltersWarn(t *testing.T) {
	SetLevel(LevelError)
	defer SetLevel(LevelInfo)
	out := captureOutput(t, func() {
		Infof("info msg")
		Warnf("warn msg")
		Errorf("error msg")
	})
	if strings.Contains(out, "[INFO]") {
		t.Errorf("LevelError 应过滤 Info: %q", out)
	}
	if strings.Contains(out, "[WARN]") {
		t.Errorf("LevelError 应过滤 Warn: %q", out)
	}
	if !strings.Contains(out, "[ERROR] error msg") {
		t.Errorf("LevelError 应输出 Error: %q", out)
	}
}

// TestSetLevelDebugOutputsAll 验证 LevelDebug 时全量输出（含 Debug）。
func TestSetLevelDebugOutputsAll(t *testing.T) {
	SetLevel(LevelDebug)
	defer SetLevel(LevelInfo)
	out := captureOutput(t, func() {
		Debugf("debug msg")
		Infof("info msg")
		Errorf("error msg")
	})
	if !strings.Contains(out, "[DEBUG] debug msg") {
		t.Errorf("LevelDebug 应输出 Debug: %q", out)
	}
	if !strings.Contains(out, "[INFO] info msg") {
		t.Errorf("LevelDebug 应输出 Info: %q", out)
	}
	if !strings.Contains(out, "[ERROR] error msg") {
		t.Errorf("LevelDebug 应输出 Error: %q", out)
	}
}

// TestSetLevelConcurrent 验证并发 SetLevel + 输出无竞态（-race）。
func TestSetLevelConcurrent(t *testing.T) {
	SetLevel(LevelInfo)
	defer SetLevel(LevelInfo)

	var wg sync.WaitGroup
	// 多个 goroutine 并发切换级别
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				SetLevel(LevelWarn)
				SetLevel(LevelInfo)
			}
		}()
	}
	// 多个 goroutine 并发输出
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				Infof("concurrent %d", j)
				Warnf("concurrent %d", j)
				Errorf("concurrent %d", j)
			}
		}()
	}
	wg.Wait()
}
