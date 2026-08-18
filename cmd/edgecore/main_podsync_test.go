package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/protocol"
)

// newPodSyncStore 打开一个临时 SQLite 库供 handlePodSync 测试使用
// （真实 Store，测后自动清理；与 metamanager 包测试同方案）。
func newPodSyncStore(t *testing.T) *metamanager.Store {
	t.Helper()
	s, err := metamanager.Open(filepath.Join(t.TempDir(), "edgeflow.db"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// podSyncMsg 构造一条 PodSync 下发消息（负载 = operation + pod JSON）。
func podSyncMsg(t *testing.T, operation, podJSON string) *protocol.Message {
	t.Helper()
	msg, err := protocol.NewMessage(protocol.TypePodSync, "cloud", "edge-1", PodSyncPayload{
		Operation: operation,
		Pod:       json.RawMessage(podJSON),
	})
	if err != nil {
		t.Fatalf("NewMessage 失败: %v", err)
	}
	return msg
}

// TestHandlePodSyncAdd 验证 add：Pod JSON 原样落盘，返回 nil。
func TestHandlePodSyncAdd(t *testing.T) {
	s := newPodSyncStore(t)
	pod := `{"name":"nginx","namespace":"default","image":"nginx:1.25","replicas":1}`

	if err := handlePodSync(s, podSyncMsg(t, "add", pod)); err != nil {
		t.Fatalf("handlePodSync(add) 失败: %v", err)
	}
	pods, err := s.ListPods()
	if err != nil {
		t.Fatalf("ListPods 失败: %v", err)
	}
	if len(pods) != 1 || pods[0] != pod {
		t.Errorf("add 后 ListPods = %v，期望 [%s]（原样落盘）", pods, pod)
	}
}

// TestHandlePodSyncUpdate 验证 update：同命名空间同名 Pod 覆盖更新，不新增记录。
func TestHandlePodSyncUpdate(t *testing.T) {
	s := newPodSyncStore(t)
	if err := handlePodSync(s, podSyncMsg(t, "add", `{"name":"nginx","namespace":"default","image":"nginx:1.25"}`)); err != nil {
		t.Fatalf("handlePodSync(add) 失败: %v", err)
	}
	updated := `{"name":"nginx","namespace":"default","image":"nginx:1.26","replicas":2}`
	if err := handlePodSync(s, podSyncMsg(t, "update", updated)); err != nil {
		t.Fatalf("handlePodSync(update) 失败: %v", err)
	}
	pods, err := s.ListPods()
	if err != nil {
		t.Fatalf("ListPods 失败: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("update 后 ListPods 长度 = %d，期望 1（同 key 覆盖不新增）", len(pods))
	}
	if pods[0] != updated {
		t.Errorf("update 后内容应为最新记录: %s", pods[0])
	}
}

// TestHandlePodSyncDelete 验证 delete：按命名空间+名称删除，
// 同名但不同命名空间的 Pod 不受影响；删除不存在的 Pod 幂等成功。
func TestHandlePodSyncDelete(t *testing.T) {
	s := newPodSyncStore(t)
	if err := handlePodSync(s, podSyncMsg(t, "add", `{"name":"nginx","namespace":"default","image":"nginx:1.25"}`)); err != nil {
		t.Fatalf("handlePodSync(add default) 失败: %v", err)
	}
	if err := handlePodSync(s, podSyncMsg(t, "add", `{"name":"nginx","namespace":"prod","image":"nginx:1.26"}`)); err != nil {
		t.Fatalf("handlePodSync(add prod) 失败: %v", err)
	}

	// 只删 default/nginx，prod/nginx 应保留
	if err := handlePodSync(s, podSyncMsg(t, "delete", `{"name":"nginx","namespace":"default"}`)); err != nil {
		t.Fatalf("handlePodSync(delete default) 失败: %v", err)
	}
	pods, err := s.ListPods()
	if err != nil {
		t.Fatalf("ListPods 失败: %v", err)
	}
	if len(pods) != 1 || !strings.Contains(pods[0], `"namespace":"prod"`) {
		t.Errorf("删除 default/nginx 后 ListPods = %v，期望只剩 prod/nginx", pods)
	}

	// 删除不存在的 Pod：幂等成功
	if err := handlePodSync(s, podSyncMsg(t, "delete", `{"name":"nginx","namespace":"default"}`)); err != nil {
		t.Errorf("重复 delete 期望幂等成功，实际: %v", err)
	}
}

// TestHandlePodSyncUnknownOperation 验证未知 operation：返回 error
// （EdgeHub 会据此回 Ack code=error，云端不再重试同 ID）。
func TestHandlePodSyncUnknownOperation(t *testing.T) {
	s := newPodSyncStore(t)
	err := handlePodSync(s, podSyncMsg(t, "scale", `{"name":"nginx","namespace":"default"}`))
	if err == nil {
		t.Fatal("未知 operation 期望报错，实际成功")
	}
	if !strings.Contains(err.Error(), "未知的 PodSync operation") {
		t.Errorf("错误文案不符: %v", err)
	}
	// 未知 operation 不应落盘任何数据
	pods, lerr := s.ListPods()
	if lerr != nil {
		t.Fatalf("ListPods 失败: %v", lerr)
	}
	if len(pods) != 0 {
		t.Errorf("未知 operation 后 ListPods 长度 = %d，期望 0", len(pods))
	}
}

// TestHandlePodSyncBadPayload 验证坏 payload：返回 error（回 Ack code=error）。
func TestHandlePodSyncBadPayload(t *testing.T) {
	s := newPodSyncStore(t)
	// 手工构造 Payload 为非法 JSON 的消息（绕过 NewMessage 的序列化）
	msg := &protocol.Message{Type: protocol.TypePodSync, Payload: json.RawMessage(`{"operation":`)}
	if err := handlePodSync(s, msg); err == nil {
		t.Fatal("坏 payload 期望报错，实际成功")
	}
}

// TestHandlePodSyncDeleteMissingName 验证 delete 时 pod 缺少 name：返回 error。
func TestHandlePodSyncDeleteMissingName(t *testing.T) {
	s := newPodSyncStore(t)
	err := handlePodSync(s, podSyncMsg(t, "delete", `{"namespace":"default"}`))
	if err == nil {
		t.Fatal("delete 缺 name 期望报错，实际成功")
	}
	if !strings.Contains(err.Error(), "缺少 name") {
		t.Errorf("错误文案不符: %v", err)
	}
}

// TestHandlePodSyncUpdateNamespaceNormalization 验证：update 排除同名 Pod 时
// 空 namespace 与 "default" 视为等价（normalizeNS 归一，与 metamanager 的
// key 派生规则一致），旧值不会被重复计入超卖校验。
// 若未归一：5500+5900=11400m > 6000m 上限 → 误拒；归一后 5900m ≤ 6000m → 允许。
func TestHandlePodSyncUpdateNamespaceNormalization(t *testing.T) {
	for k, v := range map[string]string{
		"EDGEFLOW_EDGECORE_NODE_CPU_MILLI":         "4000",
		"EDGEFLOW_EDGECORE_NODE_MEMORY_BYTES":      "8589934592",
		"EDGEFLOW_EDGECORE_OVERCOMMIT_CPU_RATE":    "1.5",
		"EDGEFLOW_EDGECORE_OVERCOMMIT_MEMORY_RATE": "1.5",
	} {
		t.Setenv(k, v)
	}
	s := newPodSyncStore(t)

	// 旧值：namespace 省略（落盘 key 归一为 default），5500m（2 副本 × 2750m）
	oldPod := `{"name":"big","image":"nginx:1.25","replicas":2,` +
		`"resources":{"cpuRequest":"2750m","cpuLimit":"2750m"}}`
	if err := handlePodSync(s, podSyncMsg(t, "add", oldPod)); err != nil {
		t.Fatalf("add 失败: %v", err)
	}
	// 新值：namespace 显式 "default"，5900m（2 副本 × 2950m）
	newPod := `{"name":"big","namespace":"default","image":"nginx:1.26","replicas":2,` +
		`"resources":{"cpuRequest":"2950m","cpuLimit":"2950m"}}`
	if err := handlePodSync(s, podSyncMsg(t, "update", newPod)); err != nil {
		t.Fatalf("update 排除空 namespace 旧值后应允许，实际: %v", err)
	}
}
