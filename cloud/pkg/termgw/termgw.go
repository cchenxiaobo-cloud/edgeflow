// 云端终端会话网关（v0.49.0，spec 0022，G31 远程运维）。
//
// 职责：会话生命周期（create/list/close + 空闲过期）+ 命令下发与结果关联
// （execID → pending chan 同步等待）+ 命令级详细审计（JSONL）。白名单裁决在
// 边侧执行器（单一裁决点），云端只做会话态与投递。
//
// 并发约定：
//   - sessions/pending 由 mu 保护；TermResult 回调（连接读 goroutine）仅
//     投递 chan，不做长活。
//   - TermResult 逐条校验上报节点与会话归属一致（v0.48 P2-6 同款加固：
//     防被攻陷连接伪造他人节点结果）。
package termgw

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// 会话状态。
const (
	SessionActive  = "active"
	SessionClosed  = "closed"
	SessionExpired = "expired"
)

// 业务错误（HTTP 层映射：ErrNodeOffline→503，ErrSessionNotFound→404，
// ErrSessionState→409，ErrSessionLimit→429）。
var (
	ErrNodeOffline     = errors.New("termgw: 节点离线")
	ErrSessionNotFound = errors.New("termgw: 会话不存在")
	ErrSessionState    = errors.New("termgw: 会话状态不允许该操作")
	ErrSessionLimit    = errors.New("termgw: 会话数达上限")
)

// Session 是一次受控运维会话（云端易失态，重启丢失——审计在文件）。
type Session struct {
	ID         string `json:"id"`
	NodeID     string `json:"nodeId"`
	Reason     string `json:"reason"`
	Operator   string `json:"operator"`
	CreatedAt  int64  `json:"createdAt"`
	LastActive int64  `json:"lastActiveAt"`
	State      string `json:"state"`
}

