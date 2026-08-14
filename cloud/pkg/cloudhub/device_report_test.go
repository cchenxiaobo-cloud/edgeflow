// CloudHub DeviceReport 接收与分发（WBS 5.3）测试：
// 已注册节点上报 → 校验通过 → 注入的回调被调用；
// 未注册/缺 deviceName/坏 payload → 拒绝且不触发回调。
package cloudhub

import (
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/protocol"

	"github.com/gorilla/websocket"
)

// fakeDeviceReportHandler 是记录型 DeviceReport 回调（并发安全），用于断言触发内容。
type fakeDeviceReportHandler struct {
	mu    sync.Mutex
	nodes []string
	calls []DeviceReportPayload
}

func (f *fakeDeviceReportHandler) handle(nodeID string, dr DeviceReportPayload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes = append(f.nodes, nodeID)
	f.calls = append(f.calls, dr)
}

func (f *fakeDeviceReportHandler) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDeviceReportHandler) first() DeviceReportPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return DeviceReportPayload{}
	}
	return f.calls[0]
}

// TestDeviceReportHandlerInvoked 验证链路：已注册节点上报 DeviceReport →
// 校验通过 → 注入的回调被调用（携带 Source 作为 nodeID，契约字段原样透出）。
func TestDeviceReportHandlerInvoked(t *testing.T) {
	srv, wsURL := newTestServer(t)
	handler := &fakeDeviceReportHandler{}
	srv.SetDeviceReportHandler(handler.handle)

	ws := dial(t, wsURL)
	const nodeID = "edge-dev-1"
	register(t, ws, nodeID)

	// 契约示例：properties 是 map[string]float64，reportedAt 为毫秒时间戳
	msg, err := protocol.NewMessage(protocol.TypeDeviceReport, nodeID, "cloud",
		DeviceReportPayload{
			DeviceName: "sensor-01",
			Namespace:  "default",
			Properties: map[string]float64{"temperature": 25.5, "humidity": 60},
			ReportedAt: 1755168000000,
		})
	if err != nil {
		t.Fatalf("构造 DeviceReport 消息失败: %v", err)
	}
	sendMsg(t, ws, msg)

	waitFor(t, 3*time.Second, func() bool { return handler.count() == 1 }, "回调应被调用一次")
	got := handler.first()
	if got.DeviceName != "sensor-01" || got.Namespace != "default" ||
		got.Properties["temperature"] != 25.5 || got.Properties["humidity"] != 60 ||
		got.ReportedAt != 1755168000000 {
		t.Errorf("回调内容不符: %+v", got)
	}
	handler.mu.Lock()
	gotNode := handler.nodes[0]
	handler.mu.Unlock()
	if gotNode != nodeID {
		t.Errorf("回调 nodeID = %q，期望取消息 Source %q", gotNode, nodeID)
	}
}

// TestDeviceReportRejectedCases 验证非法上报被拒绝且不触发回调：
//   - 未注册节点上报 → not_registered Ack
//   - payload 缺 deviceName → invalid_message Ack
//   - payload 无法解析 → invalid_message Ack
func TestDeviceReportRejectedCases(t *testing.T) {
	srv, wsURL := newTestServer(t)
	handler := &fakeDeviceReportHandler{}
	srv.SetDeviceReportHandler(handler.handle)

	// 场景 1：未注册连接直接上报
	wsUnregistered := dial(t, wsURL)
	m, err := protocol.NewMessage(protocol.TypeDeviceReport, "edge-noreg", "cloud",
		DeviceReportPayload{DeviceName: "sensor-01", Properties: map[string]float64{"temperature": 25.5}})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	sendMsg(t, wsUnregistered, m)
	ack := readMsg(t, wsUnregistered)
	if ack.Type != protocol.TypeAck {
		t.Fatalf("期望 Ack，收到 %s", ack.Type)
	}
	var payload AckPayload
	if err := ack.DecodePayload(&payload); err != nil || payload.Code != CodeNotRegistered {
		t.Errorf("期望 not_registered Ack，实际 %+v (err=%v)", payload, err)
	}

	// 场景 2：已注册节点上报缺 deviceName
	ws := dial(t, wsURL)
	register(t, ws, "edge-dev-2")
	m2, err := protocol.NewMessage(protocol.TypeDeviceReport, "edge-dev-2", "cloud",
		DeviceReportPayload{Properties: map[string]float64{"temperature": 25.5}})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	sendMsg(t, ws, m2)
	ack2 := readMsg(t, ws)
	if ack2.Type != protocol.TypeAck {
		t.Fatalf("期望 Ack，收到 %s", ack2.Type)
	}
	if err := ack2.DecodePayload(&payload); err != nil || payload.Code != CodeInvalidMessage {
		t.Errorf("期望 invalid_message Ack，实际 %+v (err=%v)", payload, err)
	}

	// 场景 3：payload 无法解析（信封合法、负载是 JSON 字符串而非对象）——
	// 直接写原始字节（sendMsg 会重新编码，无法表达非法负载）
	raw := []byte(`{"id":"m3","type":"DeviceReport","version":"v1","source":"edge-dev-2","target":"cloud","timestamp":1,"payload":"not-a-device-report"}`)
	if err := ws.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("设置写超时失败: %v", err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatalf("发送原始消息失败: %v", err)
	}
	ack3 := readMsg(t, ws)
	if err := ack3.DecodePayload(&payload); err != nil || payload.Code != CodeInvalidMessage {
		t.Errorf("期望 invalid_message Ack，实际 %+v (err=%v)", payload, err)
	}

	// 三个拒绝场景都不应触发回调
	if handler.count() != 0 {
		t.Errorf("非法上报不应触发回调，实际 %d 次", handler.count())
	}
}
