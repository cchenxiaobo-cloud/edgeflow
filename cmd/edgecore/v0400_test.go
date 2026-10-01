// v0400_test.go：边缘告警管理器与设定值回告单测（spec 0013 US-2/US-5）。
package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"edgeflow/edge/pkg/devicetwin"
	"edgeflow/pkg/alarm"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/rules"
)

// fakeAlarmLinkage 记录联动回调。
type fakeAlarmLinkage struct {
	mu     sync.Mutex
	alarms []alarm.Alarm
}

func (f *fakeAlarmLinkage) OnAlarm(a alarm.Alarm) {
	f.mu.Lock()
	f.alarms = append(f.alarms, a)
	f.mu.Unlock()
}

// fakeDispatch 记录分发消息（priority, msg）。
type fakeDispatch struct {
	mu   sync.Mutex
	sent []dispatchRec
	fail bool
}

type dispatchRec struct {
	priority int
	msg      *protocol.Message
}

func (f *fakeDispatch) send(priority int, msg *protocol.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return
	}
	f.sent = append(f.sent, dispatchRec{priority: priority, msg: msg})
}

func (f *fakeDispatch) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// newTestAlarmManager 构造测试管理器（store 传 nil 走无台账降级路径）。
func newTestAlarmManager(t *testing.T, d *fakeDispatch, linkage Linkage) *alarmManager {
	t.Helper()
	m := newAlarmManager("node-1", nil, d.send, defaultAlarmReannounceSec)
	if linkage != nil {
		m.linkage = linkage
	}
	return m
}

// testRuleEvent 构造规则触发事件。
func testRuleEvent(ruleID, severity string) rules.Event {
	return rules.Event{
		RuleID: ruleID, RuleName: "R", DeviceName: "sensor-01", Namespace: "default",
		Property: "temperature", Value: 88, Severity: severity,
		Message: "越限", TriggeredAt: time.Now().UnixMilli(),
	}
}

// TestAlarmManagerFirstRaiseAndAggregate 覆盖首触建单 + 聚合计数 + 联动一次。
func TestAlarmManagerFirstRaiseAndAggregate(t *testing.T) {
	d := &fakeDispatch{}
	link := &fakeAlarmLinkage{}
	m := newTestAlarmManager(t, d, link)
	m.ObserveRuleEvent(testRuleEvent("rule-a", "critical"))
	if d.count() != 1 {
		t.Fatalf("首触应上行 1 条，实际 %d", d.count())
	}
	if len(link.alarms) != 1 {
		t.Fatalf("联动应触发 1 次，实际 %d", len(link.alarms))
	}
	// 同 episode 聚合：阈值（10）内不重发。
	for i := 0; i < 5; i++ {
		m.ObserveRuleEvent(testRuleEvent("rule-a", "critical"))
	}
	if d.count() != 1 {
		t.Fatalf("节流期内不应重发，实际 %d", d.count())
	}
	// 再命中至阈值（首触后累计 10 次命中 → 第 10 次强制重发，共 2 条）。
	for i := 0; i < 5; i++ {
		m.ObserveRuleEvent(testRuleEvent("rule-a", "critical"))
	}
	if d.count() != 2 {
		t.Fatalf("达阈值应重发，实际 %d", d.count())
	}
	// 重发保持同 alarmID、count 递增（云端合并语义的数据面）。
	d.mu.Lock()
	last := d.sent[len(d.sent)-1]
	d.mu.Unlock()
	var a alarm.Alarm
	if err := json.Unmarshal(last.msg.Payload, &a); err != nil {
		t.Fatalf("解析告警失败: %v", err)
	}
	if a.Count != 11 { // 首触 1 + 命中 10
		t.Fatalf("聚合 count = %d, want 11", a.Count)
	}
	if a.State != alarm.StateRaised {
		t.Fatalf("边侧状态应为 raised: %s", a.State)
	}
}

// TestAlarmManagerPriorityMapping 覆盖 severity→priority 映射。
func TestAlarmManagerPriorityMapping(t *testing.T) {
	d := &fakeDispatch{}
	m := newTestAlarmManager(t, d, nil)
	m.ObserveRuleEvent(testRuleEvent("r1", "critical"))
	m.ObserveRuleEvent(testRuleEvent("r2", "warning"))
	m.ObserveRuleEvent(testRuleEvent("r3", "info"))
	m.ObserveRuleEvent(testRuleEvent("r4", "alien")) // 未知等级归一 info
	d.mu.Lock()
	defer d.mu.Unlock()
	want := []int{2, 1, 0, 0}
	for i, w := range want {
		if d.sent[i].priority != w {
			t.Errorf("sent[%d].priority = %d, want %d", i, d.sent[i].priority, w)
		}
	}
}

