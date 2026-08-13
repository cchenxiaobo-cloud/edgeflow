package registry

import (
	"sync"
	"testing"
	"time"

	v1alpha1 "edgeflow/apis/edge/v1alpha1"
)

// fullInfo 构造一个覆盖全部字段的节点信息，用于映射测试。
func fullInfo() *NodeInfo {
	return &NodeInfo{
		NodeID:          "node-1",
		NodeName:        "node-1",
		Arch:            "arm64",
		OS:              "linux",
		EdgecoreVersion: "v0.1.0",
		CPU:             4,
		Memory:          8 << 30,
		IP:              "192.168.1.10",
		RegisteredAt:    1700000000000, // 2023-11-14T22:13:20Z
		LastHeartbeatAt: 1700000060000, // 2023-11-14T22:14:20Z
		Status:          StatusReady,
	}
}

// TestToEdgeNodeFieldMapping 验证 NodeInfo → EdgeNode 的字段映射完整性。
func TestToEdgeNodeFieldMapping(t *testing.T) {
	r := New()
	node := r.ToEdgeNode(fullInfo())
	if node == nil {
		t.Fatal("ToEdgeNode 不应返回 nil")
	}

	// TypeMeta
	if node.Kind != "EdgeNode" {
		t.Errorf("Kind = %q，期望 EdgeNode", node.Kind)
	}
	if want := v1alpha1.SchemeGroupVersion.String(); node.APIVersion != want {
		t.Errorf("APIVersion = %q，期望 %q", node.APIVersion, want)
	}

	// ObjectMeta
	if node.Name != "node-1" {
		t.Errorf("Name = %q，期望 node-1", node.Name)
	}
	if node.CreationTimestamp != "2023-11-14T22:13:20Z" {
		t.Errorf("CreationTimestamp = %q，期望 2023-11-14T22:13:20Z", node.CreationTimestamp)
	}
	if got := node.Labels["edgeflow.io/node-id"]; got != "node-1" {
		t.Errorf("label node-id = %q", got)
	}
	if got := node.Labels["edgeflow.io/arch"]; got != "arm64" {
		t.Errorf("label arch = %q", got)
	}
	if got := node.Labels["edgeflow.io/os"]; got != "linux" {
		t.Errorf("label os = %q", got)
	}

	// Spec
	if node.Spec.NodeID != "node-1" {
		t.Errorf("Spec.NodeID = %q，期望 node-1", node.Spec.NodeID)
	}
	if node.Spec.Role != v1alpha1.NodeRoleEdge {
		t.Errorf("Spec.Role = %q，期望 edge", node.Spec.Role)
	}
	if len(node.Spec.Addresses) != 1 {
		t.Fatalf("Spec.Addresses 长度 = %d，期望 1", len(node.Spec.Addresses))
	}
	if addr := node.Spec.Addresses[0]; addr.Type != v1alpha1.NodeAddressTypeInternalIP || addr.Address != "192.168.1.10" {
		t.Errorf("Spec.Addresses[0] = %+v，期望 InternalIP/192.168.1.10", addr)
	}

	// Status
	if node.Status.Phase != v1alpha1.NodePhaseRunning {
		t.Errorf("Status.Phase = %q，期望 Running", node.Status.Phase)
	}
	if node.Status.HeartbeatTime != "2023-11-14T22:14:20Z" {
		t.Errorf("HeartbeatTime = %q，期望 2023-11-14T22:14:20Z", node.Status.HeartbeatTime)
	}
	if node.Status.LastSeenTime != node.Status.HeartbeatTime {
		t.Errorf("LastSeenTime = %q，应等于 HeartbeatTime %q", node.Status.LastSeenTime, node.Status.HeartbeatTime)
	}
	if node.Status.Version != "v0.1.0" {
		t.Errorf("Status.Version = %q，期望 v0.1.0", node.Status.Version)
	}
	if len(node.Status.Conditions) != 1 {
		t.Fatalf("Conditions 长度 = %d，期望 1", len(node.Status.Conditions))
	}
	cond := node.Status.Conditions[0]
	if cond.Type != v1alpha1.NodeConditionReady || cond.Status != "True" || cond.Reason != "HeartbeatReceived" {
		t.Errorf("Ready 条件 = %+v，期望 Type=Ready Status=True Reason=HeartbeatReceived", cond)
	}
	if cond.LastTransitionTime != node.Status.HeartbeatTime {
		t.Errorf("条件 LastTransitionTime = %q，应等于 HeartbeatTime", cond.LastTransitionTime)
	}
}

