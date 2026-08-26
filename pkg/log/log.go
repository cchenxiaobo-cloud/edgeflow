// Package log 提供基于标准库 log 的轻量日志封装。
//
// 支持 Debug / Info / Warn / Error 四个级别，输出到标准错误（stderr），
// 自动带时间戳，例如：
//
//	2026/08/13 09:00:00 [INFO] cloudcore starting, version=v0.1.0
//
// 级别过滤（P3）：SetLevel 设置全局最低输出级别（默认 LevelInfo，
// 即 Debug 被过滤），低于该级别的日志不再输出；并发安全（atomic）。
//
// 后续如需结构化日志、日志轮转等能力，可在此包内扩展或替换实现。
package log

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync/atomic"
)

// Level 表示日志级别。
type Level int32

const (
	// LevelDebug 调试级别（最低，开发调试用）。
	LevelDebug Level = iota
	// LevelInfo 信息级别。
	LevelInfo
	// LevelWarn 警告级别。
	LevelWarn
	// LevelError 错误级别。
	LevelError
)

// levelNames 是日志级别对应的输出前缀。
var levelNames = map[Level]string{
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
}

// globalLevel 是全局最低输出级别（并发安全）。默认 LevelInfo 输出全部。
var globalLevel atomic.Int32

func init() {
	globalLevel.Store(int32(LevelInfo))
}

// SetLevel 设置全局最低输出级别（并发安全）。低于该级别的日志不再输出。
// 默认 LevelInfo：Debug 被过滤、Info/Warn/Error 全量输出。
// SetLevel(LevelWarn) 后 Debug/Info 被过滤，仅 Warn/Error 输出。
func SetLevel(level Level) {
	globalLevel.Store(int32(level))
}

// GetLevel 返回当前全局最低输出级别。
func GetLevel() Level {
	return Level(globalLevel.Load())
}

// logger 是内部使用的标准库 logger，带日期时间前缀。
// 测试中会临时替换为内存 buffer，因此不导出。
var logger = log.New(os.Stderr, "", log.LstdFlags)

// SetOutput 设置日志输出目标（默认 stderr）。测试可注入 bytes.Buffer
// 捕获输出断言（如周期配置启动日志）；传 nil 恢复默认 stderr。
// 与 SetLevel 同为全局状态：测试注入后应 defer 恢复。
func SetOutput(w io.Writer) {
	if w == nil {
		w = os.Stderr
	}
	logger.SetOutput(w)
}

// Output 返回当前日志输出目标（v0.11.0，L20b+：Windows 测试注入恢复用；
// 与 SetOutput 对称）。
func Output() io.Writer {
	return logger.Writer()
}

// Debugf 打印一条 Debug 级别日志，用法与 fmt.Printf 相同。
func Debugf(format string, args ...any) {
	logf(LevelDebug, format, args...)
}

// Infof 打印一条 Info 级别日志，用法与 fmt.Printf 相同。
func Infof(format string, args ...any) {
	logf(LevelInfo, format, args...)
}

// Warnf 打印一条 Warn 级别日志，用法与 fmt.Printf 相同。
func Warnf(format string, args ...any) {
	logf(LevelWarn, format, args...)
}

// Errorf 打印一条 Error 级别日志，用法与 fmt.Printf 相同。
func Errorf(format string, args ...any) {
	logf(LevelError, format, args...)
}

// logf 是统一输出入口：级别过滤后拼接前缀交给标准库 logger。
func logf(level Level, format string, args ...any) {
	if level < Level(globalLevel.Load()) {
		return // 低于全局最低级别，直接丢弃
	}
	name, ok := levelNames[level]
	if !ok {
		name = "INFO" // 未知级别兜底为 INFO，避免输出异常
	}
	msg := fmt.Sprintf(format, args...)
	logger.Printf("[%s] %s", name, msg)
}
