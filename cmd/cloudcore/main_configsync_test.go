package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/registry"
	"edgeflow/pkg/protocol"
)

// newConfigSyncServer 构造注册了 POST /api/v1/nodes/{nodeID}/config-sync
// 路由的测试服务，并注入 fake reliableSend（与 newPodSyncServer 同方案：
// 无需真实 WebSocket 连接与 Ack 往返即可覆盖各错误路径）。
func newConfigSyncServer(t *testing.T, send func(ctx context.Context, nodeID string, msg *protocol.Message, opts cloudhub.ReliableOptions) error) *httptest.Server {
	t.Helper()
	api := &nodeAPI{reg: registry.New(), reliableSend: send}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/nodes/{nodeID}/config-sync", api.syncConfig)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// postConfigSync 向测试服务发送一条 config-sync 请求，返回响应。
func postConfigSync(t *testing.T, srv *httptest.Server, nodeID, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/v1/nodes/"+nodeID+"/config-sync", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST config-sync 失败: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// validConfigBody 是合法的 add 请求体（测试基座）。
const validConfigBody = `{"operation":"add","config":{"name":"app-config","namespace":"default","kind":"ConfigMap","data":{"key1":"value1"}}}`

// TestSyncConfigBadJSON 验证请求体不是合法 JSON 时返回 400。
func TestSyncConfigBadJSON(t *testing.T) {
	srv := newConfigSyncServer(t, func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		t.Error("坏 JSON 不应触发可靠投递")
		return nil
	})
	resp := postConfigSync(t, srv, "node-1", `{"operation":`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "invalid json body") {
		t.Errorf("400 响应文案不符: %s", body)
	}
}

// TestSyncConfigMissingFields 验证缺 operation 或 config.name 时返回 400。
func TestSyncConfigMissingFields(t *testing.T) {
	srv := newConfigSyncServer(t, func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		t.Error("缺字段不应触发可靠投递")
		return nil
	})

	// 缺 operation
	resp := postConfigSync(t, srv, "node-1", `{"config":{"name":"app-config","kind":"ConfigMap","data":{"k":"v"}}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 operation 状态码 = %d，期望 400", resp.StatusCode)
	}
	// 缺 config.name
	resp = postConfigSync(t, srv, "node-1", `{"operation":"add","config":{"namespace":"default","kind":"ConfigMap","data":{"k":"v"}}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 config.name 状态码 = %d，期望 400", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "required") {
		t.Errorf("400 响应文案不符: %s", body)
	}
}

// TestSyncConfigInvalidOperation 验证 operation 取值不在 {add,update,delete}
// 内时直接返回 400（云端前置校验，省一轮可靠投递往返）。
func TestSyncConfigInvalidOperation(t *testing.T) {
	for _, op := range []string{"create", "DELETE", "", "upsert"} {
		srv := newConfigSyncServer(t, func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
			t.Error("非法 operation 不应触发可靠投递")
			return nil
		})
		body := `{"operation":"` + op + `","config":{"name":"app-config","kind":"ConfigMap","data":{"k":"v"}}}`
		resp := postConfigSync(t, srv, "node-1", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("operation=%q 状态码 = %d，期望 400", op, resp.StatusCode)
		}
	}
}

// TestSyncConfigInvalidKind 验证 kind 不在 {ConfigMap,Secret} 白名单内时返回 400
// （契约约束：防止未知类型数据落到边缘）。
func TestSyncConfigInvalidKind(t *testing.T) {
	for _, kind := range []string{"configmap", "SecretV2", "", "TLS"} {
		srv := newConfigSyncServer(t, func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
			t.Error("非法 kind 不应触发可靠投递")
			return nil
		})
		body := `{"operation":"add","config":{"name":"app-config","kind":"` + kind + `","data":{"k":"v"}}}`
		resp := postConfigSync(t, srv, "node-1", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("kind=%q 状态码 = %d，期望 400", kind, resp.StatusCode)
		}
		if body := readBody(t, resp); !strings.Contains(body, "invalid kind") {
			t.Errorf("400 响应文案不符: %s", body)
		}
	}
}

