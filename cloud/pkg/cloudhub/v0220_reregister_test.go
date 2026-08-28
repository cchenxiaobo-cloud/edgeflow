package cloudhub

import (
	"sync"
	"testing"
	"time"
)

// T-05（CHN-01，v0.22.0）验收单测（cloudhub 视角）：同连接换 nodeID 重注册的
// 事件一致性与顺序。下游注册表（CloudHubAdapter → registry.Store）的幽灵节点
// 断言见 cloud/pkg/registry/v0220_ghost_node_test.go（注册表包反向依赖本包，
// 适配器桥接测试只能放那边）。
//
// 事实核查：handleRegister 的 evicted 补发逻辑已在既有代码（先删旧注册项 →
// 锁外先 OnNodeDisconnected(evicted) 再 OnNodeRegistered(new)），顺序正确；
// 既有 TestNodeEventsReRegisterNewID 已断言事件内容。本文件补齐审计验收 3：
// 事件顺序——先 OnNodeDisconnected(old) 后 OnNodeRegistered(new)。

// teeEvents 把事件同时喂给多个 NodeEvents（生产侧 SetNodeEvents 只收一个
// 回调；测试需要同时喂记录器与下游桥接器时用此组合器）。
type teeEvents []NodeEvents

func (t teeEvents) OnNodeRegistered(info NodeInfo) {
	for _, h := range t {
		h.OnNodeRegistered(info)
	}
}

func (t teeEvents) OnNodeHeartbeat(nodeID string) {
	for _, h := range t {
		h.OnNodeHeartbeat(nodeID)
	}
}

func (t teeEvents) OnNodeDisconnected(nodeID string) {
	for _, h := range t {
		h.OnNodeDisconnected(nodeID)
	}
}

// timelineEvent 是统一时间线上的单条事件。
type timelineEvent struct {
	kind   string // "registered" | "disconnected"
	nodeID string
}

// eventTimeline 按发生顺序统一记录注册/断开事件（并发安全）。fakeEvents 的
// registered/disconnected 是分切片，不带时间线，无法断言相对顺序。
type eventTimeline struct {
	mu  sync.Mutex
	log []timelineEvent
}

func (e *eventTimeline) OnNodeRegistered(info NodeInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, timelineEvent{kind: "registered", nodeID: info.NodeID})
}

func (e *eventTimeline) OnNodeHeartbeat(nodeID string) {}

func (e *eventTimeline) OnNodeDisconnected(nodeID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, timelineEvent{kind: "disconnected", nodeID: nodeID})
}

// TestReRegisterEventOrderGhostFree 验收 3 + 验收 2（cloudhub 视角）：
// 同连接换 ID 重注册 → ①事件顺序先断开后注册；②注册表无旧 nodeID 幽灵连接。
func TestReRegisterEventOrderGhostFree(t *testing.T) {
	srv, wsURL := newTestServer(t)
	tl := &eventTimeline{}
	srv.SetNodeEvents(tl)

	ws := dial(t, wsURL)
	register(t, ws, "node-seq-old")
	register(t, ws, "node-seq-new")

	waitFor(t, 3*time.Second, func() bool {
		tl.mu.Lock()
		defer tl.mu.Unlock()
		return len(tl.log) >= 2
	}, "应至少产生断开+注册两个事件")

	// 验收 2（cloudhub 视角）：注册表只剩新连接，旧 nodeID 无幽灵残留
	if srv.NodeCount() != 1 {
		t.Errorf("注册表应只剩新 nodeID 一个连接，实际 %d", srv.NodeCount())
	}
	if _, ok := srv.NodeInfo("node-seq-old"); ok {
		t.Errorf("旧 nodeID node-seq-old 不应残留于注册表（幽灵连接）")
	}
	if _, ok := srv.NodeInfo("node-seq-new"); !ok {
		t.Errorf("新 nodeID node-seq-new 应在注册表中")
	}

	// 验收 3：事件顺序——先断开(old) 后注册(new)
	tl.mu.Lock()
	defer tl.mu.Unlock()
	discoIdx, regIdx := -1, -1
	for i, e := range tl.log {
		if e.kind == "disconnected" && e.nodeID == "node-seq-old" && discoIdx < 0 {
			discoIdx = i
		}
		if e.kind == "registered" && e.nodeID == "node-seq-new" && regIdx < 0 {
			regIdx = i
		}
	}
	if discoIdx < 0 {
		t.Fatalf("未捕获 node-seq-old 的断开事件，时间线: %v", tl.log)
	}
	if regIdx < 0 {
		t.Fatalf("未捕获 node-seq-new 的注册事件，时间线: %v", tl.log)
	}
	if discoIdx > regIdx {
		t.Errorf("事件顺序错误：断开(idx=%d) 应先于注册(idx=%d)，时间线: %v", discoIdx, regIdx, tl.log)
	}
}
