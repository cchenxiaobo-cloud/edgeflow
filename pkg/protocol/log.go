// 日志同步负载（v0.48.0，spec 0021）：边→云 LogSync 消息的批量日志条目。
//
// 形态约定（与 alarm.Alarm 同构思路——一处定义、三处使用）：
//   - TypeLogSync 消息负载（边→云，批量）；
//   - 边缘 logcollector 缓冲条目（采集中间态）；
//   - 云端 logstore 存储行（检索 API 返回体元素）。
//
// 设计裁决（spec 0021 §3）：
//   - ts 取采集时刻（文件行内时间不解析——文件行无可靠时间源）；
//   - message 单条 ≤4KB（边侧截断保证，云侧校验拒绝超限）；
//   - level 白名单 debug|info|warn|error（云侧校验）。
package protocol

import (
	"errors"
	"strconv"
	"strings"
)

// TypeLogSync 是边→云的批量日志同步消息（v0.48.0）。
const TypeLogSync = "LogSync"

// 日志条目常量。
const (
	// LogLevelDebug/Info/Warn/Error 是日志级别白名单（云侧校验）。
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"

	// LogMaxBatchEntries 是单个 LogSync 批的最大条数（云侧校验拒绝超限）。
	LogMaxBatchEntries = 200
	// LogMaxSourceLen 是 source 字段最大长度。
	LogMaxSourceLen = 64
	// LogMaxMessageBytes 是单条 message 最大字节数（UTF-8）。
	LogMaxMessageBytes = 4 * 1024
)

// LogEntry 是一条日志（边→云同步 + 云端存储 + 检索返回的统一形态）。
type LogEntry struct {
	Ts      int64  `json:"ts"`      // 采集时刻（毫秒）
	Source  string `json:"source"`  // 来源标识：文件名（无 .log 后缀）或事件源名（如 scriptrunner）
	Level   string `json:"level"`   // 级别：debug|info|warn|error
	Message string `json:"message"` // 日志正文（单行，边侧已截断 4KB）
}

// LogBatch 是一条 LogSync 消息的负载（批量日志条目）。
type LogBatch struct {
	Entries []LogEntry `json:"entries"`
}

// Validate 校验批量日志负载：条数 1..200、source/level 非空且 level 白名单、
// source ≤64 字符、message ≤4KB。
func (b LogBatch) Validate() error {
	if len(b.Entries) == 0 {
		return errors.New("entries 为空")
	}
	if len(b.Entries) > LogMaxBatchEntries {
		return errors.New("entries 超限（单批最多 200 条）")
	}
	for i, e := range b.Entries {
		if strings.TrimSpace(e.Source) == "" {
			return errors.New("entries[" + strconv.Itoa(i) + "].source 为空")
		}
		if len(e.Source) > LogMaxSourceLen {
			return errors.New("entries[" + strconv.Itoa(i) + ".source 超长（>64）")
		}
		switch e.Level {
		case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
		default:
			return errors.New("entries[" + strconv.Itoa(i) + ".level 非法: " + e.Level)
		}
		if len(e.Message) > LogMaxMessageBytes {
			return errors.New("entries[" + strconv.Itoa(i) + ".message 超限（>4KB）")
		}
	}
	return nil
}
