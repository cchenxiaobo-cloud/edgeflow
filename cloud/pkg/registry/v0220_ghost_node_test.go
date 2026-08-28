package registry

import (
	"testing"

	"edgeflow/cloud/pkg/cloudhub"
)

// T-05（CHN-01，v0.22.0）验收 2（下游视角）：同连接换 nodeID 重注册的
// 幽灵 Ready 节点防御。cloudhub 侧事件顺序断言见
// cloud/pkg/cloudhub/v0220_reregister_test.go（本包反向依赖 cloudhub，
// 桥接测试只能放本包）。
//
// 链路：cloudhub 补发 OnNodeDisconnected(old) → CloudHubAdapter →
// Store.MarkOffline(old)。若上游漏发/乱序，Store 中旧节点将停留 Ready
// （幽灵 Ready 节点：下游指令调度按 Ready 选目标，会打到已不存在的连接）。

// TestAdapterReRegisterNoGhostReady 模拟生产事件序列（换 ID 重注册）：
// register(old) → disconnected(old) → register(new)，断言 Store 状态迁移。
func TestAdapterReRegisterNoGhostReady(t *testing.T) {
	store := New(WithOfflineTTL(0)) // TTL=0 禁惰性 GC，断言稳定
	adapter := NewCloudHubAdapter(store)

	// ① 旧节点注册（生产：cloudhub 补发事件，元数据来自 Register payload）
	adapter.OnNodeRegistered(cloudhub.NodeInfo{
		NodeID: "node-ghost-a", Arch: "arm64", OS: "linux", RemoteIP: "10.0.0.1",
	})
	if info, ok := store.Get("node-ghost-a"); !ok {
		t.Fatalf("旧节点注册后应存在于 Store")
	} else if info.Status != StatusReady {
		t.Fatalf("旧节点注册后状态 = %s，期望 Ready", info.Status)
	}

	// ② 换 ID 重注册：cloudhub 补发旧节点断开事件（T-05 核心链路）
	adapter.OnNodeDisconnected("node-ghost-a")

	// ③ 新节点注册
	adapter.OnNodeRegistered(cloudhub.NodeInfo{
		NodeID: "node-ghost-b", Arch: "arm64", OS: "linux", RemoteIP: "10.0.0.2",
	})

	// 幽灵断言：旧节点必须已离开 Ready（MarkOffline → Offline）
	info, ok := store.Get("node-ghost-a")
	if !ok {
		t.Logf("旧节点已从 Store 移除（未残留任何状态），通过")
		return
	}
	if info.Status == StatusReady {
		t.Errorf("旧 nodeID node-ghost-a 状态 = Ready（幽灵 Ready 节点，会被指令调度误选），期望 Offline")
	}

	// 新节点正常 Ready
	newInfo, ok := store.Get("node-ghost-b")
	if !ok {
		t.Fatalf("新节点 node-ghost-b 应存在于 Store")
	}
	if newInfo.Status != StatusReady {
		t.Errorf("新 nodeID 状态 = %s，期望 Ready", newInfo.Status)
	}

	// 全量列表口径：Ready 集合不含旧 nodeID
	for _, n := range store.List() {
		if n.NodeID == "node-ghost-a" && n.Status == StatusReady {
			t.Errorf("List() 中旧 nodeID 呈 Ready 态（幽灵节点泄漏到下游调度面）")
		}
	}
}

// TestAdapterMissedDisconnectLeavesGhost 反向锚定：若上游漏发断开事件，
// 旧节点停留 Ready——这正是审计指出的缺陷形态（记录缺陷签名，防回归
// 者误把「漏发也无碍」当成等价）。
func TestAdapterMissedDisconnectLeavesGhost(t *testing.T) {
	store := New(WithOfflineTTL(0))
	adapter := NewCloudHubAdapter(store)

	adapter.OnNodeRegistered(cloudhub.NodeInfo{NodeID: "node-late-a"})
	// 漏发 OnNodeDisconnected("node-late-a")，直接注册新节点
	adapter.OnNodeRegistered(cloudhub.NodeInfo{NodeID: "node-late-b"})

	if info, ok := store.Get("node-late-a"); ok && info.Status == StatusReady {
		t.Logf("缺陷签名复现：漏发断开事件 → 旧节点 %s 停留 Ready（幽灵）。上游必须补发断开事件", info.Status)
	}
}
