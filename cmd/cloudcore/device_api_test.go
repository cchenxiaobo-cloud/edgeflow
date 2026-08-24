// 设备状态查询与指令下发 API（WBS 5.3/2.2）测试：
// 列表语义（全量/单节点/空数组/404）与指令下发各错误路径（仿 podsync 测试模式）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/devicestatus"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/protocol"
)

// newDeviceAPIServer 构造带设备链路路由的测试服务
// （/api/v1/devices、/api/v1/nodes/{nodeID}/devices、device-command），
// 并注入 fake reliableSend（替代真实 CloudHub，无需 WebSocket 即可
// 覆盖各错误路径）。
func newDeviceAPIServer(t *testing.T, reg *registry.Registry, devices devicestatus.Store, send func(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error) *httptest.Server {
	t.Helper()
	api := &nodeAPI{reg: reg, devices: devices, reliableSend: send}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/devices", api.listDevices)
	mux.HandleFunc("GET /api/v1/nodes/{nodeID}/devices", api.listNodeDevices)
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/device-command", api.sendDeviceCommand)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// decodeDeviceStatusList 解析设备状态列表响应。
func decodeDeviceStatusList(t *testing.T, resp *http.Response) deviceStatusList {
	t.Helper()
	var list deviceStatusList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	return list
}

// postDeviceCommand 向测试服务发送一条设备指令请求，返回响应。
func postDeviceCommand(t *testing.T, srv *httptest.Server, nodeID, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/v1/nodes/"+nodeID+"/device-command", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST device-command 失败: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestDeviceCommandBadJSON 验证请求体不是合法 JSON 时返回 400。
func TestDeviceCommandBadJSON(t *testing.T) {
	srv := newDeviceAPIServer(t, registry.New(), devicestatus.NewStore(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		t.Error("坏 JSON 不应触发可靠投递")
		return nil
	})
	resp := postDeviceCommand(t, srv, "node-1", `{"deviceName":`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "invalid json body") {
		t.Errorf("400 响应文案不符: %s", body)
	}
}

// TestDeviceCommandMissingFields 验证缺 deviceName 或 property 时返回 400。
func TestDeviceCommandMissingFields(t *testing.T) {
	srv := newDeviceAPIServer(t, registry.New(), devicestatus.NewStore(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		t.Error("缺字段不应触发可靠投递")
		return nil
	})

	// 缺 deviceName
	resp := postDeviceCommand(t, srv, "node-1", `{"namespace":"default","property":"targetTemp","value":25}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 deviceName 状态码 = %d，期望 400", resp.StatusCode)
	}
	// 缺 property
	resp = postDeviceCommand(t, srv, "node-1", `{"deviceName":"sensor-01","value":25}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 property 状态码 = %d，期望 400", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "required") {
		t.Errorf("400 响应文案不符: %s", body)
	}
}

// TestDeviceCommandNodeOffline 验证节点离线/未注册（ErrNodeOffline）时返回 404。
func TestDeviceCommandNodeOffline(t *testing.T) {
	srv := newDeviceAPIServer(t, registry.New(), devicestatus.NewStore(), func(_ context.Context, nodeID string, _ *protocol.Message, _ cloudhub.ReliableOptions) error {
		if nodeID != "node-1" {
			t.Errorf("reliableSend 收到的 nodeID = %q，期望 node-1", nodeID)
		}
		return cloudhub.ErrNodeOffline
	})
	resp := postDeviceCommand(t, srv, "node-1", `{"deviceName":"sensor-01","property":"targetTemp","value":25}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "node offline") {
		t.Errorf("404 响应文案不符: %s", body)
	}
}

// TestDeviceCommandAckTimeout 验证确认超时重试耗尽（ErrAckTimeout）时返回 504。
func TestDeviceCommandAckTimeout(t *testing.T) {
	srv := newDeviceAPIServer(t, registry.New(), devicestatus.NewStore(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return cloudhub.ErrAckTimeout
	})
	resp := postDeviceCommand(t, srv, "node-1", `{"deviceName":"sensor-01","property":"targetTemp","value":25}`)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("状态码 = %d，期望 504", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "ack timeout") {
		t.Errorf("504 响应文案不符: %s", body)
	}
}

// TestDeviceCommandAckFailed 验证边缘明确回 error Ack（ErrAckFailed）时
// 返回 502（指令已送达但执行失败，区别于 404 未送达/504 超时）。
func TestDeviceCommandAckFailed(t *testing.T) {
	srv := newDeviceAPIServer(t, registry.New(), devicestatus.NewStore(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return cloudhub.ErrAckFailed
	})
	resp := postDeviceCommand(t, srv, "node-1", `{"deviceName":"sensor-01","property":"targetTemp","value":25}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "edge rejected ack") {
		t.Errorf("502 响应文案不符: %s", body)
	}
}

// TestDeviceCommandUnexpectedError 验证非契约错误落入 500 兜底分支。
func TestDeviceCommandUnexpectedError(t *testing.T) {
	srv := newDeviceAPIServer(t, registry.New(), devicestatus.NewStore(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return errors.New("some unexpected error")
	})
	resp := postDeviceCommand(t, srv, "node-1", `{"deviceName":"sensor-01","property":"targetTemp","value":25}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "send failed") {
		t.Errorf("500 响应文案不符: %s", body)
	}
}

// TestDeviceCommandOK 验证成功路径：返回 200 + acked:true；下发的消息
// 信封与负载符合契约（Type=DeviceCommand、Source=cloud、Target=节点 ID、
// payload 字段原样）；期望值写入云端设备状态存储。
func TestDeviceCommandOK(t *testing.T) {
	var gotNodeID string
	var gotMsg *protocol.Message
	devices := devicestatus.NewStore()
	srv := newDeviceAPIServer(t, registry.New(), devices, func(_ context.Context, nodeID string, msg *protocol.Message, _ cloudhub.ReliableOptions) error {
		gotNodeID = nodeID
		gotMsg = msg
		return nil
	})

	body := `{"deviceName":"sensor-01","namespace":"default","property":"targetTemp","value":25}`
	resp := postDeviceCommand(t, srv, "node-1", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q，期望 application/json", ct)
	}
	if got := readBody(t, resp); got != `{"status":"ok","acked":true}` {
		t.Errorf("响应体 = %q", got)
	}

	// 校验下发的消息参数
	if gotNodeID != "node-1" {
		t.Errorf("reliableSend 收到的 nodeID = %q，期望 node-1", gotNodeID)
	}
	if gotMsg == nil {
		t.Fatal("reliableSend 未收到消息")
	}
	if gotMsg.Type != protocol.TypeDeviceCommand || gotMsg.Source != "cloud" || gotMsg.Target != "node-1" {
		t.Errorf("消息信封不符: type=%s source=%s target=%s",
			gotMsg.Type, gotMsg.Source, gotMsg.Target)
	}
	var sent deviceCommandRequest
	if err := json.Unmarshal(gotMsg.Payload, &sent); err != nil {
		t.Fatalf("解析下发的 payload 失败: %v", err)
	}
	if sent.DeviceName != "sensor-01" || sent.Namespace != "default" ||
		sent.Property != "targetTemp" || sent.Value != 25 {
		t.Errorf("下发的 payload 与原请求不符: %+v", sent)
	}

	// 期望值已写入云端存储（下发成功 = 边缘确认）
	got, ok := devices.Get("node-1", "default", "sensor-01")
	if !ok || got.Desired["targetTemp"] != 25 {
		t.Errorf("期望值应写入设备状态存储: ok=%v %+v", ok, got)
	}
	if got.NodeID != "node-1" {
		t.Errorf("NodeID = %q，期望 node-1", got.NodeID)
	}
}

// TestDeviceCommandOKNilStore 验证存储未注入时成功路径仍返回 200（不 panic）。
func TestDeviceCommandOKNilStore(t *testing.T) {
	srv := newDeviceAPIServer(t, registry.New(), devicestatus.Store(nil), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return nil
	})
	resp := postDeviceCommand(t, srv, "node-1", `{"deviceName":"sensor-01","property":"targetTemp","value":25}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
}

// TestDeviceAPIList 验证 GET /api/v1/devices：K8s List 风格包装、
// 按 nodeID/namespace/deviceName 排序、字段按契约透出。
func TestDeviceAPIList(t *testing.T) {
	devices := devicestatus.NewStore()
	devices.Upsert("edge-002", devicestatus.DeviceStatus{DeviceName: "b", Namespace: "default",
		Properties: map[string]float64{"temperature": 25.5}, LastReportedAt: 1755168000000})
	devices.Upsert("edge-001", devicestatus.DeviceStatus{DeviceName: "a", Namespace: "zone-x",
		Properties: map[string]float64{"p": 2}})
	devices.Upsert("edge-001", devicestatus.DeviceStatus{DeviceName: "a", Namespace: "default",
		Properties: map[string]float64{"p": 3}})
	devices.SetDesired("edge-001", "default", "a", "targetTemp", 25)

	srv := newDeviceAPIServer(t, registry.New(), devices, nil)
	resp, err := http.Get(srv.URL + "/api/v1/devices")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}

	list := decodeDeviceStatusList(t, resp)
	if list.Kind != "DeviceStatusList" || list.APIVersion != "v1" {
		t.Errorf("kind/apiVersion 不符: %+v", list)
	}
	if len(list.Items) != 3 {
		t.Fatalf("条目数 = %d，期望 3", len(list.Items))
	}
	// 排序：nodeID → namespace → deviceName
	expect := []string{"edge-001/default/a", "edge-001/zone-x/a", "edge-002/default/b"}
	for i, e := range expect {
		it := list.Items[i]
		got := it.NodeID + "/" + it.Namespace + "/" + it.DeviceName
		if got != e {
			t.Errorf("items[%d] = %q，期望 %q", i, got, e)
		}
	}
	// 契约字段完整透出（含 desired 与 properties）
	it := list.Items[0]
	if it.Desired["targetTemp"] != 25 || it.Properties["p"] != 3 {
		t.Errorf("JSON 字段未按契约透出: %+v", it)
	}
	it = list.Items[2]
	if it.LastReportedAt != 1755168000000 || it.Properties["temperature"] != 25.5 {
		t.Errorf("JSON 字段未按契约透出: %+v", it)
	}
}

// TestDeviceAPIEmptyArray 验证无数据时 items 为 [] 而非 null。
func TestDeviceAPIEmptyArray(t *testing.T) {
	srv := newDeviceAPIServer(t, registry.New(), devicestatus.NewStore(), nil)

	resp, err := http.Get(srv.URL + "/api/v1/devices")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	raw := strings.TrimSpace(string(rawBytes))
	if !strings.Contains(raw, `"items":[]`) {
		t.Errorf("空数据 items 应为 []，实际响应: %s", raw)
	}
	if strings.Contains(raw, "null") {
		t.Errorf("空数据响应不应含 null: %s", raw)
	}
	var list deviceStatusList
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if list.Items == nil {
		t.Error("items 应为非 nil 空切片")
	}
}

// TestDeviceAPINodeDevices 验证 GET /api/v1/nodes/{nodeID}/devices 三态语义：
//   - 节点存在且有设备 → 200 + 条目
//   - 节点存在但无设备 → 200 + 空数组（不是 404）
//   - 节点不存在（从未注册）→ 404
func TestDeviceAPINodeDevices(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.NodeInfo{NodeID: "edge-001"})
	reg.Register(registry.NodeInfo{NodeID: "edge-002"})
	devices := devicestatus.NewStore()
	devices.Upsert("edge-001", devicestatus.DeviceStatus{DeviceName: "sensor-01", Namespace: "default",
		Properties: map[string]float64{"temperature": 25.5}})

	srv := newDeviceAPIServer(t, reg, devices, nil)

	// 有设备的节点：200 + 条目
	resp, err := http.Get(srv.URL + "/api/v1/nodes/edge-001/devices")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	func() {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("有设备节点状态码 = %d，期望 200", resp.StatusCode)
		}
		list := decodeDeviceStatusList(t, resp)
		if len(list.Items) != 1 || list.Items[0].DeviceName != "sensor-01" {
			t.Errorf("条目不符: %+v", list.Items)
		}
	}()

	// 无设备的已注册节点：200 + 空数组
	resp, err = http.Get(srv.URL + "/api/v1/nodes/edge-002/devices")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	func() {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("无设备节点状态码 = %d，期望 200（空数组而非 404）", resp.StatusCode)
		}
		rawBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读取响应失败: %v", err)
		}
		if !strings.Contains(string(rawBytes), `"items":[]`) {
			t.Errorf("无设备节点 items 应为 []，实际: %s", string(rawBytes))
		}
	}()

	// 未知节点：404
	resp, err = http.Get(srv.URL + "/api/v1/nodes/nope/devices")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知节点状态码 = %d，期望 404", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析 404 响应失败: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("404 响应应含错误信息: %v", body)
	}
}

// TestDeviceCommandBodyTooLarge 验证 P2-5：device-command 请求体超过
// 1MiB 上限时返回 413（与 syncPod 共用 decodeWriteBody 的大小限制）。
func TestDeviceCommandBodyTooLarge(t *testing.T) {
	srv := newDeviceAPIServer(t, registry.New(), devicestatus.NewStore(), func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		t.Error("超大请求体不应触发可靠投递")
		return nil
	})
	body := `{"deviceName":"sensor-1","namespace":"default","property":"temp","pad":"` +
		strings.Repeat("x", maxWriteBodyBytes) + `"}`
	resp := postDeviceCommand(t, srv, "node-1", body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "too large") {
		t.Errorf("413 响应文案不符: %s", body)
	}
}
