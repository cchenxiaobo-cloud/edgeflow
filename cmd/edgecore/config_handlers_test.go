package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/protocol"
)

// newConfigSyncStore 打开一个临时 SQLite 库供 handleConfigSync 测试使用
// （真实 Store，测后自动清理；与 handlePodSync 测试同方案）。
func newConfigSyncStore(t *testing.T) *metamanager.Store {
	t.Helper()
	s, err := metamanager.Open(filepath.Join(t.TempDir(), "edgeflow.db"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// configSyncMsg 构造一条 ConfigSync 下发消息（负载 = operation + config JSON）。
func configSyncMsg(t *testing.T, operation, configJSON string) *protocol.Message {
	t.Helper()
	msg, err := protocol.NewMessage(protocol.TypeConfigSync, "cloud", "edge-1", ConfigSyncPayload{
		Operation: operation,
		Config:    json.RawMessage(configJSON),
	})
	if err != nil {
		t.Fatalf("NewMessage 失败: %v", err)
	}
	return msg
}

// TestHandleConfigSyncAdd 验证 add：config JSON 原样落盘，返回 nil。
func TestHandleConfigSyncAdd(t *testing.T) {
	s := newConfigSyncStore(t)
	cfg := `{"name":"app-config","namespace":"default","kind":"ConfigMap","data":{"key1":"value1"}}`

	if err := handleConfigSync(s, configSyncMsg(t, "add", cfg)); err != nil {
		t.Fatalf("handleConfigSync(add) 失败: %v", err)
	}
	configs, err := s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 1 || configs[0] != cfg {
		t.Errorf("add 后 ListConfigs = %v，期望 [%s]（原样落盘）", configs, cfg)
	}
}

// TestHandleConfigSyncUpdate 验证 update：同命名空间同名配置覆盖更新，不新增记录。
func TestHandleConfigSyncUpdate(t *testing.T) {
	s := newConfigSyncStore(t)
	if err := handleConfigSync(s, configSyncMsg(t, "add", `{"name":"app-config","namespace":"default","kind":"ConfigMap","data":{"key1":"value1"}}`)); err != nil {
		t.Fatalf("handleConfigSync(add) 失败: %v", err)
	}
	updated := `{"name":"app-config","namespace":"default","kind":"ConfigMap","data":{"key1":"value2","key2":"value2"}}`
	if err := handleConfigSync(s, configSyncMsg(t, "update", updated)); err != nil {
		t.Fatalf("handleConfigSync(update) 失败: %v", err)
	}
	configs, err := s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("update 后 ListConfigs 长度 = %d，期望 1（同 key 覆盖不新增）", len(configs))
	}
	if configs[0] != updated {
		t.Errorf("update 后内容应为最新记录: %s", configs[0])
	}
}

// TestHandleConfigSyncSecret 验证 Secret 下发：value 按 base64 编码语义原样落盘
// （云端负责编码，边缘不做解码/改写）。
func TestHandleConfigSyncSecret(t *testing.T) {
	s := newConfigSyncStore(t)
	secret := `{"name":"app-secret","namespace":"default","kind":"Secret","data":{"password":"cGFzc3dvcmQ="}}`

	if err := handleConfigSync(s, configSyncMsg(t, "add", secret)); err != nil {
		t.Fatalf("handleConfigSync(add Secret) 失败: %v", err)
	}
	configs, err := s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 1 || configs[0] != secret {
		t.Errorf("Secret 应原样落盘（base64 语义）: %v", configs)
	}
}

// TestHandleConfigSyncDelete 验证 delete：按命名空间+名称删除，
// 同名但不同命名空间的配置不受影响；删除不存在的配置幂等成功。
func TestHandleConfigSyncDelete(t *testing.T) {
	s := newConfigSyncStore(t)
	if err := handleConfigSync(s, configSyncMsg(t, "add", `{"name":"app-config","namespace":"default","kind":"ConfigMap","data":{"k":"v"}}`)); err != nil {
		t.Fatalf("handleConfigSync(add default) 失败: %v", err)
	}
	if err := handleConfigSync(s, configSyncMsg(t, "add", `{"name":"app-config","namespace":"prod","kind":"ConfigMap","data":{"k":"v"}}`)); err != nil {
		t.Fatalf("handleConfigSync(add prod) 失败: %v", err)
	}

	// 只删 default/app-config，prod/app-config 应保留
	if err := handleConfigSync(s, configSyncMsg(t, "delete", `{"name":"app-config","namespace":"default"}`)); err != nil {
		t.Fatalf("handleConfigSync(delete default) 失败: %v", err)
	}
	configs, err := s.ListConfigs()
	if err != nil {
		t.Fatalf("ListConfigs 失败: %v", err)
	}
	if len(configs) != 1 || !strings.Contains(configs[0], `"namespace":"prod"`) {
		t.Errorf("删除 default/app-config 后 ListConfigs = %v，期望只剩 prod/app-config", configs)
	}

	// 删除不存在的配置：幂等成功
	if err := handleConfigSync(s, configSyncMsg(t, "delete", `{"name":"app-config","namespace":"default"}`)); err != nil {
		t.Errorf("重复 delete 期望幂等成功，实际: %v", err)
	}
}

// TestHandleConfigSyncUnknownOperation 验证未知 operation：返回 error
// （与 handlePodSync 行为一致：EdgeHub 会据此回 Ack code=error，云端不再重试同 ID）。
func TestHandleConfigSyncUnknownOperation(t *testing.T) {
	s := newConfigSyncStore(t)
	err := handleConfigSync(s, configSyncMsg(t, "scale", `{"name":"app-config","namespace":"default","kind":"ConfigMap"}`))
	if err == nil {
		t.Fatal("未知 operation 期望报错，实际成功")
	}
	if !strings.Contains(err.Error(), "未知的 ConfigSync operation") {
		t.Errorf("错误文案不符: %v", err)
	}
	// 未知 operation 不应落盘任何数据
	configs, lerr := s.ListConfigs()
	if lerr != nil {
		t.Fatalf("ListConfigs 失败: %v", lerr)
	}
	if len(configs) != 0 {
		t.Errorf("未知 operation 后 ListConfigs 长度 = %d，期望 0", len(configs))
	}
}

// TestHandleConfigSyncBadPayload 验证坏 payload：返回 error（回 Ack code=error）。
func TestHandleConfigSyncBadPayload(t *testing.T) {
	s := newConfigSyncStore(t)
	// 手工构造 Payload 为非法 JSON 的消息（绕过 NewMessage 的序列化）
	msg := &protocol.Message{Type: protocol.TypeConfigSync, Payload: json.RawMessage(`{"operation":`)}
	if err := handleConfigSync(s, msg); err == nil {
		t.Fatal("坏 payload 期望报错，实际成功")
	}
}

// TestHandleConfigSyncDeleteMissingName 验证 delete 时 config 缺少 name：返回 error。
func TestHandleConfigSyncDeleteMissingName(t *testing.T) {
	s := newConfigSyncStore(t)
	err := handleConfigSync(s, configSyncMsg(t, "delete", `{"namespace":"default"}`))
	if err == nil {
		t.Fatal("delete 缺 name 期望报错，实际成功")
	}
	if !strings.Contains(err.Error(), "缺少 name") {
		t.Errorf("错误文案不符: %v", err)
	}
}
