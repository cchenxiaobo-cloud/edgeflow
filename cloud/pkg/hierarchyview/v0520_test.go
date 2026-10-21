// v0520_test.go：告警计数快照路径单测（spec 0025 裁决 2，KI §52#1 修复锚）。
package hierarchyview

import (
	"testing"

	"edgeflow/cloud/pkg/regionstore"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/alarm"
)

// countAlarms 实现双接口的替身：计数源可用时 List 不应被调用（listCalls 断言）；
// List 若被调用返回空（模拟截断语义——计数路径绕过 List）。
type countAlarms struct {
	counts    map[string]map[string]int64
	listCalls int
}

func (f *countAlarms) List(nodeID, state, severity string, limit int) []alarm.Alarm {
	f.listCalls++
	return nil
}

func (f *countAlarms) CountByNodeState() map[string]map[string]int64 { return f.counts }

// US-1 计数源路径：计数不受 alarmScanLimit 截断语义影响（等价 >2000 场景），
// Summary 与 AlarmStats 均走 CountByNodeState（List 零调用）。
func TestV0520CountSourcePath(t *testing.T) {
	reg := regionstore.New("")
	a, err := reg.Create("A", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.AssignNodes(a.ID, []string{"nA1"}); err != nil {
		t.Fatal(err)
	}
	nodes := &fakeNodes{list: []registry.NodeInfo{{NodeID: "nA1", Status: registry.StatusReady}}}
	alarms := &countAlarms{counts: map[string]map[string]int64{
		"nA1": {alarm.StateRaised: 1500, alarm.StateClosed: 500},
		"nX":  {alarm.StateRaised: 500}, // 未归属节点
	}}
	v := New(reg, nodes, alarms)

	root := v.Summary()
	if root.AlarmActiveCount != 1500 {
		t.Fatalf("活跃告警=%d（want 1500：closed 不计）", root.AlarmActiveCount)
	}
	if alarms.listCalls != 0 {
		t.Fatalf("计数源可用时 List 被调用 %d 次（应 0）", alarms.listCalls)
	}

	st := v.AlarmStats()
	if len(st.Regions) != 1 {
		t.Fatalf("区域数=%d（want 1）", len(st.Regions))
	}
	rs := st.Regions[0]
	if rs.RegionName != "A" || rs.Total != 2000 || rs.Raised != 1500 || rs.Closed != 500 || rs.Acked != 0 {
		t.Fatalf("区域计数不符: %+v", rs)
	}
	if st.Unassigned.Total != 500 || st.Unassigned.Raised != 500 {
		t.Fatalf("unassigned 桶不符: %+v", st.Unassigned)
	}
	if alarms.listCalls != 0 {
		t.Fatalf("AlarmStats 后 List 被调用 %d 次（应 0）", alarms.listCalls)
	}
}

// US-2 回退路径：未实现 AlarmCountSource 的旧替身走 List（行为与 v0.51 一致，
// limit=alarmScanLimit 传递不变）。
func TestV0520FallbackListPath(t *testing.T) {
	v, alarms := fixtureView(t)
	root := v.Summary()
	if root.AlarmActiveCount != 2 {
		t.Fatalf("回退路径活跃告警=%d（want 2：r1 raised + r2 acked，r3 closed 不计）", root.AlarmActiveCount)
	}
	if alarms.lastLimit != alarmScanLimit {
		t.Fatalf("List limit=%d（want %d）", alarms.lastLimit, alarmScanLimit)
	}
	st := v.AlarmStats()
	var aStat, bStat *RegionAlarmStat
	for i := range st.Regions {
		switch st.Regions[i].RegionName {
		case "A":
			aStat = &st.Regions[i]
		case "B":
			bStat = &st.Regions[i]
		}
	}
	if aStat == nil || bStat == nil {
		t.Fatalf("区域桶缺失: %+v", st.Regions)
	}
	if aStat.Total != 1 || aStat.Closed != 1 {
		t.Fatalf("A 计数不符: %+v", *aStat)
	}
	if bStat.Total != 2 || bStat.Raised != 1 || bStat.Acked != 1 {
		t.Fatalf("B 计数不符: %+v", *bStat)
	}
}