// TestToEdgeNodeStatusMapping 表驱动验证状态映射（Phase + Ready 条件）。
func TestToEdgeNodeStatusMapping(t *testing.T) {
	r := New()
	tests := []struct {
		name       string
		status     NodeStatus
		wantPhase  string
		wantCond   string
		wantReason string
	}{
		{"Ready", StatusReady, v1alpha1.NodePhaseRunning, "True", "HeartbeatReceived"},
		{"Offline", StatusOffline, v1alpha1.NodePhaseOffline, "False", "HeartbeatTimeout"},
		{"Unknown", StatusUnknown, v1alpha1.NodePhaseUnknown, "Unknown", "NodeNotConnected"},
		{"空状态兜底", "", v1alpha1.NodePhaseUnknown, "Unknown", "NodeNotConnected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := fullInfo()
			info.Status = tt.status
			node := r.ToEdgeNode(info)
			if node.Status.Phase != tt.wantPhase {
				t.Errorf("Phase = %q，期望 %q", node.Status.Phase, tt.wantPhase)
			}
			if len(node.Status.Conditions) != 1 {
				t.Fatalf("Conditions 长度 = %d，期望 1", len(node.Status.Conditions))
			}
			cond := node.Status.Conditions[0]
			if cond.Status != tt.wantCond || cond.Reason != tt.wantReason {
				t.Errorf("Ready 条件 = %+v，期望 Status=%s Reason=%s", cond, tt.wantCond, tt.wantReason)
			}
		})
	}
}

// TestToEdgeNodeEdgeCases 验证边界场景：nil 输入、空 IP/Arch/OS/时间戳。
func TestToEdgeNodeEdgeCases(t *testing.T) {
	r := New()
	if node := r.ToEdgeNode(nil); node != nil {
		t.Errorf("ToEdgeNode(nil) 应返回 nil，实际 %+v", node)
	}

	info := &NodeInfo{NodeID: "n1"}
	node := r.ToEdgeNode(info)
	if node.Name != "n1" {
		t.Errorf("Name 兜底应取 NodeID，实际 %q", node.Name)
	}
	if len(node.Spec.Addresses) != 0 {
		t.Errorf("IP 为空时 Addresses 应为空，实际 %+v", node.Spec.Addresses)
	}
	if len(node.Labels) != 1 {
		t.Errorf("Arch/OS 为空时 Labels 应只有 node-id，实际 %+v", node.Labels)
	}
	if node.Status.HeartbeatTime != "" || node.CreationTimestamp != "" {
		t.Errorf("时间戳为 0 时应为空串，实际 HeartbeatTime=%q CreationTimestamp=%q",
			node.Status.HeartbeatTime, node.CreationTimestamp)
	}
	if node.Status.Phase != v1alpha1.NodePhaseUnknown {
		t.Errorf("空状态 Phase = %q，期望 Unknown", node.Status.Phase)
	}
}

// TestToEdgeNodeIndependentCopy 验证转换结果是独立副本：
// 修改产出对象或输入信息，互不影响。
func TestToEdgeNodeIndependentCopy(t *testing.T) {
	r := New()
	info := fullInfo()
	node := r.ToEdgeNode(info)

	// 修改产出对象的引用字段（map/slice）
	node.Labels["hacked"] = "1"
	node.Labels["edgeflow.io/arch"] = "x86"
	node.Spec.Addresses[0].Address = "0.0.0.0"
	node.Status.Conditions[0].Status = "False"

	// 重新转换应得到干净副本
	again := r.ToEdgeNode(info)
	if again.Labels["hacked"] != "" || again.Labels["edgeflow.io/arch"] != "arm64" {
		t.Errorf("修改产出对象污染了后续转换: %+v", again.Labels)
	}
	if again.Spec.Addresses[0].Address != "192.168.1.10" {
		t.Errorf("修改产出对象污染了后续转换: %+v", again.Spec.Addresses)
	}
	if again.Status.Conditions[0].Status != "True" {
		t.Errorf("修改产出对象污染了后续转换: %+v", again.Status.Conditions)
	}

	// 修改输入也不影响已产出的对象（用未被测试改动的字段验证）
	info.NodeID = "mutated"
	info.IP = "9.9.9.9"
	if node.Spec.NodeID != "node-1" {
		t.Errorf("修改输入污染了已产出对象: %+v", node.Spec)
	}
	if node.Status.HeartbeatTime != "2023-11-14T22:14:20Z" {
		t.Errorf("修改输入污染了已产出对象: %+v", node.Status)
	}
	// 再次转换应反映新输入（反之亦然：输入变更只影响后续转换）
	fresh := r.ToEdgeNode(info)
	if fresh.Spec.NodeID != "mutated" || fresh.Spec.Addresses[0].Address != "9.9.9.9" {
		t.Errorf("再次转换应反映新输入: %+v", fresh.Spec)
	}
}

