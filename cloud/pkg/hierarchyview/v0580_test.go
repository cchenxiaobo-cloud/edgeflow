// v0580_test.go：静默窗口活跃口径单测（v0.58.0，spec 0031 裁决 4）。
package hierarchyview

import (
	"testing"

	"edgeflow/cloud/pkg/regionstore"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/alarm"
)

// fakeMutes 静默源替身：指定节点集合在测试期间恒静默。
type fakeMutes struct{ nodes map[string]bool }

func (f *fakeMutes) Muted(nodeID string, nowMs int64) bool { return f.nodes[nodeID] }

// US-1 静默排除（计数源路径）：被静默节点的活跃告警不计入区域活跃数；
// 未静默节点正常计数；AlarmStats 四态计数不受静默影响（历史事实）。
func TestV0580SummaryMuteExclusion(t *testing.T) {
	reg := regionstore.New("")
	a, err := reg.Create("A", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.AssignNodes(a.ID, []string{"nA1", "nA2"}); err != nil {
		t.Fatal(err)
	}
	nodes := &fakeNodes{list: []registry.NodeInfo{
		{NodeID: "nA1", Status: registry.StatusReady},
		{NodeID: "nA2", Status: registry.StatusReady},
	}}
	alarms := &countAlarms{counts: map[string]map[string]int64{
		"nA1": {alarm.StateRaised: 2},
		"nA2": {alarm.StateRaised: 1, alarm.StateClosed: 1},
	}}
	v := New(reg, nodes, alarms)
	v.SetMutes(&fakeMutes{nodes: map[string]bool{"nA1": true}})

	root := v.Summary()
	if root.AlarmActiveCount != 1 {
		t.Fatalf("活跃告警=%d（want 1：nA1 静默排除，nA2 raised=1）", root.AlarmActiveCount)
	}
}

// US-2 静默排除（回退路径）：List 替身同样生效（双路径口径一致）。
func TestV0580SummaryMuteExclusionFallback(t *testing.T) {
	v, alarms := fixtureView(t)
	// fixtureView：r1 raised nB1 / r2 acked nB1 / r3 closed nA1 → 活跃 2。
	v.SetMutes(&fakeMutes{nodes: map[string]bool{"nB1": true}})
	root := v.Summary()
	if root.AlarmActiveCount != 0 {
		t.Fatalf("nB1 静默后活跃=%d（want 0）", root.AlarmActiveCount)
	}
	_ = alarms
	// 解除静默（换空源）→ 恢复计数。
	v.SetMutes(nil)
	if got := v.Summary().AlarmActiveCount; got != 2 {
		t.Fatalf("解除静默后活跃=%d（want 2）", got)
	}
}

// US-3 无静默源：nil 行为不变（v0.57 既有行为锚）。
func TestV0580NoMuteSourceUnchanged(t *testing.T) {
	v, _ := fixtureView(t)
	if got := v.Summary().AlarmActiveCount; got != 2 {
		t.Fatalf("无静默源活跃=%d（want 2）", got)
	}
	if v.mutedNow("nB1", 0) {
		t.Fatalf("无静默源 mutedNow 应恒 false")
	}
}
