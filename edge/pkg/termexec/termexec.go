// 边缘受控命令执行器（v0.49.0，spec 0022，G31 远程运维）。
//
// 职责：接收云端 TermExec，白名单裁决后 exec.Command 直调（不走 shell），
// 合并输出截断回传 TermResult，逐命令本地审计 JSONL（防篡改副账，v0.49.0
// 不上云）。
//
// 安全边界（spec 0022 裁决 2/3/8）：
//   - opt-in：EDGEFLOW_EDGED_TERM=on 默认关（未启用时所有命令 rejected）；
//   - 白名单：EDGEFLOW_EDGED_TERM_ALLOWLIST 逗号分隔可执行文件名，精确匹配
//     大小写敏感，默认 uname,date,uptime,df,free,ps（全只读运维命令）；
//   - 不走 shell：exec.CommandContext(cmd, args...) 参数独立传递；
//   - 超时终止：context deadline 到点 kill 进程，OK=false + timeout 原因；
//   - 输出上限 64KB（保留头部）+ UTF-8 净化（v0.48 P2-1 同款——非法字节
//     经 JSON 序列化膨胀会触发云端 Validate 整体拒收）。
package termexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// 执行边界。
const (
	// DefaultAllowlist 是默认白名单（复核 P2-2：date 经 args 可改时钟（-s）/
	// 回读文件（-f/-r），「全只读」裁决不成立，剔除；uptime/uname/df/free/ps
	// 确认无状态副作用。参数语义治理（黑名单）留后续版本。
	DefaultAllowlist = "uname,uptime,df,free,ps"
	// MaxOutputBytes 是回传输出截断上限。
	MaxOutputBytes = 64 * 1024
	// MaxConcurrent 是并发执行上限（防命令风暴拖垮节点）。
	MaxConcurrent = 4
	// DefaultTimeoutSec 是未指定超时时的默认值。
	DefaultTimeoutSec = 30
)

