package termgw

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/protocol"
)

// newTestGateway 构造测试网关（审计落临时文件）。
func newTestGateway(t *testing.T, send SendFunc, ttl time.Duration) *Gateway {
	t.Helper()
	g, err := New(Options{
		AuditPath:  filepath.Join(t.TempDir(), "term-audit.jsonl"),
		SessionTTL: ttl,
	}, send)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(g.Stop)
	return g
}

// TestV0490SessionLifecycle 全生命周期：创建 → active → close → 幂等再关。
func TestV0490SessionLifecycle(t *testing.T) {
	g := newTestGateway(t, nil, time.Minute)
	s, err := g.CreateSession("n1", "例行巡检", "op-1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.State != SessionActive || s.NodeID != "n1" || s.Operator != "op-1" {
		t.Fatalf("会话字段不符: %+v", s)
	}
	if len(g.List("n1")) != 1 || len(g.List("")) != 1 || len(g.List("other")) != 0 {
		t.Fatalf("列表过滤不符: %+v", g.List(""))
	}
	if err := g.Close(s.ID, "op-2"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 幂等：再关成功且不重复审计（无返回值，不 panic 即路径通）。
	if err := g.Close(s.ID, "op-2"); err != nil {
		t.Fatalf("Close 幂等: %v", err)
	}
	// 关闭后 exec → ErrSessionState。
	if _, err := g.Exec(s.ID, "uname", nil, 5, "op-1"); !errors.Is(err, ErrSessionState) {
		t.Fatalf("关闭后 exec 应 ErrSessionState: %v", err)
	}
}

// TestV0490SessionLimits 上限约束：per-node 3 / global 4（测试用小上限）。
func TestV0490SessionLimits(t *testing.T) {
	g := newTestGateway(t, nil, time.Minute)
	g.maxPer, g.maxGlb = 3, 4
	for i := 0; i < 3; i++ {
		if _, err := g.CreateSession("n1", "reason", "op"); err != nil {
			t.Fatalf("第 %d 个会话不应受限: %v", i+1, err)
		}
	}
	if _, err := g.CreateSession("n1", "reason", "op"); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("per-node 上限应触发: %v", err)
	}
	if _, err := g.CreateSession("n2", "reason", "op"); err != nil {
		t.Fatalf("n2 第一个会话不应受限: %v", err)
	}
	// global=4：已建 4 个 → 全局受限。
	if _, err := g.CreateSession("n3", "reason", "op"); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("global 上限应触发: %v", err)
	}
	// 关闭一个后配额释放。
	sessions := g.List("n1")
	if err := g.Close(sessions[0].ID, "op"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := g.CreateSession("n1", "reason", "op"); err != nil {
		t.Fatalf("释放后应可再建: %v", err)
	}
}

// TestV0490ExecRoundtrip exec 全链：SendToNode 捕获 TermExec → 注入 TermResult →
// Exec 返回结果 + 审计含命令。
func TestV0490ExecRoundtrip(t *testing.T) {
	var sent *protocol.Message
	var mu sync.Mutex
	g := newTestGateway(t, func(nodeID string, m *protocol.Message) error {
		mu.Lock()
		sent = m
		mu.Unlock()
		return nil
	}, time.Minute)
	s, _ := g.CreateSession("n1", "巡检", "op-9")

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 模拟边缘回结果（归属一致：nodeID=n1）。
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		m := sent
		mu.Unlock()
		var req protocol.TermExecPayload
		if err := m.DecodePayload(&req); err != nil {
			t.Errorf("DecodePayload: %v", err)
			return
		}
		g.DeliverResult("n1", &protocol.TermResultPayload{
			ExecID: req.ExecID, SessionID: req.SessionID, OK: true, ExitCode: 0,
			Output: "Linux edge", DurationMs: 12, Ts: time.Now().UnixMilli(),
		})
	}()
	res, err := g.Exec(s.ID, "uname", []string{"-a"}, 5, "op-9")
	<-done
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.OK || res.Output != "Linux edge" || res.ExitCode != 0 {
		t.Fatalf("结果不符: %+v", res)
	}
	// 审计落盘含 cmd 与 operator。
	data, _ := os.ReadFile(g.AuditPath())
	if !strings.Contains(string(data), `"cmd":"uname"`) || !strings.Contains(string(data), `"operator":"op-9"`) {
		t.Fatalf("审计缺关键字段: %s", data)
	}
}