// TestSyncConfigEmptyData 验证 add/update 时 data 为空返回 400
// （delete 不需要 data，按 name 删除）。
func TestSyncConfigEmptyData(t *testing.T) {
	// add + 空 data → 400
	srv := newConfigSyncServer(t, func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		t.Error("空 data 不应触发可靠投递")
		return nil
	})
	resp := postConfigSync(t, srv, "node-1", `{"operation":"add","config":{"name":"app-config","kind":"ConfigMap","data":{}}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("add+空 data 状态码 = %d，期望 400", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "data is required") {
		t.Errorf("400 响应文案不符: %s", body)
	}

	// delete + 空 data → 允许（200，fake send 成功）
	srv = newConfigSyncServer(t, func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return nil
	})
	resp = postConfigSync(t, srv, "node-1", `{"operation":"delete","config":{"name":"app-config","namespace":"default","kind":"ConfigMap","data":{}}}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("delete+空 data 状态码 = %d，期望 200（delete 不校验 data）", resp.StatusCode)
	}
}

// TestSyncConfigNodeOffline 验证节点离线/未注册（ErrNodeOffline）时返回 404。
func TestSyncConfigNodeOffline(t *testing.T) {
	srv := newConfigSyncServer(t, func(_ context.Context, nodeID string, _ *protocol.Message, _ cloudhub.ReliableOptions) error {
		if nodeID != "node-1" {
			t.Errorf("reliableSend 收到的 nodeID = %q，期望 node-1", nodeID)
		}
		return cloudhub.ErrNodeOffline
	})
	resp := postConfigSync(t, srv, "node-1", validConfigBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "node offline") {
		t.Errorf("404 响应文案不符: %s", body)
	}
}

// TestSyncConfigAckTimeout 验证确认超时重试耗尽（ErrAckTimeout）时返回 504。
func TestSyncConfigAckTimeout(t *testing.T) {
	srv := newConfigSyncServer(t, func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return cloudhub.ErrAckTimeout
	})
	resp := postConfigSync(t, srv, "node-1", validConfigBody)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("状态码 = %d，期望 504", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "ack timeout") {
		t.Errorf("504 响应文案不符: %s", body)
	}
}

// TestSyncConfigAckFailed 验证边缘明确回 error Ack（ErrAckFailed）时返回 502
// （消息已送达但被拒绝，映射 Bad Gateway，区别于 404 未送达/504 超时）。
func TestSyncConfigAckFailed(t *testing.T) {
	srv := newConfigSyncServer(t, func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return cloudhub.ErrAckFailed
	})
	resp := postConfigSync(t, srv, "node-1", `{"operation":"delete","config":{"name":"app-config","kind":"ConfigMap"}}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "edge rejected ack") {
		t.Errorf("502 响应文案不符: %s", body)
	}
}

// TestSyncConfigOK 验证成功路径：返回 200 + acked:true，且下发的消息
// 信封与负载符合契约（Type=ConfigSync、Source=cloud、Target=节点 ID、payload 原样）。
func TestSyncConfigOK(t *testing.T) {
	var gotNodeID string
	var gotMsg *protocol.Message
	srv := newConfigSyncServer(t, func(_ context.Context, nodeID string, msg *protocol.Message, _ cloudhub.ReliableOptions) error {
		gotNodeID = nodeID
		gotMsg = msg
		return nil
	})

	resp := postConfigSync(t, srv, "node-1", validConfigBody)
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
	if gotMsg.Type != protocol.TypeConfigSync || gotMsg.Source != "cloud" || gotMsg.Target != "node-1" {
		t.Errorf("消息信封不符: type=%s source=%s target=%s",
			gotMsg.Type, gotMsg.Source, gotMsg.Target)
	}
	var sent configSyncRequest
	if err := json.Unmarshal(gotMsg.Payload, &sent); err != nil {
		t.Fatalf("解析下发的 payload 失败: %v", err)
	}
	if sent.Operation != "add" || sent.Config.Name != "app-config" ||
		sent.Config.Namespace != "default" || sent.Config.Kind != "ConfigMap" ||
		sent.Config.Data["key1"] != "value1" {
		t.Errorf("下发的 payload 与原请求不符: %+v", sent)
	}
}

// TestSyncConfigUnexpectedError 验证非契约错误（未知错误）落入 500 兜底分支。
func TestSyncConfigUnexpectedError(t *testing.T) {
	srv := newConfigSyncServer(t, func(context.Context, string, *protocol.Message, cloudhub.ReliableOptions) error {
		return errors.New("some unexpected error")
	})
	resp := postConfigSync(t, srv, "node-1", validConfigBody)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "send failed") {
		t.Errorf("500 响应文案不符: %s", body)
	}
}