// AuditEntry 是边侧执行审计（本地 JSONL 一行）。
type AuditEntry struct {
	Ts         int64  `json:"ts"`
	ExecID     string `json:"execId"`
	SessionID  string `json:"sessionId"`
	NodeID     string `json:"nodeId,omitempty"`
	Operator   string `json:"operator,omitempty"`
	Cmd        string `json:"cmd"`
	Args       string `json:"args,omitempty"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exitCode,omitempty"`
	Rejected   bool   `json:"rejected,omitempty"`
	Reason     string `json:"reason,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

// Executor 是边缘受控执行器。
type Executor struct {
	enabled   bool
	allow     map[string]struct{}
	auditPath string

	auditMu sync.Mutex
	auditF  *os.File

	sem chan struct{} // 并发闸

	stopCh   chan struct{}
	stopOnce sync.Once
}

// New 创建执行器。allowlist 为空串时用默认集；"off"（复核 P3-1 兑现注释）
// 视为空白名单（拒绝一切）。
func New(enabled bool, allowlist, auditPath string) *Executor {
	allow := map[string]struct{}{}
	if strings.TrimSpace(allowlist) == "" {
		allowlist = DefaultAllowlist
	}
	if strings.ToLower(strings.TrimSpace(allowlist)) != "off" {
		for _, name := range strings.Split(allowlist, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				allow[name] = struct{}{}
			}
		}
	}
	e := &Executor{
		enabled:   enabled,
		allow:     allow,
		auditPath: auditPath,
		sem:       make(chan struct{}, MaxConcurrent),
		stopCh:    make(chan struct{}),
	}
	if enabled && auditPath != "" {
		if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err == nil {
			if f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				e.auditF = f
			} else {
				log.Warnf("termexec: 本地审计文件打开失败（%s）: %v", auditPath, err)
			}
		} else {
			log.Warnf("termexec: 本体审计目录创建失败（%s）: %v", auditPath, err)
		}
	}
	return e
}

// Enabled 报告执行器是否启用。
func (e *Executor) Enabled() bool { return e.enabled }

// Stop 释放审计文件句柄（幂等）。
func (e *Executor) Stop() {
	e.stopOnce.Do(func() { close(e.stopCh) })
	if e.auditF != nil {
		_ = e.auditF.Close()
		e.auditF = nil
	}
}

func (e *Executor) audit(entry AuditEntry) {
	e.auditMu.Lock()
	defer e.auditMu.Unlock()
	if e.auditF == nil {
		return
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = e.auditF.Write(append(b, '\n'))
}

// sanitizeOutput 输出净化：UTF-8 非法字节替换为 '?' 后按 UTF-8 边界截断到
// 上限（v0.48 P2-1 教训：截断后悬空字节经 JSON 往返膨胀 U+FFFD）。
func sanitizeOutput(s string) (string, bool) {
	s = strings.ToValidUTF8(s, "?")
	truncated := false
	if len(s) > MaxOutputBytes {
		cut := MaxOutputBytes
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		s = s[:cut]
		truncated = true
	}
	return s, truncated
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 } // 非 continuation 字节

// Handle 处理一条 TermExec（cloudhub 回调注入；调用方须在独立 goroutine
// 调用——命令耗时会长于读循环容忍）。返回应回传的 TermResult 负载。
func (e *Executor) Handle(nodeID string, p protocol.TermExecPayload) *protocol.TermResultPayload {
	res := &protocol.TermResultPayload{
		ExecID:    p.ExecID,
		SessionID: p.SessionID,
		Ts:        time.Now().UnixMilli(),
	}
	fail := func(reason string) *protocol.TermResultPayload {
		res.Rejected = true
		res.Reason = reason
		e.audit(AuditEntry{
			Ts: res.Ts, ExecID: p.ExecID, SessionID: p.SessionID, NodeID: nodeID,
			Operator: p.Operator, Cmd: p.Cmd, Args: strings.Join(p.Args, " "),
			Rejected: true, Reason: reason,
		})
		return res
	}

	if !e.enabled {
		return fail("终端执行未启用（EDGEFLOW_EDGED_TERM=on 开启）")
	}
	if err := p.Validate(); err != nil {
		return fail("参数非法: " + err.Error())
	}
	if _, ok := e.allow[p.Cmd]; !ok {
		return fail(fmt.Sprintf("命令 %q 不在白名单", p.Cmd))
	}
	// 白名单精确匹配可执行文件名；用固定目录列表（minimalEnv 同款 PATH）
	// 显式解析出绝对路径并直呼 exec.Command（复核 P2-1：消除「进程环境 PATH
	// 解析 + CommandContext 二次解析」的 TOCTOU 与劫持面）。
	cmdPath, err := resolveInFixedPath(p.Cmd)
	if err != nil {
		return fail(fmt.Sprintf("命令 %q 不可用: %v", p.Cmd, err))
	}

	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-time.After(5 * time.Second):
		return fail("执行并发达上限（排队超时）")
	}

	timeout := time.Duration(p.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = DefaultTimeoutSec * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cmdPath, p.Args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Env = minimalEnv()

	start := time.Now()
	runErr := cmd.Run()
	res.DurationMs = time.Since(start).Milliseconds()
	res.Ts = time.Now().UnixMilli()

	if ctx.Err() == context.DeadlineExceeded {
		res.OK = false
		res.Reason = fmt.Sprintf("timeout（%ds 已终止）", p.TimeoutSec)
	} else if runErr != nil {
		res.OK = true // 命令已执行；非零退出/启动失败经 ExitCode/Reason 表达
		if ee, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Reason = runErr.Error()
		}
	} else {
		res.OK = true
		res.ExitCode = 0
	}
	out, trunc := sanitizeOutput(buf.String())
	res.Output = out
	res.Truncated = trunc

	e.audit(AuditEntry{
		Ts: res.Ts, ExecID: p.ExecID, SessionID: p.SessionID, NodeID: nodeID,
		Operator: p.Operator, Cmd: p.Cmd, Args: strings.Join(p.Args, " "),
		OK: res.OK, ExitCode: res.ExitCode, Rejected: false,
		Reason: res.Reason, DurationMs: res.DurationMs,
	})
	return res
}

// fixedPathDirs 是命令解析的固定目录（与 minimalEnv PATH 一致；唯一解析源）。
var fixedPathDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// resolveInFixedPath 在固定目录列表中解析命令绝对路径（复核 P2-1：
// 不依赖进程自身 PATH——部署方 PATH 含可写目录时同名文件无法劫持）。
func resolveInFixedPath(name string) (string, error) {
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("命令名不得含路径分隔符")
	}
	for _, dir := range fixedPathDirs {
		cand := filepath.Join(dir, name)
		if info, err := os.Stat(cand); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", fmt.Errorf("固定 PATH 内未找到 %q", name)
}

// minimalEnv 收敛执行环境（复核 v0.47 脚本最小 env 同款思路）：PATH 必需，
// TERM 兜底；不透传全量环境（云侧命令不由节点环境变量驱动行为）。
func minimalEnv() []string {
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	if term, ok := os.LookupEnv("TERM"); ok && term != "" {
		env = append(env, "TERM="+term)
	}
	return env
}
