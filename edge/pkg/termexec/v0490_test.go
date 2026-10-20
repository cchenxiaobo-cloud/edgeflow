package termexec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/protocol"
)

func testPayload(execID, cmd string, args ...string) protocol.TermExecPayload {
	return protocol.TermExecPayload{
		ExecID: execID, SessionID: "ts-test", Cmd: cmd, Args: args,
		TimeoutSec: 10, Operator: "op-1",
	}
}

// TestV0490DisabledRejects 未启用时一律拒绝（默认关零行为）。
func TestV0490DisabledRejects(t *testing.T) {
	e := New(false, "", "")
	defer e.Stop()
	res := e.Handle("n1", testPayload("te-1", "uname"))
	if !res.Rejected || res.OK || !strings.Contains(res.Reason, "未启用") {
		t.Fatalf("未启用应拒绝: %+v", res)
	}
}

// TestV0490Allowlist 白名单裁决：默认集内放行、集外拒绝、env 覆盖生效。
func TestV0490Allowlist(t *testing.T) {
	dir := t.TempDir()
	e := New(true, "", filepath.Join(dir, "audit.jsonl"))
	defer e.Stop()
	if res := e.Handle("n1", testPayload("te-1", "uname", "-a")); !res.OK || res.Rejected {
		t.Fatalf("默认白名单内 uname 应执行: %+v", res)
	}
	if res := e.Handle("n1", testPayload("te-2", "shutdown")); !res.Rejected || strings.Contains(res.Reason, "未启用") {
		t.Fatalf("白名单外 shutdown 应拒绝: %+v", res)
	}
	// 路径分隔符在协议层拒绝。
	if res := e.Handle("n1", testPayload("te-3", "/bin/sh")); !res.Rejected {
		t.Fatalf("含路径分隔符应拒绝: %+v", res)
	}
	// env 覆盖：只允许 echo。
	e2 := New(true, "echo", filepath.Join(dir, "a2.jsonl"))
	defer e2.Stop()
	if res := e2.Handle("n1", testPayload("te-4", "echo", "hello")); !res.OK || res.Output != "hello\n" {
		t.Fatalf("echo 应执行: %+v", res)
	}
	if res := e2.Handle("n1", testPayload("te-5", "uname")); !res.Rejected {
		t.Fatalf("覆盖后 uname 应拒绝: %+v", res)
	}
}

// TestV0490ExitCodeTimeout 退出码与超时：非零退出码保留；超时终止标记 timeout。
func TestV0490ExitCodeTimeout(t *testing.T) {
	dir := t.TempDir()
	e := New(true, "false,sleep", filepath.Join(dir, "a3.jsonl"))
	defer e.Stop()
	if res := e.Handle("n1", testPayload("te-1", "false")); !res.OK || res.ExitCode != 1 {
		t.Fatalf("false 应执行且 exit=1: %+v", res)
	}
	p := testPayload("te-2", "sleep", "30")
	p.TimeoutSec = 1
	start := time.Now()
	res := e.Handle("n1", p)
	if res.OK || !strings.Contains(res.Reason, "timeout") || time.Since(start) > 5*time.Second {
		t.Fatalf("超时应终止: %+v（耗时 %s）", res, time.Since(start))
	}
}

// TestV0490OutputTruncate 输出截断：sanitizeOutput 单元断言（>64KB 保留头部 +
// truncated 标记；协议层单参 4KB 上限使 80KB 单参数不可达——那是 Validate 的
// 正确拒绝路径，见 TestV0490Allowlist 与协议单测）。
func TestV0490OutputTruncate(t *testing.T) {
	long := strings.Repeat("x", 80*1024)
	out, trunc := sanitizeOutput(long)
	if !trunc || len(out) != MaxOutputBytes || !strings.HasPrefix(out, "xxx") {
		t.Fatalf("截断不符: truncated=%v len=%d", trunc, len(out))
	}
	// UTF-8 边界：多字节字符中间截断不产生悬空起始字节。
	multi := strings.Repeat("中", 30000) // 90KB UTF-8（每字 3B）
	out2, trunc2 := sanitizeOutput(multi)
	// 回退语义：截断点退到字符起始，[0,cut) 以完整"中"结束（cut=65535，
	// 比 65536 少 1B——后者会把一个字切成两半）；断言整字对齐而非固定长度。
	if !trunc2 || len(out2) > MaxOutputBytes || len(out2)%3 != 0 {
		t.Fatalf("多字节截断不符: truncated=%v len=%d", trunc2, len(out2))
	}
	// JSON 往返不膨胀（v0.48 P2-1 同款回归）。
	b, err := json.Marshal(protocol.TermResultPayload{Output: out2, Truncated: trunc2})
	if err != nil || len(b) > 100*1024 {
		t.Fatalf("JSON 往返异常膨胀: err=%v len=%d", err, len(b))
	}
	// 近界集成：4×(4KB-1) 参数（协议单参/总量上限内）echo 输出 ≈16KB 完整回传。
	dir := t.TempDir()
	e := New(true, "echo", filepath.Join(dir, "a4.jsonl"))
	defer e.Stop()
	seg := strings.Repeat("y", 4*1024-1)
	res := e.Handle("n1", testPayload("te-1", "echo", seg, seg, seg, seg))
	if !res.OK || res.Truncated || len(res.Output) != 4*4*1024-4+3+1 {
		t.Fatalf("近界回传不符: reason=%s len=%d", res.Reason, len(res.Output))
	}
}

// TestV0490AuditTrail 逐命令本地审计：成功/拒绝均落盘（防篡改副账）。
func TestV0490AuditTrail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	e := New(true, "", path)
	e.Handle("n1", testPayload("te-1", "uname"))
	e.Handle("n1", testPayload("te-2", "reboot"))
	e.Stop()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("审计文件缺失: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应 2 条审计: %d", len(lines))
	}
	var e1, e2 AuditEntry
	if json.Unmarshal([]byte(lines[0]), &e1) != nil || json.Unmarshal([]byte(lines[1]), &e2) != nil {
		t.Fatalf("审计 JSON 解析失败")
	}
	if e1.ExecID != "te-1" || !e1.OK || e1.Rejected {
		t.Fatalf("成功审计不符: %+v", e1)
	}
	if e2.ExecID != "te-2" || !e2.Rejected || !strings.Contains(e2.Reason, "白名单") {
		t.Fatalf("拒绝审计不符: %+v", e2)
	}
}

// TestV0490ConcurrentCap 并发闸：MaxConcurrent 之外的命令排队（5s 超时拒绝）。
func TestV0490ConcurrentCap(t *testing.T) {
	dir := t.TempDir()
	e := New(true, "sleep", filepath.Join(dir, "a5.jsonl"))
	defer e.Stop()
	done := make(chan *protocol.TermResultPayload, MaxConcurrent+2)
	for i := 0; i < MaxConcurrent+2; i++ {
		p := testPayload("te-"+string(rune('a'+i)), "sleep", "8")
		p.TimeoutSec = 10
		go func(pp protocol.TermExecPayload) { done <- e.Handle("n1", pp) }(p)
	}
	okCount, rejCount := 0, 0
	for i := 0; i < MaxConcurrent+2; i++ {
		res := <-done
		if res.Rejected && strings.Contains(res.Reason, "并发") {
			rejCount++
		} else if res.OK {
			okCount++
		}
	}
	// 至少 MaxConcurrent 个执行成功；排队超时者被拒绝（不阻塞不死锁）。
	if okCount < MaxConcurrent || rejCount == 0 {
		t.Fatalf("并发闸异常: ok=%d rejected=%d", okCount, rejCount)
	}
}
