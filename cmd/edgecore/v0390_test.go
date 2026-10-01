// v0390_test.go：上行补传与状态上报单测（spec 0012 US-2/US-3/US-6）。
package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/protocol"
	"edgeflow/pkg/rules"
)

// openTestRelay 在临时目录构造队列 + 补传中继（send 注入 fake）。
func openTestRelay(t *testing.T, o uplinkOptions, send func(*protocol.Message) error) (*uplinkRelay, *metamanager.UplinkQueue) {
	t.Helper()
	st, err := metamanager.Open(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatalf("打开测试 Store 失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q, err := metamanager.NewUplinkQueue(st, o.MaxRows)
	if err != nil {
		t.Fatalf("创建测试队列失败: %v", err)
	}
	if o.Batch == 0 {
		o.Batch = defaultUplinkBatch
	}
	if o.ReportSec == 0 {
		o.ReportSec = defaultUplinkReportSec
	}
	return newUplinkRelay(q, send, o, "edge-test"), q
}

// testEventMsg 构造一条 RuleEvent 测试消息（经 sink 同一构造路径）。
func testEventMsg(t *testing.T, id string) *protocol.Message {
	t.Helper()
	msg, err := buildRuleEventMessage("edge-test", rules.Event{
		RuleID: "r1", DeviceName: "dev-1", Severity: "warning",
		TriggeredAt: 1000, Value: 1.5, Message: "test",
	})
	if err != nil {
		t.Fatalf("构造事件消息失败: %v", err)
	}
	msg.ID = id
	return msg
}

// TestUplinkPriorityForSeverity 验证严重级→优先级映射。
func TestUplinkPriorityForSeverity(t *testing.T) {
	cases := map[string]int{
		"critical": metamanager.UplinkPriorityHigh,
		"warning":  metamanager.UplinkPriorityNormal,
		"info":     metamanager.UplinkPriorityLow,
		"":         metamanager.UplinkPriorityLow,
		"other":    metamanager.UplinkPriorityLow,
	}
	for sev, want := range cases {
		if got := uplinkPriorityForSeverity(sev); got != want {
			t.Errorf("severity %q → %d，期望 %d", sev, got, want)
		}
	}
}

// TestParseUplinkOptionsFromEnv 验证开关与参数解析（默认/自定义/非法回退）。
func TestParseUplinkOptionsFromEnv(t *testing.T) {
	// 默认（全部清空）
	for _, k := range []string{envUplinkEnabled, envUplinkMaxRows, envUplinkBatch, envUplinkRate, envUplinkReportSec} {
		t.Setenv(k, "")
	}
	o := parseUplinkOptionsFromEnv()
	if o.Enabled || o.MaxRows != metamanager.DefaultUplinkMaxRows || o.Batch != defaultUplinkBatch ||
		o.Rate != defaultUplinkRate || o.ReportSec != defaultUplinkReportSec {
		t.Fatalf("默认解析不符: %+v", o)
	}
	// 自定义 + 越界回退
	t.Setenv(envUplinkEnabled, "on")
	t.Setenv(envUplinkMaxRows, "500")
	t.Setenv(envUplinkBatch, "8")
	t.Setenv(envUplinkRate, "0") // 0 = 不限速（合法）
	t.Setenv(envUplinkReportSec, "abc")
	o = parseUplinkOptionsFromEnv()
	if !o.Enabled || o.MaxRows != 500 || o.Batch != 8 || o.Rate != 0 || o.ReportSec != defaultUplinkReportSec {
		t.Fatalf("自定义解析不符: %+v", o)
	}
	// 非 on 值 = 关闭
	t.Setenv(envUplinkEnabled, "off")
	if parseUplinkOptionsFromEnv().Enabled {
		t.Fatal("off 应解析为关闭")
	}
}

// TestBuildUplinkReportMessage 验证上报消息构造与负载。
func TestBuildUplinkReportMessage(t *testing.T) {
	st := metamanager.UplinkStats{Total: 7, Sent: 3, Dropped: 2, OldestTs: 123456}
	msg, err := buildUplinkReportMessage("edge-x", st)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if msg.Type != protocol.TypeUplinkReport || msg.Source != "edge-x" || msg.Target != targetCloud {
		t.Fatalf("消息头不符: %+v", msg)
	}
	var p uplinkReportPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		t.Fatalf("解析负载失败: %v", err)
	}
	if p.Depth != 7 || p.Sent != 3 || p.Dropped != 2 || p.OldestTs != 123456 {
		t.Fatalf("负载不符: %+v", p)
	}
}

// TestRelayDrainSuccess 验证成功补传：按序发送 + 全部 Ack + 计数。
func TestRelayDrainSuccess(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	fake := func(m *protocol.Message) error {
		mu.Lock()
		sent = append(sent, m.ID)
		mu.Unlock()
		return nil
	}
	r, q := openTestRelay(t, uplinkOptions{}, fake)
	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := q.EnqueueUplink(metamanager.UplinkPriorityNormal, testEventMsg(t, id)); err != nil {
			t.Fatalf("入队失败: %v", err)
		}
	}
	r.drain()
	st, _ := q.UplinkDepth()
	if st.Total != 0 || st.Sent != 3 {
		t.Fatalf("补传后 Total=%d Sent=%d，期望 0/3", st.Total, st.Sent)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(sent, ",") != "m1,m2,m3" {
		t.Fatalf("发送序 = %v，期望 [m1 m2 m3]", sent)
	}
}

// TestRelayDrainOfflineStops 验证离线时本轮停止（不 Ack、不丢）。
func TestRelayDrainOfflineStops(t *testing.T) {
	calls := 0
	fake := func(*protocol.Message) error {
		calls++
		return errors.New("未连接到云端")
	}
	r, q := openTestRelay(t, uplinkOptions{}, fake)
	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := q.EnqueueUplink(metamanager.UplinkPriorityNormal, testEventMsg(t, id)); err != nil {
			t.Fatalf("入队失败: %v", err)
		}
	}
	r.drain()
	if calls != 1 {
		t.Fatalf("离线时应只尝试 1 条，实际 %d", calls)
	}
	st, _ := q.UplinkDepth()
	if st.Total != 3 || st.Sent != 0 {
		t.Fatalf("离线后 Total=%d Sent=%d，期望 3/0（不丢）", st.Total, st.Sent)
	}
	// 恢复：换成功 send 后重跑，全部补传。
	r.send = func(*protocol.Message) error { return nil }
	r.drain()
	st2, _ := q.UplinkDepth()
	if st2.Total != 0 || st2.Sent != 3 {
		t.Fatalf("恢复后 Total=%d Sent=%d，期望 0/3", st2.Total, st2.Sent)
	}
}

