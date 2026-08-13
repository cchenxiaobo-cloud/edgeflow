package v1alpha1

import (
	"encoding/json"
	"testing"
)

// TestEdgeNodeSetDefaults 验证默认值填充：Role -> edge，Phase -> Pending。
func TestEdgeNodeSetDefaults(t *testing.T) {
	n := &EdgeNode{}
	n.SetDefaults()
	if n.Spec.Role != NodeRoleEdge {
		t.Errorf("Role 默认值 = %q, 期望 %q", n.Spec.Role, NodeRoleEdge)
	}
	if n.Status.Phase != NodePhasePending {
		t.Errorf("Phase 默认值 = %q, 期望 %q", n.Status.Phase, NodePhasePending)
	}
}

// TestEdgeNodeSetDefaultsKeepsExplicitValues 验证已显式填写的字段不被默认值覆盖。
func TestEdgeNodeSetDefaultsKeepsExplicitValues(t *testing.T) {
	n := &EdgeNode{
		Spec:   EdgeNodeSpec{Role: NodeRoleCloud},
		Status: EdgeNodeStatus{Phase: NodePhaseRunning},
	}
	n.SetDefaults()
	if n.Spec.Role != NodeRoleCloud {
		t.Errorf("Role = %q, 期望保持 %q", n.Spec.Role, NodeRoleCloud)
	}
	if n.Status.Phase != NodePhaseRunning {
		t.Errorf("Phase = %q, 期望保持 %q", n.Status.Phase, NodePhaseRunning)
	}
}

// TestEdgeNodeDeepCopy 验证深拷贝：修改副本的 map / slice / 嵌套结构，
// 不影响原对象。
func TestEdgeNodeDeepCopy(t *testing.T) {
	orig := &EdgeNode{
		ObjectMeta: ObjectMeta{
			Name:        "edge-1",
			Labels:      map[string]string{"zone": "a"},
			Annotations: map[string]string{"note": "x"},
		},
		Spec: EdgeNodeSpec{
			NodeID:    "id-1",
			Role:      NodeRoleEdge,
			Addresses: []NodeAddress{{Type: NodeAddressTypeInternalIP, Address: "10.0.0.1"}},
		},
		Status: EdgeNodeStatus{
			Phase:         NodePhaseRunning,
			HeartbeatTime: "2026-08-13T12:00:00Z",
			Conditions:    []NodeCondition{{Type: NodeConditionReady, Status: "True"}},
		},
	}

	cp := orig.DeepCopy()

	// 修改副本：map 键值、slice 元素与长度、嵌套结构、字符串字段
	cp.Labels["zone"] = "b"
	cp.Annotations["note"] = "y"
	cp.Spec.Addresses[0].Address = "10.0.0.99"
	cp.Spec.Addresses = append(cp.Spec.Addresses, NodeAddress{Type: NodeAddressTypeHostname, Address: "h1"})
	cp.Status.Conditions[0].Status = "False"
	cp.Status.Conditions = append(cp.Status.Conditions, NodeCondition{Type: "MemoryPressure"})
	cp.Status.HeartbeatTime = "2026-08-13T13:00:00Z"

	// 断言原对象不受影响
	if orig.Labels["zone"] != "a" {
		t.Errorf("副本修改 Labels 影响了原对象: %q", orig.Labels["zone"])
	}
	if orig.Annotations["note"] != "x" {
		t.Errorf("副本修改 Annotations 影响了原对象: %q", orig.Annotations["note"])
	}
	if orig.Spec.Addresses[0].Address != "10.0.0.1" {
		t.Errorf("副本修改 Addresses 影响了原对象: %q", orig.Spec.Addresses[0].Address)
	}
	if len(orig.Spec.Addresses) != 1 {
		t.Errorf("副本追加 Addresses 影响了原对象长度: %d", len(orig.Spec.Addresses))
	}
	if orig.Status.Conditions[0].Status != "True" {
		t.Errorf("副本修改 Conditions 影响了原对象: %q", orig.Status.Conditions[0].Status)
	}
	if len(orig.Status.Conditions) != 1 {
		t.Errorf("副本追加 Conditions 影响了原对象长度: %d", len(orig.Status.Conditions))
	}
	if orig.Status.HeartbeatTime != "2026-08-13T12:00:00Z" {
		t.Errorf("副本修改 HeartbeatTime 影响了原对象: %q", orig.Status.HeartbeatTime)
	}
}

// TestEdgeNodeJSONRoundTrip 验证 JSON 序列化往返：
// kind / apiVersion 出现在 JSON 顶层（TypeMeta 内联展开），
// metadata / spec / status 正常编解码。
func TestEdgeNodeJSONRoundTrip(t *testing.T) {
	orig := &EdgeNode{
		TypeMeta: TypeMeta{Kind: "EdgeNode", APIVersion: SchemeGroupVersion.String()},
		ObjectMeta: ObjectMeta{
			Name:      "edge-1",
			Namespace: "default",
			Labels:    map[string]string{"zone": "a"},
		},
		Spec: EdgeNodeSpec{
			NodeID: "id-1",
			Role:   NodeRoleEdge,
		},
		Status: EdgeNodeStatus{
			Phase:         NodePhaseRunning,
			HeartbeatTime: "2026-08-13T12:00:00Z",
		},
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal 失败: %v", err)
	}

	// kind / apiVersion 应出现在 JSON 顶层，而非嵌套的 typeMeta 里
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal 到 map 失败: %v", err)
	}
	if raw["kind"] != "EdgeNode" {
		t.Errorf("JSON 顶层 kind = %v, 期望 EdgeNode", raw["kind"])
	}
	if raw["apiVersion"] != "edgeflow.io/v1alpha1" {
		t.Errorf("JSON 顶层 apiVersion = %v, 期望 edgeflow.io/v1alpha1", raw["apiVersion"])
	}
	if _, ok := raw["metadata"]; !ok {
		t.Error("JSON 顶层缺少 metadata 字段")
	}
	if _, ok := raw["spec"]; !ok {
		t.Error("JSON 顶层缺少 spec 字段")
	}

	// 反序列化后字段应完整往返
	var got EdgeNode
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal 到 EdgeNode 失败: %v", err)
	}
	if got.Name != "edge-1" || got.Namespace != "default" {
		t.Errorf("metadata 往返失败: got Name=%q Namespace=%q", got.Name, got.Namespace)
	}
	if got.Spec.NodeID != "id-1" || got.Spec.Role != NodeRoleEdge {
		t.Errorf("spec 往返失败: got NodeID=%q Role=%q", got.Spec.NodeID, got.Spec.Role)
	}
	if got.Status.Phase != NodePhaseRunning {
		t.Errorf("status 往返失败: got Phase=%q", got.Status.Phase)
	}
}
