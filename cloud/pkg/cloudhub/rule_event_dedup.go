// RuleEvent 接收幂等（v0.39.0，spec 0012 US-4）：按消息 ID 滚动去重。
//
// 补传（至少一次）语义下，边侧重发会带来重复投递；本组件在协议层消化
// 重复：同 ID 二次到达直接丢弃（不回调 handler），并维护 per-node
// 接收/重复计数（补传可视化数据源）。
// 对既有单发路径透明：不重发则不命中去重集，行为与 v0.38.0 一致。
package cloudhub

import (
	"sync"
)

// defaultRuleDedupCapacity 是去重滚动窗口容量（FIFO 淘汰最旧；
// 超窗后的"超老重复"会被再次接收——KNOWN-ISSUES §40 登记边界）。
const defaultRuleDedupCapacity = 10000

// RuleEventCounters 是节点级上行接收计数（可视化数据源）。
type RuleEventCounters struct {
	Received   int64 // 去重后的接收数
	Duplicated int64 // 重复丢弃数
}

// ruleEventDedup 是滚动去重集合 + 节点计数（send-once 集合，容量固定）。
type ruleEventDedup struct {
	mu       sync.Mutex
	capacity int
	seen     map[string]struct{}
	order    []string // FIFO 淘汰序（与 seen 同步维护）
	perNode  map[string]*RuleEventCounters
}

// newRuleEventDedup 构造去重组件；capacity <= 0 用默认容量。
func newRuleEventDedup(capacity int) *ruleEventDedup {
	if capacity <= 0 {
		capacity = defaultRuleDedupCapacity
	}
	return &ruleEventDedup{
		capacity: capacity,
		seen:     make(map[string]struct{}),
		perNode:  make(map[string]*RuleEventCounters),
	}
}

// checkAndCount 检查消息 ID：首次 → 登记 + received++，返回 false；
// 重复 → duplicated++，返回 true。msgID 为空时不去重（放行 + received++）。
func (d *ruleEventDedup) checkAndCount(nodeID, msgID string) (duplicated bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.perNode[nodeID]
	if c == nil {
		c = &RuleEventCounters{}
		d.perNode[nodeID] = c
	}
	if msgID == "" {
		c.Received++
		return false
	}
	if _, ok := d.seen[msgID]; ok {
		c.Duplicated++
		return true
	}
	d.seen[msgID] = struct{}{}
	d.order = append(d.order, msgID)
	if len(d.order) > d.capacity {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, oldest)
	}
	c.Received++
	return false
}

// ensureRuleDedup 懒初始化去重组件（Server 零值防御）。
func (s *Server) ensureRuleDedup() *ruleEventDedup {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ruleDedup == nil {
		s.ruleDedup = newRuleEventDedup(defaultRuleDedupCapacity)
	}
	return s.ruleDedup
}

// ruleEventCheckDup 检查并登记 RuleEvent 消息 ID（幂等）。
// 返回 true 表示重复投递（调用方应丢弃、不回调 handler）。
func (s *Server) ruleEventCheckDup(nodeID, msgID string) bool {
	return s.ensureRuleDedup().checkAndCount(nodeID, msgID)
}

// RuleEventStats 返回各节点上行接收计数快照（去重后接收 / 重复丢弃）。
// 供可视化端点读取（补传观测面）。
func (s *Server) RuleEventStats() map[string]RuleEventCounters {
	s.mu.RLock()
	d := s.ruleDedup
	s.mu.RUnlock()
	if d == nil {
		return map[string]RuleEventCounters{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]RuleEventCounters, len(d.perNode))
	for k, v := range d.perNode {
		out[k] = *v
	}
	return out
}
