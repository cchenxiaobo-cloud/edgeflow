package opcua

import (
	"context"
	"strings"
	"testing"
	"time"

	"edgeflow/edge/pkg/mapper"
	"edgeflow/edge/pkg/metamanager"
	opcuapkg "edgeflow/pkg/opcua"
	"edgeflow/pkg/opcuasim"
)

// fakeLedger 记录台账操作（测试断言用）。
type fakeLedger struct {
	recs []metamanager.OpRecord
}

func (f *fakeLedger) SaveOp(r metamanager.OpRecord) error {
	f.recs = append(f.recs, r)
	return nil
}

func (f *fakeLedger) last() *metamanager.OpRecord {
	if len(f.recs) == 0 {
		return nil
	}
	return &f.recs[len(f.recs)-1]
}

// findDown 从后往前查找方向为 down 的台账记录（HandleCommand 后
// snapshot 会追加一条 up 记录，down 记录在其前）。
func (f *fakeLedger) findDown() *metamanager.OpRecord {
	for i := len(f.recs) - 1; i >= 0; i-- {
		if f.recs[i].Direction == metamanager.DirDown {
			return &f.recs[i]
		}
	}
	return nil
}

// startMapper 启动模拟器 + Mapper（模拟器 Step 20ms 加速收敛）。
func startMapper(t *testing.T, points []PointDef, opts ...Option) (*OPCUAMapper, *opcuasim.Simulator, *fakeLedger) {
	t.Helper()
	sim := opcuasim.New("127.0.0.1:0", opcuasim.WithStep(20*time.Millisecond), opcuasim.WithSeed(1))
	if err := sim.Start(); err != nil {
		t.Fatalf("模拟器启动失败: %v", err)
	}
	t.Cleanup(func() { _ = sim.Stop() })
	ledger := &fakeLedger{}
	m, err := New("opc.tcp://"+sim.Addr(),
		append([]Option{WithPoints(points), WithLedger(ledger)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.Start(context.TODO()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	return m, sim, ledger
}

var simPoints = []PointDef{
	{Name: "temperature", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodeTemperature)},
	{Name: "humidity", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodeHumidity)},
	{Name: "running", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodeRunning)},
	{Name: "label", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodeLabel)},
	{Name: "setpoint", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodeSetpoint)},
}

// TestCollectConversion 验证 Collect 转换策略：数值/Boolean/String 全部转出，
// 台账记录 up/ok。
func TestCollectConversion(t *testing.T) {
	m, _, ledger := startMapper(t, simPoints)
	props, err := m.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if props["running"] != 1 {
		t.Fatalf("running 应为 1: %v", props["running"])
	}
	if props["label"] != 0 {
		t.Fatalf("label 字符串 'opcua-sim' 应 ParseFloat 失败跳过: %v", props["label"])
	}
	if _, ok := props["temperature"]; !ok {
		t.Fatalf("temperature 缺失: %v", props)
	}
	if _, ok := props["setpoint"]; !ok {
		t.Fatalf("setpoint 缺失: %v", props)
	}
	last := ledger.last()
	if last == nil || last.Direction != metamanager.DirUp || last.Result != "ok" {
		t.Fatalf("台账异常: %+v", last)
	}
}

// TestCollectBadNode 验证未知节点 Status 非 Good → 属性跳过（不报错）。
func TestCollectBadNode(t *testing.T) {
	pts := []PointDef{{Name: "bad", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, 99999)}}
	m, _, _ := startMapper(t, pts)
	props, err := m.Collect()
	if err != nil {
		t.Fatalf("Collect 不应报错: %v", err)
	}
	if len(props) != 0 {
		t.Fatalf("未知节点应跳过: %v", props)
	}
}

// TestHandleCommand 验证写点 + 回读验证 + 台账 down/ok + 快照。
func TestHandleCommand(t *testing.T) {
	m, _, ledger := startMapper(t, simPoints)
	rep, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "opcua-device-01",
		Namespace:  "default",
		Property:   "setpoint",
		Value:      77.5,
	})
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if rep.Properties["setpoint"] != 77.5 {
		t.Fatalf("快照 setpoint 未更新: %v", rep.Properties)
	}
	last := ledger.findDown()
	if last == nil || last.Direction != metamanager.DirDown || last.Result != "ok" {
		t.Fatalf("台账异常: %+v", last)
	}
	if !strings.Contains(last.RegAddr, "3001") {
		t.Fatalf("台账 RegAddr 异常: %s", last.RegAddr)
	}
}