// TestListEdgeNodesSortedAndIndependent 验证列表按 Name 排序、返回独立副本。
func TestListEdgeNodesSortedAndIndependent(t *testing.T) {
	r := New()
	r.Register(sampleInfo("node-b"))
	r.Register(sampleInfo("node-a"))

	list := r.ListEdgeNodes()
	if len(list) != 2 {
		t.Fatalf("ListEdgeNodes 长度 = %d，期望 2", len(list))
	}
	if list[0].Name != "node-a" || list[1].Name != "node-b" {
		t.Errorf("应按 Name 排序: %+v", list)
	}
	if list[0].Spec.NodeID != "node-a" || list[0].Status.Phase != v1alpha1.NodePhaseRunning {
		t.Errorf("元素应为完整 EdgeNode: %+v", list[0])
	}

	// 修改返回对象（map/slice/字段）不污染注册表
	list[0].Labels["hacked"] = "1"
	list[0].Status.Phase = v1alpha1.NodePhaseOffline
	list[0].Spec.Addresses = append(list[0].Spec.Addresses, v1alpha1.NodeAddress{Type: "DNS", Address: "x"})

	nodes := r.ListEdgeNodes()
	if nodes[0].Labels["hacked"] != "" {
		t.Errorf("修改列表返回值污染了注册表: %+v", nodes[0].Labels)
	}
	if nodes[0].Status.Phase != v1alpha1.NodePhaseRunning {
		t.Errorf("修改列表返回值污染了注册表: %+v", nodes[0].Status)
	}
	// sampleInfo 带 IP，应保持原始 1 条 InternalIP，不被 append 的 DNS 污染
	if addrs := nodes[0].Spec.Addresses; len(addrs) != 1 ||
		addrs[0].Type != v1alpha1.NodeAddressTypeInternalIP || addrs[0].Address != "192.168.1.10" {
		t.Errorf("修改列表返回值污染了注册表: %+v", addrs)
	}
}

// TestListEdgeNodesConcurrent 验证并发读写+转换安全（配合 -race 检测数据竞争）。
func TestListEdgeNodesConcurrent(t *testing.T) {
	r := New()
	const workers = 8
	const perWorker = 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := "node-" + string(rune('a'+w))
			for i := 0; i < perWorker; i++ {
				switch i % 3 {
				case 0:
					r.Register(sampleInfo(id))
				case 1:
					r.MarkOffline(id)
				default:
					_ = r.ListEdgeNodes()
					if info, ok := r.Get(id); ok {
						_ = r.ToEdgeNode(&info)
					}
				}
			}
		}(w)
	}
	wg.Wait()

	// 每个 worker 注册了自己的节点，最终应全部在册
	if n := r.Count(); n != workers {
		t.Errorf("并发后 Count = %d，期望 %d", n, workers)
	}
	// 注册表快照应为每个节点一个对象
	if nodes := r.ListEdgeNodes(); len(nodes) != workers {
		t.Errorf("并发后 ListEdgeNodes 长度 = %d，期望 %d", len(nodes), workers)
	}
}

// TestListEdgeNodesEmpty 验证空注册表返回空切片（非 nil，JSON 序列化为 []）。
func TestListEdgeNodesEmpty(t *testing.T) {
	r := New()
	list := r.ListEdgeNodes()
	if list == nil || len(list) != 0 {
		t.Errorf("空注册表应返回空切片，实际 %v", list)
	}
}

// TestMsToRFC3339 验证毫秒时间戳转 RFC3339 的格式与边界。
func TestMsToRFC3339(t *testing.T) {
	if got := msToRFC3339(1700000000000); got != "2023-11-14T22:13:20Z" {
		t.Errorf("msToRFC3339(1700000000000) = %q，期望 2023-11-14T22:13:20Z", got)
	}
	if got := msToRFC3339(0); got != "" {
		t.Errorf("msToRFC3339(0) = %q，期望空串", got)
	}
	if got := msToRFC3339(-1); got != "" {
		t.Errorf("msToRFC3339(-1) = %q，期望空串", got)
	}
	// 输出必须是合法 RFC3339（可被 time.Parse 回读）
	now := time.Now().UnixMilli()
	if _, err := time.Parse(time.RFC3339, msToRFC3339(now)); err != nil {
		t.Errorf("msToRFC3339 输出不是合法 RFC3339: %v", err)
	}
}