// TestV0490DeliverResultOwnership 归属校验：他人节点上报的结果被丢弃。
func TestV0490DeliverResultOwnership(t *testing.T) {
	g := newTestGateway(t, func(nodeID string, m *protocol.Message) error { return nil }, time.Minute)
	s, _ := g.CreateSession("n1", "巡检", "op")

	// exec 挂起等待（边缘不回）→ 伪造他人结果不应投递 → 等待超时。
	go func() {
		time.Sleep(50 * time.Millisecond)
		// 伪造：伪造节点 n2 上报归属 n1 会话的结果。
		g.DeliverResult("n2", &protocol.TermResultPayload{
			ExecID: "te-forged", SessionID: s.ID, OK: true, Ts: time.Now().UnixMilli(),
		})
	}()
	res, err := g.Exec(s.ID, "date", nil, 1, "op")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.OK || !res.Rejected || !strings.Contains(res.Reason, "超时") {
		t.Fatalf("伪造结果应被丢弃（等待超时）: %+v err=%v", res, err)
	}
}

// TestV0490NodeOffline exec 节点离线：send 返回错误 → ErrNodeOffline（不入队）。
func TestV0490NodeOffline(t *testing.T) {
	g := newTestGateway(t, func(nodeID string, m *protocol.Message) error {
		return ErrNodeOffline
	}, time.Minute)
	s, _ := g.CreateSession("n1", "巡检", "op")
	if _, err := g.Exec(s.ID, "df", nil, 5, "op"); !errors.Is(err, ErrNodeOffline) {
		t.Fatalf("离线应 ErrNodeOffline: %v", err)
	}
	// 审计记失败。
	data, _ := os.ReadFile(g.AuditPath())
	if !strings.Contains(string(data), `"rejected":true`) {
		t.Fatalf("离线审计缺 rejected: %s", data)
	}
}

// TestV0490IdleExpiry 空闲过期：TTL 内无活动 → expire 置 expired + 配额释放。
func TestV0490IdleExpiry(t *testing.T) {
	g := newTestGateway(t, nil, 50*time.Millisecond)
	s, _ := g.CreateSession("n1", "巡检", "op")
	time.Sleep(120 * time.Millisecond)
	g.expire()
	g.mu.RLock()
	state := g.sessions[s.ID].State
	g.mu.RUnlock()
	if state != SessionExpired {
		t.Fatalf("应 expired: %s", state)
	}
	// expired 后 exec → ErrSessionState。
	if _, err := g.Exec(s.ID, "ps", nil, 5, "op"); !errors.Is(err, ErrSessionState) {
		t.Fatalf("过期后 exec 应 ErrSessionState: %v", err)
	}
	// 配额已释放：per-node 可再建。
	if _, err := g.CreateSession("n1", "再巡检", "op"); err != nil {
		t.Fatalf("过期释放配额后应可再建: %v", err)
	}
}

// TestV0490CreateValidation 创建校验：空 reason / 超长 reason 拒绝。
func TestV0490CreateValidation(t *testing.T) {
	g := newTestGateway(t, nil, time.Minute)
	if _, err := g.CreateSession("n1", "", "op"); err == nil {
		t.Fatal("空 reason 应拒绝")
	}
	if _, err := g.CreateSession("", "r", "op"); err == nil {
		t.Fatal("空 nodeID 应拒绝")
	}
	if _, err := g.CreateSession("n1", strings.Repeat("r", 257), "op"); err == nil {
		t.Fatal("超长 reason 应拒绝")
	}
}

// TestV0490ReadAuditPaging 审计分页：倒序 + offset + X-Total-Count 语义（total 不受分页影响）。
func TestV0490ReadAuditPaging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	var sb strings.Builder
	for i := 0; i < 25; i++ {
		sb.WriteString(`{"ts":1000,"action":"exec","nodeId":"n1","cmd":"cmd` + itoa(i) + `"}` + "\n")
	}
	sb.WriteString(`{"ts":1001,"action":"exec","nodeId":"n2","cmd":"other"}` + "\n")
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	page, total, err := ReadAudit(path, "n1", 10, 0)
	if err != nil || total != 25 || len(page) != 10 {
		t.Fatalf("首页: total=%d len=%d err=%v", total, len(page), err)
	}
	if page[0].Cmd != "cmd24" { // 最新在前
		t.Fatalf("倒序不符: %s", page[0].Cmd)
	}
	p2, total2, _ := ReadAudit(path, "n1", 10, 20)
	if total2 != 25 || len(p2) != 5 || p2[0].Cmd != "cmd4" {
		t.Fatalf("第二页: total=%d len=%d first=%s", total2, len(p2), p2[0].Cmd)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