// TestAlarmManagerEpisodeExpire 覆盖过期后新 episode（新 alarmID）。
func TestAlarmManagerEpisodeExpire(t *testing.T) {
	d := &fakeDispatch{}
	m := newTestAlarmManager(t, d, nil)
	m.ObserveRuleEvent(testRuleEvent("rule-x", "info"))
	m.mu.Lock()
	firstID := m.active[m.activeKeyForTest("node-1|rule|rule-x|default|sensor-01")].a.AlarmID
	m.mu.Unlock()
	// 人为把 episode 推到过期窗口之外。
	m.mu.Lock()
	for _, ep := range m.active {
		ep.a.UpdatedAt = time.Now().UnixMilli() - (alarmEpisodeExpireSec+1)*1000
	}
	m.mu.Unlock()
	m.ObserveRuleEvent(testRuleEvent("rule-x", "info"))
	d.mu.Lock()
	count := len(d.sent)
	d.mu.Unlock()
	if count != 2 {
		t.Fatalf("过期后新触发应重发，实际 %d", count)
	}
	m.mu.Lock()
	var secondID string
	for _, ep := range m.active {
		secondID = ep.a.AlarmID
	}
	m.mu.Unlock()
	if secondID == firstID {
		t.Fatalf("过期后应生成新 alarmID")
	}
	if secondID == "" || firstID == "" {
		t.Fatalf("alarmID 不应为空")
	}
}

// activeKeyForTest 测试辅助：直接返回 key（Observe 内部以 DedupKey 为键）。
func (m *alarmManager) activeKeyForTest(key string) string { return key }

// TestAlarmManagerStop 覆盖停止后空转。
func TestAlarmManagerStop(t *testing.T) {
	d := &fakeDispatch{}
	m := newTestAlarmManager(t, d, nil)
	m.Stop()
	m.ObserveRuleEvent(testRuleEvent("rule-z", "info"))
	if d.count() != 0 {
		t.Fatalf("停止后不应上行")
	}
}

// TestHandleSetpointAccepted 覆盖设定值回告：缓存 + 回告 / 普通指令零变化 / 失败不回告。
func TestHandleSetpointAccepted(t *testing.T) {
	d := &fakeDispatch{}
	spCtx := &setpointContext{nodeID: "node-1", dispatch: d.send}

	mkMsg := func(t *testing.T, class, spID string) *protocol.Message {
		t.Helper()
		msg, err := protocol.NewMessage(protocol.TypeDeviceCommand, "cloud", "node-1",
			devicetwin.DeviceCommandPayload{
				DeviceName: "sensor-01", Namespace: "default", Property: "targetTemp",
				Value: 25.5, Class: class, SetpointID: spID,
			})
		if err != nil {
			t.Fatalf("构造消息失败: %v", err)
		}
		return msg
	}

	// 设定值：缓存 + 回告。
	handleSetpointAccepted(spCtx, mkMsg(t, "setpoint", "sp-1"), nil)
	if d.count() != 1 {
		t.Fatalf("设定值应回告 1 条，实际 %d", d.count())
	}
	d.mu.Lock()
	res := d.sent[0]
	d.mu.Unlock()
	if res.msg.Type != protocol.TypeSetpointResult {
		t.Fatalf("回告类型 = %s", res.msg.Type)
	}
	var payload setpointResultPayload
	if err := json.Unmarshal(res.msg.Payload, &payload); err != nil {
		t.Fatalf("解析回告失败: %v", err)
	}
	if payload.SetpointID != "sp-1" || !payload.OK {
		t.Fatalf("回告内容不符: %+v", payload)
	}

	// 普通指令（无 class/setpointId）：零变化。
	handleSetpointAccepted(spCtx, mkMsg(t, "", ""), nil)
	if d.count() != 1 {
		t.Fatalf("普通指令不应回告")
	}

	// 受理失败（执行错误）：不回告（由 Ack error 路径闭环）。
	handleSetpointAccepted(spCtx, mkMsg(t, "setpoint", "sp-2"), errFakeExec)
	if d.count() != 1 {
		t.Fatalf("受理失败不应回告")
	}

	// nil ctx：安全空转。
	handleSetpointAccepted(nil, mkMsg(t, "setpoint", "sp-3"), nil)
}

// errFakeExec 是执行失败的哨兵错误。
var errFakeExec = &fakeExecError{}

type fakeExecError struct{}

func (*fakeExecError) Error() string { return "exec failed" }

// TestNewAlarmIDUnique 覆盖 ID 唯一性。
func TestNewAlarmIDUnique(t *testing.T) {
	seen := map[string]bool{}
	now := time.Now().UnixMilli()
	for i := 0; i < 100; i++ {
		id := newAlarmID(now)
		if seen[id] {
			t.Fatalf("alarmID 重复: %s", id)
		}
		seen[id] = true
	}
}