// TermAuditEntry 是命令级审计记录（云端 JSONL 一行）。
type TermAuditEntry struct {
	Ts         int64  `json:"ts"`     // 完成时间（毫秒）
	Action     string `json:"action"` // session-create | exec | session-close | session-expire
	SessionID  string `json:"sessionId,omitempty"`
	ExecID     string `json:"execId,omitempty"`
	NodeID     string `json:"nodeId"`
	Operator   string `json:"operator"`
	Cmd        string `json:"cmd,omitempty"`
	Args       string `json:"args,omitempty"` // 空格连接（审计展示用）
	OK         bool   `json:"ok,omitempty"`
	ExitCode   int    `json:"exitCode,omitempty"`
	Rejected   bool   `json:"rejected,omitempty"`
	Reason     string `json:"reason,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

// auditWriter 是命令级审计 JSONL 追加写（并发安全，写失败仅记日志不阻塞主流程）。
type auditWriter struct {
	mu   sync.Mutex
	file *os.File
	path string
}

func newAuditWriter(path string) (*auditWriter, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("termgw: 审计目录创建失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("termgw: 审计文件打开失败: %w", err)
	}
	return &auditWriter{file: f, path: path}, nil
}

func (w *auditWriter) record(e TermAuditEntry) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	b, err := json.Marshal(e)
	if err != nil {
		log.Errorf("termgw: 审计编码失败: %v", err)
		return
	}
	if _, err := w.file.Write(append(b, '\n')); err != nil {
		log.Errorf("termgw: 审计写入失败: %v", err)
	}
}

// ReadAudit 读取审计文件尾部（limit 条，nodeID 过滤）+ 全量匹配总数。
// 供 GET /api/v1/term/audit 使用；文件不存在视为空。
func ReadAudit(path, nodeID string, limit, offset int) ([]TermAuditEntry, int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []TermAuditEntry{}, 0, nil
		}
		return nil, 0, err
	}
	var all []TermAuditEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e TermAuditEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue // 损坏行跳过（同 logstore 回放口径）
		}
		if nodeID != "" && e.NodeID != nodeID {
			continue
		}
		all = append(all, e)
	}
	total := len(all)
	// 时间正序追加；查询按最新在前分页。
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if offset >= len(all) {
		return []TermAuditEntry{}, total, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], total, nil
}

// SendFunc 是云→边单播投递函数（装配时注入 cloudhub.Server.SendToNode）。
type SendFunc func(nodeID string, msg *protocol.Message) error

// Options 是网关可调参数（env 装配）。
type Options struct {
	AuditPath  string        // 命令级审计 JSONL（空 = 不落盘，仅内存事件）
	SessionTTL time.Duration // 空闲过期（默认 10min）
	MaxPerNode int           // 每节点并发会话上限（默认 3）
	MaxGlobal  int           // 全局并发会话上限（默认 32）
}

// Gateway 是云端终端会话网关。
type Gateway struct {
	mu       sync.RWMutex // sessions 读多写少：DeliverResult 归属校验走 RLock
	sessions map[string]*Session
	perNode  map[string]int

	active int // 活跃会话数（active 态才占配额；closed/expired 不占——历史留列表）

	pendMu  sync.Mutex
	pending map[string]chan *protocol.TermResultPayload // execID → 结果 chan

	audit  *auditWriter
	path   string
	send   SendFunc
	ttl    time.Duration
	maxPer int
	maxGlb int

	stopCh   chan struct{}
	stopOnce sync.Once
}

// New 创建网关。send 为空时 exec 一律返回节点离线（纯会话/审计面仍可用）。
func New(opt Options, send SendFunc) (*Gateway, error) {
	if opt.SessionTTL <= 0 {
		opt.SessionTTL = 10 * time.Minute
	}
	if opt.MaxPerNode <= 0 {
		opt.MaxPerNode = 3
	}
	if opt.MaxGlobal <= 0 {
		opt.MaxGlobal = 32
	}
	aw, err := newAuditWriter(opt.AuditPath)
	if err != nil {
		return nil, err
	}
	return &Gateway{
		sessions: map[string]*Session{},
		perNode:  map[string]int{},
		pending:  map[string]chan *protocol.TermResultPayload{},
		audit:    aw,
		path:     opt.AuditPath,
		send:     send,
		ttl:      opt.SessionTTL,
		maxPer:   opt.MaxPerNode,
		maxGlb:   opt.MaxGlobal,
		stopCh:   make(chan struct{}),
	}, nil
}

// Start 启动空闲过期扫描（1min 周期）。
func (g *Gateway) Start() {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-g.stopCh:
				return
			case <-t.C:
				g.expire()
			}
		}
	}()
}

// Stop 停止过期扫描（幂等）。
func (g *Gateway) Stop() {
	g.stopOnce.Do(func() { close(g.stopCh) })
}

// AuditPath 返回审计文件路径（查询端点用）。
func (g *Gateway) AuditPath() string { return g.path }

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func nowMs() int64 { return time.Now().UnixMilli() }

// CreateSession 创建会话（受 per-node/global 上限约束；reason 必填入审计）。
func (g *Gateway) CreateSession(nodeID, reason, operator string) (*Session, error) {
	if strings.TrimSpace(nodeID) == "" || len(nodeID) > 64 {
		return nil, fmt.Errorf("termgw: nodeID 非法")
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 256 {
		return nil, fmt.Errorf("termgw: reason 必填（1..256 字节，入审计）")
	}
	g.mu.Lock()
	if g.active >= g.maxGlb {
		g.mu.Unlock()
		return nil, ErrSessionLimit
	}
	if g.perNode[nodeID] >= g.maxPer {
		g.mu.Unlock()
		return nil, ErrSessionLimit
	}
	s := &Session{
		ID:         newID("ts-"),
		NodeID:     nodeID,
		Reason:     reason,
		Operator:   operator,
		CreatedAt:  nowMs(),
		LastActive: nowMs(),
		State:      SessionActive,
	}
	g.sessions[s.ID] = s
	g.perNode[nodeID]++
	g.active++
	g.mu.Unlock()
	g.audit.record(TermAuditEntry{
		Ts: nowMs(), Action: "session-create", SessionID: s.ID,
		NodeID: nodeID, Operator: operator,
	})
	return s, nil
}

// List 返回会话列表（nodeID 过滤，创建时间倒序）。
func (g *Gateway) List(nodeID string) []Session {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Session, 0, len(g.sessions))
	for _, s := range g.sessions {
		if nodeID == "" || s.NodeID == nodeID {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Close 关闭会话（幂等：closed/expired 再关返回成功不重复审计）。
func (g *Gateway) Close(sessionID, operator string) error {
	g.mu.Lock()
	s := g.sessions[sessionID]
	if s == nil {
		g.mu.Unlock()
		return ErrSessionNotFound
	}
	if s.State != SessionActive {
		g.mu.Unlock()
		return nil
	}
	s.State = SessionClosed
	g.perNode[s.NodeID]--
	g.active--
	if g.perNode[s.NodeID] <= 0 {
		delete(g.perNode, s.NodeID)
	}
	g.mu.Unlock()
	g.audit.record(TermAuditEntry{
		Ts: nowMs(), Action: "session-close", SessionID: sessionID,
		NodeID: s.NodeID, Operator: operator,
	})
	return nil
}

// Exec 在会话内执行一条白名单命令：下发 TermExec 并同步等待 TermResult。
// 等待窗口 = timeoutSec + 5s 缓冲；节点离线立即失败（不入队不补传——spec
// 裁决 10：运维命令时效性强）。
func (g *Gateway) Exec(sessionID, cmd string, args []string, timeoutSec int, operator string) (*protocol.TermResultPayload, error) {
	if timeoutSec <= 0 {
		timeoutSec = 10 // 复核 P2-3：同步面默认收敛 10s（默认 30 与 WriteTimeout 15s 冲突）
	}
	// 复核 P2-3：HTTP WriteTimeout(15s) 下同步等待窗（timeoutSec+5s）>15s
	// 时响应必丢——同步面硬钳 10s；协议层 TermMaxTimeoutSec=120 保留给
	// 后续异步化面（KI §50-1）。
	if timeoutSec > 10 {
		return &protocol.TermResultPayload{
			ExecID: newID("te-"), SessionID: sessionID, Rejected: true,
			Reason: "同步执行超时上限 10s（HTTP WriteTimeout 约束；更大超时待异步化）",
			Ts:     nowMs(),
		}, nil
	}
	g.mu.Lock()
	s := g.sessions[sessionID]
	if s == nil {
		g.mu.Unlock()
		return nil, ErrSessionNotFound
	}
	if s.State != SessionActive {
		g.mu.Unlock()
		return nil, ErrSessionState
	}
	nodeID := s.NodeID
	s.LastActive = nowMs()
	g.mu.Unlock()

	execID := newID("te-")
	req := protocol.TermExecPayload{
		ExecID:     execID,
		SessionID:  sessionID,
		Cmd:        cmd,
		Args:       args,
		TimeoutSec: timeoutSec,
		Operator:   operator,
	}
	if err := req.Validate(); err != nil {
		// 复核 P2-4：该分支同样落命令级审计（取证需 cmd/args/operator）。
		g.audit.record(TermAuditEntry{Ts: nowMs(), Action: "exec", SessionID: sessionID, ExecID: execID, NodeID: nodeID, Operator: operator, Cmd: cmd, Args: strings.Join(args, " "), Rejected: true, Reason: "参数非法: " + err.Error()})
		return &protocol.TermResultPayload{ExecID: execID, SessionID: sessionID, Rejected: true, Reason: "参数非法: " + err.Error(), Ts: nowMs()}, nil
	}

	ch := make(chan *protocol.TermResultPayload, 1)
	g.pendMu.Lock()
	g.pending[execID] = ch
	g.pendMu.Unlock()
	defer func() {
		g.pendMu.Lock()
		delete(g.pending, execID)
		g.pendMu.Unlock()
	}()

	if g.send == nil {
		g.audit.record(TermAuditEntry{Ts: nowMs(), Action: "exec", SessionID: sessionID, ExecID: execID, NodeID: nodeID, Operator: operator, Cmd: cmd, Args: strings.Join(args, " "), Rejected: true, Reason: "下行通道未配置"})
		return &protocol.TermResultPayload{ExecID: execID, SessionID: sessionID, Rejected: true, Reason: "下行通道未配置", Ts: nowMs()}, nil
	}
	msg, err := protocol.NewMessage(protocol.TypeTermExec, "cloud", nodeID, req)
	if err != nil {
		return nil, fmt.Errorf("termgw: 构造 TermExec 失败: %w", err)
	}
	if err := g.send(nodeID, msg); err != nil {
		g.audit.record(TermAuditEntry{Ts: nowMs(), Action: "exec", SessionID: sessionID, ExecID: execID, NodeID: nodeID, Operator: operator, Cmd: cmd, Args: strings.Join(args, " "), Rejected: true, Reason: err.Error()})
		if errors.Is(err, ErrNodeOffline) || strings.Contains(err.Error(), "未注册") || strings.Contains(err.Error(), "不可用") {
			return nil, ErrNodeOffline
		}
		return nil, err
	}

	wait := time.Duration(timeoutSec+5) * time.Second
	select {
	case res := <-ch:
		g.audit.record(TermAuditEntry{
			Ts: nowMs(), Action: "exec", SessionID: sessionID, ExecID: execID,
			NodeID: nodeID, Operator: operator, Cmd: cmd, Args: strings.Join(args, " "),
			OK: res.OK, ExitCode: res.ExitCode, Rejected: res.Rejected,
			Reason: res.Reason, DurationMs: res.DurationMs,
		})
		return res, nil
	case <-time.After(wait):
		g.audit.record(TermAuditEntry{Ts: nowMs(), Action: "exec", SessionID: sessionID, ExecID: execID, NodeID: nodeID, Operator: operator, Cmd: cmd, Args: strings.Join(args, " "), Rejected: true, Reason: "结果等待超时（边缘无响应）"})
		return &protocol.TermResultPayload{ExecID: execID, SessionID: sessionID, Rejected: true, Reason: "结果等待超时（边缘无响应）", Ts: nowMs()}, nil
	}
}

// DeliverResult 接收边侧 TermResult（cloudhub 回调注入；连接读 goroutine 调用）。
// 归属校验：execID 必须有 pending，且其会话节点与上报节点一致（防伪造）。
func (g *Gateway) DeliverResult(nodeID string, p *protocol.TermResultPayload) {
	if p == nil || p.ExecID == "" {
		return
	}
	g.mu.RLock()
	s := g.sessions[p.SessionID]
	g.mu.RUnlock()
	if s == nil || s.NodeID != nodeID {
		log.Warnf("termgw: 丢弃归属不符的 TermResult（node=%s session=%s）", nodeID, p.SessionID)
		return
	}
	g.pendMu.Lock()
	ch := g.pending[p.ExecID]
	g.pendMu.Unlock()
	if ch == nil {
		return // 无等待方（超时已清理/重复结果）：静默丢弃
	}
	select {
	case ch <- p:
	default: // chan 容量 1 且仅投一次，防御性忽略
	}
}

// expire 过期扫描：空闲超 TTL 的 active 会话置 expired 并审计。
func (g *Gateway) expire() {
	cutoff := nowMs() - g.ttl.Milliseconds()
	g.mu.Lock()
	var expired []*Session
	for _, s := range g.sessions {
		if s.State == SessionActive && s.LastActive < cutoff {
			s.State = SessionExpired
			g.perNode[s.NodeID]--
			g.active--
			if g.perNode[s.NodeID] <= 0 {
				delete(g.perNode, s.NodeID)
			}
			expired = append(expired, s)
		}
	}
	g.mu.Unlock()
	for _, s := range expired {
		g.audit.record(TermAuditEntry{
			Ts: nowMs(), Action: "session-expire", SessionID: s.ID,
			NodeID: s.NodeID, Operator: s.Operator,
		})
	}
}