// TestRelayRateLimit 验证速率预算节流（rate=20 → 3 条至少 ~100ms）。
func TestRelayRateLimit(t *testing.T) {
	r, q := openTestRelay(t, uplinkOptions{Rate: 20}, func(*protocol.Message) error { return nil })
	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := q.EnqueueUplink(metamanager.UplinkPriorityNormal, testEventMsg(t, id)); err != nil {
			t.Fatalf("入队失败: %v", err)
		}
	}
	start := time.Now()
	r.drain()
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("限速失效：3 条 @20/s 耗时 %v（期望 ≥80ms）", elapsed)
	}
	st, _ := q.UplinkDepth()
	if st.Total != 0 {
		t.Fatalf("限速补传后应清空，实际 %d", st.Total)
	}
}

// TestRelayStartNotifyStop 验证 worker 生命周期：Start 后唤醒即补传，Stop 干净退出。
func TestRelayStartNotifyStop(t *testing.T) {
	r, q := openTestRelay(t, uplinkOptions{ReportSec: 3600}, func(*protocol.Message) error { return nil })
	r.Start()
	if _, err := q.EnqueueUplink(metamanager.UplinkPriorityHigh, testEventMsg(t, "wake-1")); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	r.Notify()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := q.UplinkDepth()
		if st.Sent == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待唤醒补传超时（Sent=%d）", st.Sent)
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.Stop() // 不挂即通过（内部 wg.Wait）
}

// TestUplinkSinkEnqueue 验证补传模式 sink：入队 + 优先级 + 唤醒。
func TestUplinkSinkEnqueue(t *testing.T) {
	r, q := openTestRelay(t, uplinkOptions{}, func(*protocol.Message) error { return nil })
	sink := newUplinkRuleEventSink(r, nil, nil, "edge-test")
	sink(rules.Event{RuleID: "r9", DeviceName: "d9", Severity: "critical", TriggeredAt: 42, Value: 7})
	st, _ := q.UplinkDepth()
	if st.Total != 1 || st.High != 1 {
		t.Fatalf("入队后 Total=%d High=%d，期望 1/1", st.Total, st.High)
	}
	if len(r.wake) != 1 {
		t.Fatalf("应有 1 个唤醒信号，实际 %d", len(r.wake))
	}
	items, _ := q.DequeueUplinkBatch(10)
	if len(items) != 1 || items[0].Msg.Type != protocol.TypeRuleEvent {
		t.Fatalf("队列条目不符: %+v", items)
	}
	var ev rules.Event
	if err := items[0].Msg.DecodePayload(&ev); err != nil || ev.RuleID != "r9" {
		t.Fatalf("事件负载不符: ev=%+v err=%v", ev, err)
	}
}

// TestUplinkSinkFallsBackOnEnqueueError 验证超限入队失败不阻塞（兜底路径）。
func TestUplinkSinkFallsBackOnEnqueueError(t *testing.T) {
	r, q := openTestRelay(t, uplinkOptions{}, func(*protocol.Message) error { return nil })
	sink := newUplinkRuleEventSink(r, nil, nil, "edge-test")
	// 超大事件（>64KB）：Enqueue 拒绝 → 兜底（client=nil 只 Warn）→ 队列空。
	sink(rules.Event{RuleID: "big", DeviceName: "d", Severity: "info", Message: strings.Repeat("x", metamanager.MaxUplinkMsgBytes)})
	st, _ := q.UplinkDepth()
	if st.Total != 0 {
		t.Fatalf("超限事件不应入队，实际 Total=%d", st.Total)
	}
}
