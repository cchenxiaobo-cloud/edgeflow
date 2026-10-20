// 远程运维终端消息（v0.49.0，spec 0022，G31+G11）。
//
// 只增不改：新增 TermExec（云→边，受控命令执行请求）与 TermResult（边→云，
// 执行结果回传）两个消息类型；不启用 NodeJob/NodeJobResult 占位（其定义为
// 「v0.1.0 范围外保留占位」，启用即改语义）。旧版本对未知类型走 default 忽略，
// 双向兼容。
//
// 形态：受控命令会话（非 PTY）。云端逐条下发白名单命令，边缘受控执行器
// 校验白名单后 exec.Command 直调（不走 shell），合并输出截断 64KB 回传。
package protocol

import (
	"fmt"
	"strings"
)

// 终端消息类型（v0.49.0）。
const (
	TypeTermExec   = "TermExec"   // 云→边：受控命令执行请求
	TypeTermResult = "TermResult" // 边→云：执行结果回传
)

// 终端执行边界（协议层硬上限；边侧执行器另有业务约束）。
const (
	// TermMaxArgs 是单条命令最大参数个数。
	TermMaxArgs = 16
	// TermMaxArgBytes 是单个参数最大字节数。
	TermMaxArgBytes = 4096
	// TermMaxArgsBytes 是参数总长上限。
	TermMaxArgsBytes = 16 * 1024
	// TermMaxTimeoutSec 是命令超时上限（秒）。
	TermMaxTimeoutSec = 120
	// TermMaxOutputBytes 是回传输出上限（字节；边侧截断后 ≤ 此值）。
	TermMaxOutputBytes = 96 * 1024
)

// TermExecPayload 是 TermExec 消息负载（云→边）。
//
// Operator 为发起运维操作的云端凭证 ID（端到端留痕：边侧本地审计亦记录）；
// CMD 为可执行文件名（白名单精确匹配），Args 独立传递（不经 shell 解析）。
type TermExecPayload struct {
	ExecID     string   `json:"execId"`     // 云侧生成，结果关联键
	SessionID  string   `json:"sessionId"`  // 所属会话（审计关联）
	Cmd        string   `json:"cmd"`        // 可执行文件名（白名单匹配键）
	Args       []string `json:"args"`       // 参数（独立传递，不解析）
	TimeoutSec int      `json:"timeoutSec"` // 1..120
	Operator   string   `json:"operator"`   // 云端凭证 ID（端到端留痕）
}

// Validate 校验 TermExec 负载（协议层结构边界；白名单属边侧执行策略）。
func (p TermExecPayload) Validate() error {
	if strings.TrimSpace(p.ExecID) == "" || len(p.ExecID) > 64 {
		return fmt.Errorf("execId 非法（1..64 字节）")
	}
	if strings.TrimSpace(p.SessionID) == "" || len(p.SessionID) > 64 {
		return fmt.Errorf("sessionId 非法（1..64 字节）")
	}
	if strings.TrimSpace(p.Cmd) == "" || len(p.Cmd) > 64 {
		return fmt.Errorf("cmd 非法（1..64 字节）")
	}
	if strings.ContainsAny(p.Cmd, "/\\\x00") {
		return fmt.Errorf("cmd 不得含路径分隔符或 NUL")
	}
	if len(p.Args) > TermMaxArgs {
		return fmt.Errorf("args 超限（>%d 个）", TermMaxArgs)
	}
	total := 0
	for i, a := range p.Args {
		if strings.ContainsRune(a, '\x00') {
			return fmt.Errorf("args[%d] 含 NUL", i)
		}
		if len(a) > TermMaxArgBytes {
			return fmt.Errorf("args[%d] 超限（>%d 字节）", i, TermMaxArgBytes)
		}
		total += len(a)
	}
	if total > TermMaxArgsBytes {
		return fmt.Errorf("args 总长超限（>%d 字节）", TermMaxArgsBytes)
	}
	if p.TimeoutSec < 1 || p.TimeoutSec > TermMaxTimeoutSec {
		return fmt.Errorf("timeoutSec 非法（1..%d）", TermMaxTimeoutSec)
	}
	if len(p.Operator) > 64 {
		return fmt.Errorf("operator 超限（>64 字节）")
	}
	return nil
}

// TermResultPayload 是 TermResult 消息负载（边→云）。
//
// 语义：
//   - Rejected=true：边侧拒绝执行（未启用/白名单外/参数非法），Output 为空，
//     Reason 说明；ExitCode 无意义。
//   - OK=true：命令已执行；ExitCode 为进程退出码；超时被终止时 OK=false 且
//     Reason 含 "timeout"。
//   - Output 为合并 stdout+stderr（头部保留），Truncated 标记截断。
type TermResultPayload struct {
	ExecID     string `json:"execId"`              // 关联键（回填 TermExec.ExecID）
	SessionID  string `json:"sessionId"`           // 所属会话
	OK         bool   `json:"ok"`                  // 命令已执行（含非零退出码）
	ExitCode   int    `json:"exitCode,omitempty"`  // 进程退出码（OK=true 时有意义）
	Output     string `json:"output,omitempty"`    // 合并输出（≤64KB，UTF-8 安全）
	Truncated  bool   `json:"truncated,omitempty"` // Output 是否被截断
	Rejected   bool   `json:"rejected,omitempty"`  // 边侧拒绝执行
	Reason     string `json:"reason,omitempty"`    // 拒绝/失败原因
	DurationMs int64  `json:"durationMs,omitempty"`
	Ts         int64  `json:"ts"` // 边侧完成时间（毫秒）
}

// Validate 校验 TermResult 负载。
func (p TermResultPayload) Validate() error {
	if strings.TrimSpace(p.ExecID) == "" || len(p.ExecID) > 64 {
		return fmt.Errorf("execId 非法（1..64 字节）")
	}
	if strings.TrimSpace(p.SessionID) == "" || len(p.SessionID) > 64 {
		return fmt.Errorf("sessionId 非法（1..64 字节）")
	}
	if len(p.Output) > TermMaxOutputBytes {
		return fmt.Errorf("output 超限（>%d 字节）", TermMaxOutputBytes)
	}
	if len(p.Reason) > 512 {
		return fmt.Errorf("reason 超限（>512 字节）")
	}
	return nil
}

// TermValidateTestHooks 说明：TermExecPayload.Validate / TermResultPayload.Validate
// 由 protocol 包单测与契约测试直接覆盖（合法性/边界/拒绝路径），无需额外 hook。
