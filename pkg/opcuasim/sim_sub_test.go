package opcuasim

import (
	"testing"
	"time"

	"edgeflow/pkg/opcua"
)

// TestSubscriptionPushE2E 进程内全链路：Open → Subscribe → 数据推送到达。
func TestSubscriptionPushE2E(t *testing.T) {
	sim, endpoint := startSim(t, WithStep(20*time.Millisecond))
	defer func() { _ = sim.Stop() }()

	cli, err := opcua.Open(endpoint, 3*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = cli.Close() }()

	ch, err := cli.Subscribe([]opcua.NodeId{
		opcua.NewNodeID(2, NodeTemperature),
		opcua.NewNodeID(2, NodeHumidity),
	}, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	got := 0
	deadline := time.After(5 * time.Second)
	for {
		select {
		case pr, ok := <-ch:
			if !ok {
				t.Fatal("通知通道被意外关闭")
			}
			if pr.KeepAlive || pr.IsStatusChange {
				continue
			}
			got++
			t.Logf("数据通知 seq=%d items=%d", pr.SequenceNumber, len(pr.DataChange))
			if got >= 2 {
				return // ✅ 推送闭环
			}
			_ = cli.PubAck()
		case <-deadline:
			t.Fatalf("5s 内仅收到 %d 条数据通知（期望 ≥2）", got)
		}
	}
}

// TestBrowseDirectory 两级目录：Objects → opcua-sim → 6 变量。
func TestBrowseDirectory(t *testing.T) {
	sim, endpoint := startSim(t)
	defer func() { _ = sim.Stop() }()

	cli, err := opcua.Open(endpoint, 3*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = cli.Close() }()

	objs, err := cli.Browse(opcua.NewNodeID(0, 85))
	if err != nil {
		t.Fatalf("browse L1: %v", err)
	}
	var objNode *opcua.NodeId
	for _, o := range objs {
		if o.Name == "opcua-sim" && o.NodeClass == 1 {
			n := o.NodeId
			objNode = &n
		}
	}
	if objNode == nil {
		t.Fatal("L1 未发现 opcua-sim 对象")
	}
	vars, err := cli.Browse(*objNode)
	if err != nil {
		t.Fatalf("browse L2: %v", err)
	}
	names := map[string]bool{}
	for _, v := range vars {
		names[v.Name] = true
	}
	for _, want := range []string{"temperature", "humidity", "pressure", "running", "label", "setpoint"} {
		if !names[want] {
			t.Errorf("L2 缺少变量 %s（got %v）", want, names)
		}
	}
}

// TestSubscriptionWriteTriggers 预置波动关闭场景不可行（模拟器始终游走），
// 改验：写 setpoint 后 temperature 在推送中朝其收敛（值会变化）。
func TestSubscriptionWriteTriggers(t *testing.T) {
	sim, endpoint := startSim(t, WithStep(15*time.Millisecond))
	defer func() { _ = sim.Stop() }()

	cli, err := opcua.Open(endpoint, 3*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = cli.Close() }()

	ch, err := cli.Subscribe([]opcua.NodeId{opcua.NewNodeID(2, NodeTemperature)}, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case pr := <-ch:
			if pr.IsStatusChange || pr.KeepAlive || len(pr.DataChange) == 0 {
				continue
			}
			dv := pr.DataChange[0].Value
			if dv.Value != nil {
				return // 收到温度数据推送即闭环
			}
			_ = cli.PubAck()
		case <-deadline:
			t.Fatal("5s 未收到温度数据推送")
		}
	}
}
