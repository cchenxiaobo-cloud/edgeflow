package registry

import (
	"testing"
	"time"

	"edgeflow/cloud/pkg/cloudhub"
)

// TestCloudHubAdapterEvents 验证桥接器把 CloudHub 事件正确映射到注册表。
func TestCloudHubAdapterEvents(t *testing.T) {
	reg := New()
	adapter := NewCloudHubAdapter(reg)

	// 注册成功 → Register（含元数据转换）
	regAt := time.Now().Add(-time.Hour)
	adapter.OnNodeRegistered(cloudhub.NodeInfo{
		NodeID:          "node-1",
		Arch:            "amd64",
		OS:              "linux",
		EdgecoreVersion: "v0.2.0",
		CPU:             8,
		Memory:          16 << 30,
		RemoteIP:        "10.1.2.3",
		RegisteredAt:    regAt,
	})

	info, ok := reg.Get("node-1")
	if !ok {
		t.Fatal("注册事件后应能在注册表查到节点")
	}
	if info.Arch != "amd64" || info.EdgecoreVersion != "v0.2.0" || info.CPU != 8 || info.Memory != 16<<30 {
		t.Errorf("元数据转换不符: %+v", info)
	}
	if info.IP != "10.1.2.3" {
		t.Errorf("IP 映射不符: %s", info.IP)
	}
	// NodeName：M1 与 nodeID 一致
	if info.NodeName != "node-1" {
		t.Errorf("NodeName 应与 nodeID 一致，实际 %s", info.NodeName)
	}
	if info.RegisteredAt != regAt.UnixMilli() {
		t.Errorf("RegisteredAt 毫秒转换不符: %d", info.RegisteredAt)
	}
	if info.Status != StatusReady {
		t.Errorf("注册后状态应为 Ready，实际 %s", info.Status)
	}

	// 断开 → MarkOffline
	adapter.OnNodeDisconnected("node-1")
	info, _ = reg.Get("node-1")
	if info.Status != StatusOffline {
		t.Errorf("断开后状态应为 Offline，实际 %s", info.Status)
	}

	// 心跳 → 恢复 Ready 并刷新时间
	before := time.Now().UnixMilli()
	adapter.OnNodeHeartbeat("node-1")
	info, _ = reg.Get("node-1")
	if info.Status != StatusReady {
		t.Errorf("心跳后状态应为 Ready，实际 %s", info.Status)
	}
	if info.LastHeartbeatAt < before {
		t.Errorf("心跳应刷新 LastHeartbeatAt: %d < %d", info.LastHeartbeatAt, before)
	}
}
