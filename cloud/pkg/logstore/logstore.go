// 云端日志存储（v0.48.0，spec 0021 US-2/US-4）：per-node 环形缓冲 + 可选
// JSONL 持久化 + 检索查询。
//
// 选型理由：检索面是「运维查看」最小闭环——内存环形满足尾部/过滤/概要
// 全部查询形态且零依赖；JSONL 追加持久化用于跨重启保留（可选，与
// alarmstore 的 EnvFile 模式同口径的轻量持久化）。按天清理/外部索引
// （SQLite/ES）留后续版本（spec §3.8）。
//
// 并发模型：RWMutex 全局读锁 + 写锁；Append 批量一次加锁（LogSync 批
// ≤200 条），Query 快照后锁外过滤——存量 ≤5000 条/节点时查询为 O(n)
// 内存遍历，性能预算内（spec US-4）。
package logstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"

	"edgeflow/pkg/protocol"
)

// DefaultMaxPerNode 是每节点环形缓冲默认容量（条）。
const DefaultMaxPerNode = 5000

// jsonlRotateBytes 是 JSONL 文件轮转阈值（16MB，超过 rename 到 .old 单代轮转）。
const jsonlRotateBytes = 16 << 20

// nodeLog 是单节点的日志环形缓冲。
type nodeLog struct {
	entries []protocol.LogEntry // 定长环形；len(entries)==cap 时淘汰最旧
	head    int                 // 下一个写位置
	count   int                 // 有效条数
}

// push 追加一条（满则淘汰最旧）。
func (n *nodeLog) push(e protocol.LogEntry) {
	if len(n.entries) == 0 {
		return
	}
	if n.count < len(n.entries) {
		n.entries[(n.head+n.count)%len(n.entries)] = e
		n.count++
		return
	}
	// 满：覆盖 head（最旧），head 前移。
	n.entries[n.head] = e
	n.head = (n.head + 1) % len(n.entries)
}

// snapshot 返回时间序（最旧→最新）拷贝。
func (n *nodeLog) snapshot() []protocol.LogEntry {
	out := make([]protocol.LogEntry, 0, n.count)
	for i := 0; i < n.count; i++ {
		out = append(out, n.entries[(n.head+i)%len(n.entries)])
	}
	return out
}

// QueryOpts 是检索过滤条件（零值 = 全量）。
type QueryOpts struct {
	NodeID string // 空 = 跨节点
	Source string // 精确匹配；空 = 不过滤
	Level  string // 精确匹配；空 = 不过滤
	Q      string // message 子串匹配；空 = 不过滤
	Since  int64  // 毫秒；0 = 不限
	Until  int64  // 毫秒；0 = 不限
	Limit  int    // ≤0 用默认 200；上限 1000
	Offset int    // 时间序偏移（先过滤后偏移）
}

// NodeSummary 是节点日志概要（sources/levels 计数 + 最新时间）。
type NodeSummary struct {
	NodeID  string         `json:"nodeId"`
	Count   int            `json:"count"`
	Sources map[string]int `json:"sources"`
	Levels  map[string]int `json:"levels"`
	Latest  int64          `json:"latestTs"`
}

// Store 是云端日志存储（并发安全）。
type Store struct {
	mu       sync.RWMutex
	max      int
	nodes    map[string]*nodeLog
	filePath string // 空 = 不持久化
}

