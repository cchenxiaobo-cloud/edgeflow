package opcuasim

import (
	"fmt"
	"net"
	"testing"
	"time"

	"edgeflow/pkg/opcua"
)

// startSim 启动模拟器并返回端点 URL（127.0.0.1:0 取空闲端口）。
func startSim(t *testing.T, opts ...Option) (*Simulator, string) {
	t.Helper()
	sim := New("127.0.0.1:0", append([]Option{WithStep(20 * time.Millisecond), WithSeed(1)}, opts...)...)
	if err := sim.Start(); err != nil {
		t.Fatalf("启动模拟器失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	return sim, "opc.tcp://" + sim.Addr()
}

// TestClientFullProtocol 验证客户端对模拟器全协议闭环：Open 握手 →
// Read 点位 → Write setpoint → 写后读回 → Close。
func TestClientFullProtocol(t *testing.T) {
	sim, endpoint := startSim(t)
	c, err := opcua.Open(endpoint, 3*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 读全部点位
	vals, err := c.Read([]opcua.NodeId{
		opcua.NewNodeID(Namespace, NodeTemperature),
		opcua.NewNodeID(Namespace, NodeHumidity),
		opcua.NewNodeID(Namespace, NodePressure),
		opcua.NewNodeID(Namespace, NodeRunning),
		opcua.NewNodeID(Namespace, NodeLabel),
		opcua.NewNodeID(Namespace, NodeSetpoint),
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(vals) != 6 {
		t.Fatalf("Read 结果数 %d != 6", len(vals))
	}
	// temperature 接近初始值
	if f, ok := vals[0].Value.Value.(float64); !ok || f < 20 || f > 30 {
		t.Fatalf("temperature 异常: %v", vals[0].Value.Value)
	}
	// running=true, label="opcua-sim"
	if b, ok := vals[3].Value.Value.(bool); !ok || !b {
		t.Fatalf("running 异常: %v", vals[3].Value.Value)
	}
	if s, ok := vals[4].Value.Value.(string); !ok || s != "opcua-sim" {
		t.Fatalf("label 异常: %v", vals[4].Value.Value)
	}

	// 写 setpoint → 读回一致
	v, _ := opcua.NewVariant(80.0)
	st, err := c.Write(opcua.NewNodeID(Namespace, NodeSetpoint), v)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !st.IsGood() {
		t.Fatalf("Write 非 Good: %s", st)
	}
	if sim.Setpoint() != 80.0 {
		t.Fatalf("模拟器 setpoint 未更新: %v", sim.Setpoint())
	}
	vals, err = c.Read([]opcua.NodeId{opcua.NewNodeID(Namespace, NodeSetpoint)})
	if err != nil || len(vals) != 1 || vals[0].Value == nil {
		t.Fatalf("写后读回失败: %v %+v", err, vals)
	}
	if f, _ := vals[0].Value.Value.(float64); f != 80.0 {
		t.Fatalf("写后读回不一致: %v", vals[0].Value.Value)
	}
}

// TestClientReadBadNode 验证读未知节点 → BadNodeIdUnknown。
func TestClientReadBadNode(t *testing.T) {
	_, endpoint := startSim(t)
	c, err := opcua.Open(endpoint, 3*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()
	vals, err := c.Read([]opcua.NodeId{opcua.NewNodeID(Namespace, 99999)})
	if err != nil {
		t.Fatalf("Read 未知节点不应报错: %v", err)
	}
	if len(vals) != 1 || vals[0].Status == nil || vals[0].Status.IsGood() {
		t.Fatalf("未知节点应返回 Bad: %+v", vals)
	}
}

// TestClientWriteReadonly 验证写只读节点 → BadNotWritable。
func TestClientWriteReadonly(t *testing.T) {
	_, endpoint := startSim(t)
	c, err := opcua.Open(endpoint, 3*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()
	v, _ := opcua.NewVariant(1.0)
	st, err := c.Write(opcua.NewNodeID(Namespace, NodeTemperature), v)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if st.IsGood() {
		t.Fatalf("写只读节点应返回非 Good: %s", st)
	}
}

// TestTempConverges 验证温度向 setpoint 收敛（等待若干周期）。
func TestTempConverges(t *testing.T) {
	_, endpoint := startSim(t, WithStep(15*time.Millisecond))
	c, err := opcua.Open(endpoint, 3*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()
	v, _ := opcua.NewVariant(60.0)
	if _, err := c.Write(opcua.NewNodeID(Namespace, NodeSetpoint), v); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// 等待 30 个周期（450ms），温度应明显向 60 靠近
	time.Sleep(450 * time.Millisecond)
	vals, err := c.Read([]opcua.NodeId{opcua.NewNodeID(Namespace, NodeTemperature)})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	f, ok := vals[0].Value.Value.(float64)
	if !ok {
		t.Fatalf("temperature 类型异常: %T", vals[0].Value.Value)
	}
	// 初始 ~24.5，30 周期后应 > 45
	if f < 40 {
		t.Fatalf("温度未收敛（当前 %.1f，期望 >40）", f)
	}
}

// TestMaxConns 验证并发连接上限。
func TestMaxConns(t *testing.T) {
	sim, _ := startSim(t, WithMaxConns(1))
	// 第一个连接占用
	c1, err := opcua.Open("opc.tcp://"+sim.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("第一个连接失败: %v", err)
	}
	defer func() { _ = c1.Close() }()
	// 第二个连接应被拒绝（上限 1）
	if _, err := opcua.Open("opc.tcp://"+sim.Addr(), 2*time.Second); err == nil {
		t.Fatalf("超过连接上限应被拒绝")
	}
}

// TestNodeTable 验证点位表包含全部节点。
func TestNodeTable(t *testing.T) {
	sim, _ := startSim(t)
	table := sim.NodeTable()
	for _, want := range []string{"1001", "1002", "1003", "2001", "2002", "3001"} {
		if !contains(table, want) {
			t.Fatalf("点位表缺少 %s", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = fmt.Sprintf
var _ = net.JoinHostPort
