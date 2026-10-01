// v0400_test.go：告警模型单测（spec 0013 US-1）。
package alarm

import (
	"encoding/json"
	"testing"
)

// testAlarm 构造一条合法告警。
func testAlarm(id string) Alarm {
	return Alarm{
		AlarmID: id, NodeID: "node-1", Source: SourceRule, RuleID: "rule-a",
		Namespace: "default", DeviceName: "sensor-01",
		Severity: SeverityWarning, State: StateRaised,
		Message: "温度越限", Count: 1, RaisedAt: 1000, UpdatedAt: 1000,
	}
}

// TestAlarmValidate 覆盖合法与非法形态。
func TestAlarmValidate(t *testing.T) {
	a := testAlarm("alm-1")
	if err := a.Validate(); err != nil {
		t.Fatalf("合法告警不应报错: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Alarm)
	}{
		{"缺 alarmId", func(a *Alarm) { a.AlarmID = "" }},
		{"缺 nodeId", func(a *Alarm) { a.NodeID = "" }},
		{"非法 source", func(a *Alarm) { a.Source = "alien" }},
		{"非法 severity", func(a *Alarm) { a.Severity = "fatal" }},
		{"非法 state", func(a *Alarm) { a.State = "open" }},
		{"count 为 0", func(a *Alarm) { a.Count = 0 }},
		{"缺 raisedAt", func(a *Alarm) { a.RaisedAt = 0 }},
		{"rule 源缺 ruleId", func(a *Alarm) { a.RuleID = "" }},
	}
	for _, tc := range cases {
		bad := testAlarm("alm-bad")
		tc.mut(&bad)
		if err := bad.Validate(); err == nil {
			t.Errorf("%s：应校验失败", tc.name)
		}
	}
}

// TestAlarmCanTransition 覆盖状态机合法/非法迁移矩阵。
func TestAlarmCanTransition(t *testing.T) {
	valid := map[string][]string{
		StateRaised: {StateAcked, StateAssigned, StateClosed},
		StateAcked:  {StateAssigned, StateClosed},
	}
	for from, tos := range valid {
		for _, to := range tos {
			if !CanTransition(from, to) {
				t.Errorf("%s→%s 应合法", from, to)
			}
		}
	}
	invalid := [][2]string{
		{StateRaised, StateRaised}, {StateAcked, StateAcked},
		{StateAssigned, StateAcked}, {StateAssigned, StateRaised},
		{StateClosed, StateClosed}, {StateClosed, StateRaised},
		{StateClosed, StateAcked}, {StateClosed, StateAssigned},
	}
	for _, p := range invalid {
		if CanTransition(p[0], p[1]) {
			t.Errorf("%s→%s 应非法", p[0], p[1])
		}
	}
}

// TestAlarmStateRank 覆盖秩与未知状态。
func TestAlarmStateRank(t *testing.T) {
	if !(StateRank(StateRaised) < StateRank(StateAcked) &&
		StateRank(StateAcked) < StateRank(StateAssigned) &&
		StateRank(StateAssigned) < StateRank(StateClosed)) {
		t.Fatalf("状态秩应单调递增")
	}
	if StateRank("alien") != -1 {
		t.Fatalf("未知状态秩应为 -1")
	}
}

// TestAlarmDedupKeyJSON 覆盖去重键与 JSON 往返。
func TestAlarmDedupKeyJSON(t *testing.T) {
	a := testAlarm("alm-2")
	want := "node-1|rule|rule-a|default|sensor-01"
	if got := a.DedupKey(); got != want {
		t.Fatalf("DedupKey = %q, want %q", got, want)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var back Alarm
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if back != a {
		t.Fatalf("JSON 往返不一致: %+v vs %+v", back, a)
	}
}
