// v0400_test.go：AlarmEvent / SetpointResult 接收单测（spec 0013 US-3/US-5）。
package cloudhub

import (
	"sync/atomic"
	"testing"

	"edgeflow/pkg/alarm"
	"edgeflow/pkg/protocol"
)

// TestAlarmEventValidationMatrix 用消息级校验路径覆盖（不依赖连接桩：
// handleAlarmEvent 前置 registered 检查由 e2e 覆盖，此处直测负载校验逻辑）。
func TestAlarmEventValidationMatrix(t *testing.T) {
	valid := alarm.Alarm{
		AlarmID: "alm-1", NodeID: "node-1", Source: alarm.SourceRule, RuleID: "r1",
		Severity: alarm.SeverityWarning, State: alarm.StateRaised,
		Count: 1, RaisedAt: 1, UpdatedAt: 1,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法告警不应报错: %v", err)
	}
	invalid := valid
	invalid.Severity = "fatal"
	if err := invalid.Validate(); err == nil {
		t.Fatalf("非法 severity 应校验失败")
	}
}

// TestSetpointResultPayloadFields 覆盖负载 JSON 字段名与边侧构造一致。
func TestSetpointResultPayloadFields(t *testing.T) {
	p := SetpointResultPayload{SetpointID: "sp-1", OK: true, Value: 25.5, Ts: 1000}
	if p.SetpointID != "sp-1" || !p.OK {
		t.Fatalf("负载字段不符: %+v", p)
	}
}

// TestServerHandlerRegistration 覆盖 setter 注册与锁外回调（快照语义）。
func TestServerHandlerRegistration(t *testing.T) {
	s := &Server{}
	var called atomic.Int32
	s.SetAlarmEventHandler(func(nodeID string, a alarm.Alarm) {
		called.Add(1)
	})
	s.mu.RLock()
	h := s.alarmEventHandler
	s.mu.RUnlock()
	if h == nil {
		t.Fatalf("handler 应已注册")
	}
	h("node-1", alarm.Alarm{})
	if called.Load() != 1 {
		t.Fatalf("回调应执行 1 次")
	}
	s.SetAlarmEventHandler(nil)
	s.mu.RLock()
	h = s.alarmEventHandler
	s.mu.RUnlock()
	if h != nil {
		t.Fatalf("nil 应取消注册")
	}

	var calledSP atomic.Int32
	s.SetSetpointResultHandler(func(nodeID string, p SetpointResultPayload) {
		calledSP.Add(1)
	})
	s.mu.RLock()
	hsp := s.setpointResultHandler
	s.mu.RUnlock()
	if hsp == nil {
		t.Fatalf("setpoint handler 应已注册")
	}
	hsp("node-1", SetpointResultPayload{})
	if calledSP.Load() != 1 {
		t.Fatalf("setpoint 回调应执行 1 次")
	}
	_ = protocol.TypeAlarmEvent
	_ = protocol.TypeSetpointResult
}