// TestHandleCommandUnknown 验证未配置点位 → 错误。
func TestHandleCommandUnknown(t *testing.T) {
	m, _, _ := startMapper(t, simPoints)
	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "opcua-device-01", Property: "nonexistent", Value: 1,
	}); err == nil {
		t.Fatalf("未知属性应报错")
	}
}

// TestHandleCommandWriteReadonly 验证写只读节点 → 服务端拒绝 → 错误。
func TestHandleCommandWriteReadonly(t *testing.T) {
	pts := []PointDef{{Name: "temperature", NodeID: opcuapkg.NewNodeID(opcuasim.Namespace, opcuasim.NodeTemperature)}}
	m, _, _ := startMapper(t, pts)
	if _, err := m.HandleCommand(mapper.DeviceCommand{
		DeviceName: "opcua-device-01", Property: "temperature", Value: 50,
	}); err == nil {
		t.Fatalf("写只读节点应报错")
	}
}

// TestStartStopIdempotent 验证 Start/Stop 幂等。
func TestStartStopIdempotent(t *testing.T) {
	m, _, _ := startMapper(t, simPoints)
	if err := m.Start(context.TODO()); err != nil {
		t.Fatalf("重复 Start: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("重复 Stop: %v", err)
	}
}

// TestVariantToFloat 验证转换策略全表。
func TestVariantToFloat(t *testing.T) {
	cases := []struct {
		v    any
		want float64
		ok   bool
	}{
		{v: int8(-5), want: -5, ok: true},
		{v: int16(-5), want: -5, ok: true},
		{v: int32(-5), want: -5, ok: true},
		{v: int64(-5), want: -5, ok: true},
		{v: uint8(5), want: 5, ok: true},
		{v: uint16(5), want: 5, ok: true},
		{v: uint32(5), want: 5, ok: true},
		{v: uint64(5), want: 5, ok: true},
		{v: float32(2.5), want: 2.5, ok: true},
		{v: float64(2.5), want: 2.5, ok: true},
		{v: true, want: 1, ok: true},
		{v: false, want: 0, ok: true},
		{v: "3.14", want: 3.14, ok: true},
		{v: "abc", ok: false},
		{v: struct{}{}, ok: false},
		{v: []float64{1, 2}, ok: false},
	}
	for _, c := range cases {
		got, ok := variantToFloat(c.v)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("variantToFloat(%v) = %v,%v want %v,%v", c.v, got, ok, c.want, c.ok)
		}
	}
}

// TestParseNodes 验证点位表解析。
func TestParseNodes(t *testing.T) {
	pts, err := ParseNodes("temperature=ns=2;i=1001,humidity=ns=2;i=1002,setpoint=ns=2;i=3001")
	if err != nil {
		t.Fatalf("ParseNodes: %v", err)
	}
	if len(pts) != 3 || pts[0].Name != "temperature" || pts[0].NodeID.String() != "ns=2;i=1001" {
		t.Fatalf("解析异常: %+v", pts)
	}
	// name 缺省退化为 nodeId
	pts2, err := ParseNodes("ns=2;i=2001")
	if err != nil || len(pts2) != 1 || pts2[0].Name != "ns=2;i=2001" {
		t.Fatalf("退化解析异常: %+v err %v", pts2, err)
	}
	// 非法条目整体报错
	if _, err := ParseNodes("a=ns=2;i=1001,b=badnode"); err == nil {
		t.Fatalf("非法条目应报错")
	}
	// 空串 → nil
	if pts3, err := ParseNodes(""); err != nil || pts3 != nil {
		t.Fatalf("空串应返回 nil: %v err %v", pts3, err)
	}
}

// TestReconnectAfterStop 验证 Stop 后重新 Start 可恢复连接。
func TestReconnectAfterStop(t *testing.T) {
	m, _, _ := startMapper(t, simPoints)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.Start(context.TODO()); err != nil {
		t.Fatalf("重新 Start: %v", err)
	}
	props, err := m.Collect()
	if err != nil {
		t.Fatalf("重新启动后 Collect: %v", err)
	}
	if len(props) == 0 {
		t.Fatalf("重新启动后采集为空")
	}
}
