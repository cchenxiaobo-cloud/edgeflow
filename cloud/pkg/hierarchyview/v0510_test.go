package hierarchyview

import (
	"testing"

	"edgeflow/cloud/pkg/regionstore"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/alarm"
)

// ── 替身 ──

type fakeNodes struct{ list []registry.NodeInfo }

func (f *fakeNodes) List() []registry.NodeInfo { return f.list }

type fakeAlarms struct {
	list      []alarm.Alarm
	lastLimit int
}

func (f *fakeAlarms) List(nodeID, state, severity string, limit int) []alarm.Alarm {
	f.lastLimit = limit
	return f.list
}

// fixtures：中心 → A → B（A 子）；节点 nA1（A，Ready）/ nB1（B，Offline）/
// nX（未归属，Unknown）；告警 r1/r2 挂 nB1（B），closed 挂 nA1（A）。
func fixtureView(t *testing.T) (*View, *fakeAlarms) {
	t.Helper()
	reg := regionstore.New("")
	a, err := reg.Create("A", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := reg.Create("B", a.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.AssignNodes(a.ID, []string{"nA1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.AssignNodes(b.ID, []string{"nB1"}); err != nil {
		t.Fatal(err)
	}
	nodes := &fakeNodes{list: []registry.NodeInfo{
		{NodeID: "nA1", NodeName: "edge-a1", Status: registry.StatusReady, IP: "10.0.0.1"},
		{NodeID: "nB1", NodeName: "edge-b1", Status: registry.StatusOffline, IP: "10.0.0.2"},
		{NodeID: "nX", NodeName: "edge-x", Status: registry.StatusUnknown},
	}}
	alarms := &fakeAlarms{list: []alarm.Alarm{
		{AlarmID: "r1", NodeID: "nB1", State: alarm.StateRaised},
		{AlarmID: "r2", NodeID: "nB1", State: alarm.StateAcked},
		{AlarmID: "r3", NodeID: "nA1", State: alarm.StateClosed},
	}}
	return New(reg, nodes, alarms), alarms
}

// ── 用例 ──

// US-1 树形递归聚合：A.nodeCount=2（含子 B）、onlineCount=1；根 unassigned=1。
func TestV0510SummaryTreeAggregation(t *testing.T) {
	v, _ := fixtureView(t)
	root := v.Summary()
	if root.NodeCount != 2 || root.UnassignedCount != 1 {
		t.Fatalf("根聚合不符: count=%d（want 2 已归属） unassigned=%d（独立桶）", root.NodeCount, root.UnassignedCount)
	}
	if len(root.Children) != 1 {
		t.Fatalf("根 children=%d", len(root.Children))
	}
	a := root.Children[0]
	if a.ID == "" || a.NodeCount != 2 || a.OnlineCount != 1 {
		t.Fatalf("A 聚合不符: %+v", a)
	}
	var b *RegionSummary
	for _, c := range a.Children {
		if c.ParentID == a.ID {
			b = c
		}
	}
	if b == nil || b.NodeCount != 1 || b.OnlineCount != 0 {
		t.Fatalf("B 聚合不符: %+v", b)
	}
}

// US-2 告警递归聚合：A.alarmActiveCount=2（B 的 raised+acked；closed 不计）。
func TestV0510SummaryAlarmRollup(t *testing.T) {
	v, _ := fixtureView(t)
	a := v.Summary().Children[0]
	if a.AlarmActiveCount != 2 {
		t.Fatalf("A active=%d（want 2，closed 不计）", a.AlarmActiveCount)
	}
}

// US-3 在线判定权威：Unknown 计在线侧、Offline 排除（裁决 5）。
func TestV0510OnlineSideByStatus(t *testing.T) {
	if !onlineSide(registry.StatusReady) || !onlineSide(registry.StatusUnknown) {
		t.Fatal("Ready/Unknown 应为在线侧")
	}
	if onlineSide(registry.StatusOffline) {
		t.Fatal("Offline 应为离线")
	}
	v, _ := fixtureView(t)
	rows, ok := v.RegionNodes(v.Summary().Children[0].ID)
	if !ok {
		t.Fatal("RegionNodes false")
	}
	m := map[string]bool{}
	for _, r := range rows {
		m[r.NodeID] = r.Online
	}
	if !m["nA1"] || m["nB1"] {
		t.Fatalf("在线判定不符: %v", m)
	}
}

// US-4 策略标记：SetPolicy 后 PolicySet=true，清除（null）后 false。
func TestV0510SummaryPolicySet(t *testing.T) {
	v, _ := fixtureView(t)
	reg := v.regions
	aID := v.Summary().Children[0].ID
	if err := reg.SetPolicy(aID, []byte(`{"alarmRoute":"ops-a"}`), 1); err != nil {
		t.Fatal(err)
	}
	if !v.Summary().Children[0].PolicySet {
		t.Fatal("PolicySet 应为 true")
	}
	if err := reg.SetPolicy(aID, nil, 2); err != nil {
		t.Fatal(err)
	}
	if v.Summary().Children[0].PolicySet {
		t.Fatal("清除后 PolicySet 应为 false")
	}
}

// US-5 区域明细含子树递归 + 未知区域 404 语义。
func TestV0510RegionNodesSubtree(t *testing.T) {
	v, _ := fixtureView(t)
	aID := v.Summary().Children[0].ID
	rows, ok := v.RegionNodes(aID)
	if !ok {
		t.Fatal("RegionNodes false")
	}
	if len(rows) != 2 {
		t.Fatalf("子树明细=%d（want 2：nA1+nB1）", len(rows))
	}
	if rows[0].NodeID != "nA1" || rows[0].Region != aID || rows[0].IP == "" {
		t.Fatalf("明细行不符: %+v", rows[0])
	}
	if _, ok := v.RegionNodes("rg-nope"); ok {
		t.Fatal("未知区域应 false")
	}
}

// US-6 告警汇聚矩阵：区域×状态计数 + unassigned 桶 + regionName 透出。
func TestV0510AlarmStatsMatrix(t *testing.T) {
	v, _ := fixtureView(t)
	resp := v.AlarmStats()
	if len(resp.Regions) != 2 {
		t.Fatalf("regions=%d（want 2：A+B）", len(resp.Regions))
	}
	m := map[string]RegionAlarmStat{}
	for _, s := range resp.Regions {
		m[s.RegionName] = s
	}
	a := m["A"]
	if a.Total != 1 || a.Closed != 1 || a.Raised != 0 {
		t.Fatalf("A 计数不符: %+v", a)
	}
	b := m["B"]
	if b.Total != 2 || b.Raised != 1 || b.Acked != 1 {
		t.Fatalf("B 计数不符: %+v", b)
	}
	if resp.Unassigned.Total != 0 {
		t.Fatalf("unassigned=%d（fixture 无无归属告警）", resp.Unassigned.Total)
	}
}

// US-7 全空库：全零不炸。
func TestV0510EmptyEverything(t *testing.T) {
	v := New(regionstore.New(""), &fakeNodes{}, &fakeAlarms{})
	root := v.Summary()
	if root.NodeCount != 0 || root.UnassignedCount != 0 || len(root.Children) != 0 {
		t.Fatalf("空库汇总不符: %+v", root)
	}
	resp := v.AlarmStats()
	if resp.Unassigned.Total != 0 || len(resp.Regions) != 0 {
		t.Fatalf("空库告警统计不符: %+v", resp)
	}
}

// US-8 告警扫描上限传递：聚合调用必须带 alarmScanLimit（裁决 3 口径）。
func TestV0510AlarmScanLimit(t *testing.T) {
	v, alarms := fixtureView(t)
	_ = v.AlarmStats()
	if alarms.lastLimit != alarmScanLimit {
		t.Fatalf("limit=%d（want %d）", alarms.lastLimit, alarmScanLimit)
	}
}