// New 创建存储；max <= 0 用默认。filePath 非空时启用 JSONL 持久化并回放。
func New(max int, filePath string) (*Store, error) {
	if max <= 0 {
		max = DefaultMaxPerNode
	}
	s := &Store{max: max, nodes: map[string]*nodeLog{}, filePath: filePath}
	if filePath != "" {
		if err := s.loadJSONL(filePath); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Append 追加一批日志（幂等性由调用方去重保证；批量一次加锁）。
func (s *Store) Append(nodeID string, entries []protocol.LogEntry) {
	if nodeID == "" || len(entries) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[nodeID]
	if !ok {
		n = &nodeLog{entries: make([]protocol.LogEntry, s.max)}
		s.nodes[nodeID] = n
	}
	for _, e := range entries {
		n.push(e)
	}
	s.appendJSONL(nodeID, entries)
}

// Query 按条件检索（时间序最旧→最新；offset/limit 作用于过滤后集合）。
// 返回 (页内条目, 过滤后总数)——总数供 X-Total-Count 分页语义（复核 P2-3）。
// 跨节点收集内联在本函数持锁段（复核 P1-1：原实现持 RLock 调 NodeIDs 二次
// 加锁，遇 pending writer 递归 RLock 互等死锁）。
func (s *Store) Query(o QueryOpts) ([]protocol.LogEntry, int) {
	limit := o.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	s.mu.RLock()
	var raw []protocol.LogEntry
	if o.NodeID != "" {
		if n, ok := s.nodes[o.NodeID]; ok {
			raw = n.snapshot()
		}
	} else {
		ids := make([]string, 0, len(s.nodes))
		for id, n := range s.nodes {
			if n.count > 0 {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			raw = append(raw, s.nodes[id].snapshot()...)
		}
		sort.Slice(raw, func(i, j int) bool { return raw[i].Ts < raw[j].Ts })
	}
	s.mu.RUnlock()

	q := strings.ToLower(o.Q)
	filtered := make([]protocol.LogEntry, 0, len(raw))
	for _, e := range raw { // 锁外过滤（快照已拷贝）
		if o.Source != "" && e.Source != o.Source {
			continue
		}
		if o.Level != "" && e.Level != o.Level {
			continue
		}
		if o.Since > 0 && e.Ts < o.Since {
			continue
		}
		if o.Until > 0 && e.Ts > o.Until {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Message), q) {
			continue
		}
		filtered = append(filtered, e)
	}
	total := len(filtered)
	out := filtered
	if o.Offset > 0 {
		if o.Offset >= len(out) {
			return []protocol.LogEntry{}, total
		}
		out = out[o.Offset:]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total
}

// NodeIDs 返回有日志的节点 ID（字典序）。
func (s *Store) NodeIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.nodes))
	for id, n := range s.nodes {
		if n.count > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// Summary 返回节点概要；节点不存在或为空返回 nil（调用方 404）。
func (s *Store) Summary(nodeID string) *NodeSummary {
	s.mu.RLock()
	n, ok := s.nodes[nodeID]
	if !ok || n.count == 0 {
		s.mu.RUnlock()
		return nil
	}
	snap := n.snapshot()
	s.mu.RUnlock()

	sum := &NodeSummary{
		NodeID:  nodeID,
		Count:   len(snap),
		Sources: map[string]int{},
		Levels:  map[string]int{},
	}
	for _, e := range snap {
		sum.Sources[e.Source]++
		sum.Levels[e.Level]++
		if e.Ts > sum.Latest {
			sum.Latest = e.Ts
		}
	}
	return sum
}

// Tail 返回节点最新 n 条（时间序最旧→最新；n<=0 默认 100，上限 1000）。
// 节点不存在或为空返回 nil。
func (s *Store) Tail(nodeID string, n int) []protocol.LogEntry {
	if n <= 0 {
		n = 100
	}
	if n > 1000 {
		n = 1000
	}
	s.mu.RLock()
	nd, ok := s.nodes[nodeID]
	if !ok || nd.count == 0 {
		s.mu.RUnlock()
		return nil
	}
	snap := nd.snapshot()
	s.mu.RUnlock()
	if len(snap) > n {
		snap = snap[len(snap)-n:]
	}
	return snap
}

// Delete 清空节点日志（轮转/清理入口）；返回是否存在。
func (s *Store) Delete(nodeID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.nodes[nodeID]
	delete(s.nodes, nodeID)
	return ok
}

// ── JSONL 持久化（可选）──

// appendJSONL 追加写持久化文件（>16MB 单代轮转到 .old）。调用方持写锁。
func (s *Store) appendJSONL(nodeID string, entries []protocol.LogEntry) {
	if s.filePath == "" {
		return
	}
	if st, err := os.Stat(s.filePath); err == nil && st.Size() >= jsonlRotateBytes {
		_ = os.Rename(s.filePath, s.filePath+".old") // 单代轮转：覆盖旧 .old
	}
	f, err := os.OpenFile(s.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return // 持久化尽力：失败不影响内存链路
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, e := range entries {
		line, err := json.Marshal(map[string]any{"nodeId": nodeID, "entry": e})
		if err != nil {
			continue
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	_ = w.Flush()
}

// loadJSONL 启动回放（损坏行跳过；每节点受 max 环形约束——回放最后 max 条）。
func (s *Store) loadJSONL(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 首次启动无文件：空库继续
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // 单行上限 8MB（4KB entry 兜底裕量）
	for sc.Scan() {
		var row struct {
			NodeID string            `json:"nodeId"`
			Entry  protocol.LogEntry `json:"entry"`
		}
		if json.Unmarshal(sc.Bytes(), &row) != nil || row.NodeID == "" {
			continue // 损坏行跳过（崩溃窗口容忍）
		}
		s.mu.Lock()
		n, ok := s.nodes[row.NodeID]
		if !ok {
			n = &nodeLog{entries: make([]protocol.LogEntry, s.max)}
			s.nodes[row.NodeID] = n
		}
		n.push(row.Entry)
		s.mu.Unlock()
	}
	return nil
}
