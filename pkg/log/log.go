// Package log 提供基于标准库 log 的轻量日志封装。
//
// 支持 Info / Warn / Error 三个级别，输出到标准错误（stderr），
// 自动带时间戳，例如：
//
//	2026/08/13 09:00:00 [INFO] cloudcore starting, version=v0.1.0
//
// 后续如需结构化日志、日志轮转等能力，可在此包内扩展或替换实现。
package log

import (
	"fmt"
	"log"
	"os"
)

// Level 表示日志级别。
type Level int

const (
	// LevelInfo 信息级别。
	LevelInfo Level = iota
	// LevelWarn 警告级别。
	LevelWarn
	// LevelError 错误级别。
	LevelError
)

// levelNames 是日志级别对应的输出前缀。
var levelNames = map[Level]string{
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
}

// logger 是内部使用的标准库 logger，带日期时间前缀。
// 测试中会临时替换为内存 buffer，因此不导出。
var logger = log.New(os.Stderr, "", log.LstdFlags)

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

// logf 是统一输出入口：拼接级别前缀后交给标准库 logger。
func logf(level Level, format string, args ...any) {
	name, ok := levelNames[level]
	if !ok {
		name = "INFO" // 未知级别兜底为 INFO，避免输出异常
	}
	msg := fmt.Sprintf(format, args...)
	logger.Printf("[%s] %s", name, msg)
}
