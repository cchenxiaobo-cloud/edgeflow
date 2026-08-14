// Pod 状态上报（WBS 6.3）与状态表清理（P2-1）测试。
package edged

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestBuildStatusPayloadPhaseMapping 验证 phase 映射全覆盖（与云端契约一致）：
// Running/Stopped/Absent/Unknown → 对应阶段；Err!=nil → Error（错误优先），
// Message 填错误文本；nodeID/lastReconcileAt 原样透传；输出按 podKey 升序。
func TestBuildStatusPayloadPhaseMapping(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, time.Hour)

	now := time.UnixMilli(1755168000000)
	e.setStatus("default/web-demo", StateRunning, nil, now)
	e.setStatus("prod/worker", StateStopped, nil, now)
	e.setStatus("kube/gone", StateAbsent, nil, now)
	e.setStatus("kube/mystery", StateUnknown, nil, now)
	e.setStatus("kube/fail1", StateUnknown, errors.New("inspect: docker daemon 不可用"), now)
	e.setStatus("kube/fail2", StateRunning, errors.New("ensure: 镜像拉取失败"), now) // 错误优先于状态

	payloads := e.BuildStatusPayload("edge-001")
	if len(payloads) != 6 {
		t.Fatalf("BuildStatusPayload 返回 %d 条，期望 6", len(payloads))
	}

	want := map[string]struct{ phase, msg string }{
		"default/web-demo": {"Running", ""},
		"prod/worker":      {"Stopped", ""},
		"kube/gone":        {"Absent", ""},
		"kube/mystery":     {"Unknown", ""},
		"kube/fail1":       {"Error", "inspect: docker daemon 不可用"},
		"kube/fail2":       {"Error", "ensure: 镜像拉取失败"},
	}
	// 输出有序性：default/web-demo < kube/fail1 < kube/fail2 < kube/gone <
	// kube/mystery < prod/worker（podKey 字典序）
	if payloads[0].PodName != "web-demo" || payloads[0].Namespace != "default" {
		t.Errorf("输出顺序不符：首个 = %+v，期望 default/web-demo 最先", payloads[0])
	}
	for _, p := range payloads {
		key := p.Namespace + "/" + p.PodName
		w, ok := want[key]
		if !ok {
			t.Errorf("出现意外的负载条目: %s（%+v）", key, p)
			continue
		}
		if p.Phase != w.phase || p.Message != w.msg {
			t.Errorf("%s: phase/message = %q/%q，期望 %q/%q", key, p.Phase, p.Message, w.phase, w.msg)
		}
		if p.NodeID != "edge-001" {
			t.Errorf("%s: nodeID = %q，期望 edge-001", key, p.NodeID)
		}
		if p.LastReconcileAt != now.UnixMilli() {
			t.Errorf("%s: lastReconcileAt = %d，期望 %d", key, p.LastReconcileAt, now.UnixMilli())
		}
	}
}

// TestBuildStatusPayloadEmpty 验证：状态表为空时返回空数组（不 panic、不报假状态）。
func TestBuildStatusPayloadEmpty(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, time.Hour)

	if got := e.BuildStatusPayload("edge-001"); len(got) != 0 {
		t.Errorf("空状态表应返回空数组，实际 %+v", got)
	}
}

// TestPodStatusPayloadJSONContract 验证负载 JSON 字段名与云端契约一致（不可改）：
// 恰好 6 个字段：nodeID/podName/namespace/phase/message/lastReconcileAt。
func TestPodStatusPayloadJSONContract(t *testing.T) {
	p := PodStatusPayload{
		NodeID:          "edge-001",
		PodName:         "web-demo",
		Namespace:       "default",
		Phase:           "Error",
		Message:         "boom",
		LastReconcileAt: 1755168000000,
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal 失败: %v", err)
	}
	for _, field := range []string{"nodeID", "podName", "namespace", "phase", "message", "lastReconcileAt"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("契约字段 %q 缺失，序列化结果: %s", field, data)
		}
	}
	if got := raw["lastReconcileAt"]; got != float64(1755168000000) {
		t.Errorf("lastReconcileAt = %v，期望 1755168000000", got)
	}
	if len(raw) != 6 {
		t.Errorf("负载字段数 = %d，期望恰好 6 个契约字段（不可增删）: %s", len(raw), data)
	}
}

// TestEdgedCleanupStatusKeepsErrorEntryUntilContainerRemoved 验证 P2-1 异常路径：
// 孤儿清理失败（容器仍在）→ 保留 Error 条目供排查；清理成功 → 条目删除。
func TestEdgedCleanupStatusKeepsErrorEntryUntilContainerRemoved(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}
	if st := e.Status()["default/nginx"]; st.State != StateRunning {
		t.Fatalf("前置条件：nginx 应已运行，实际 %+v", st)
	}

	// 云端删除 Pod，同时注入孤儿清理失败（模拟 docker 删除容器失败）
	if err := s.DeletePod("default", "nginx"); err != nil {
		t.Fatalf("DeletePod 失败: %v", err)
	}
	rt.SetFail("default/nginx#0", errors.New("docker rm 失败: 设备忙"))

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	st, ok := e.Status()["default/nginx"]
	if !ok {
		t.Fatal("孤儿清理失败后条目不应被删除（保留 Error 供排查，P2-1 异常路径）")
	}
	if st.Err == nil || !strings.Contains(st.Err.Error(), "设备忙") {
		t.Errorf("条目应保留清理错误，实际 %+v", st)
	}

	// 故障恢复 → 孤儿清理成功 → 状态转 Absent → 条目进入终态保留（P1 修复）
	rt.SetFail("default/nginx#0", nil)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第三次 reconcile 失败: %v", err)
	}
	st2, ok := e.Status()["default/nginx"]
	if !ok {
		t.Fatal("孤儿清理成功后条目应进入 Absent 终态保留（P1 修复），实际被清理")
	}
	if st2.RemovedAt.IsZero() {
		t.Error("终态保留条目应记录 RemovedAt，实际为零值")
	}
}

// TestEdgedCleanupStatusRemovesStaleEntryWhenContainerGone 验证 P2-1 异常路径 2：
// 容器被外部删除但状态条目残留（State 非 Absent）→ 条目一并清理，
// 不保留"容器已不存在"的僵尸状态。
func TestEdgedCleanupStatusRemovesStaleEntryWhenContainerGone(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}

	// 云端删除 Pod；容器同时被外部移除（模拟其他工具删除容器）
	if err := s.DeletePod("default", "nginx"); err != nil {
		t.Fatalf("DeletePod 失败: %v", err)
	}
	rt.DeleteState("default/nginx#0")

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	if _, ok := e.Status()["default/nginx"]; ok {
		t.Errorf("容器已不存在的过期条目应被清理（P2-1），实际仍在")
	}
}

// TestEdgedRemovedRetentionExpiry 验证 P1 修复的窗口期满清理：
// Absent 终态条目在 removedRetention 期满后被删除（此后不再上报）。
func TestEdgedRemovedRetentionExpiry(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, time.Millisecond)
	e.removedRetention = 50 * time.Millisecond // 缩短窗口便于测试

	if err := e.reconcileOnce(); err != nil { // 0 pods，无操作
		t.Fatalf("reconcile 失败: %v", err)
	}
	if err := s.SavePod(`{"name":"nginx","namespace":"default","image":"nginx:1.25"}`); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcile 失败: %v", err)
	}
	if err := s.DeletePod("default", "nginx"); err != nil {
		t.Fatalf("DeletePod 失败: %v", err)
	}
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcile 失败: %v", err)
	}
	if st, ok := e.Status()["default/nginx"]; !ok || st.RemovedAt.IsZero() {
		t.Fatal("删除后应进入 Absent 终态保留")
	}
	// Absent 终态应进入上报负载
	payloads := e.BuildStatusPayload("edge-test")
	found := false
	for _, p := range payloads {
		if p.PodName == "nginx" && p.Phase == "Absent" {
			found = true
		}
	}
	if !found {
		t.Error("BuildStatusPayload 应包含 Absent 终态（P1 修复），实际缺失")
	}
	// 窗口期满后清理
	time.Sleep(80 * time.Millisecond)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcile 失败: %v", err)
	}
	if _, ok := e.Status()["default/nginx"]; ok {
		t.Error("窗口期满后条目应被清理，实际仍在")
	}
}

// TestEdgedCleanupStatusKeepsEntryWhenListFails 验证 P2-1 保守路径：
// 运行时 List 失败（无法确认容器是否还在）→ 只清理已确认 Absent 的条目，
// 其余保留，避免误删排查信息；恢复后孤儿清理成功则条目被删除。
func TestEdgedCleanupStatusKeepsEntryWhenListFails(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}

	// 云端删除 Pod 且 List 失败（模拟 daemon 不可用）
	if err := s.DeletePod("default", "nginx"); err != nil {
		t.Fatalf("DeletePod 失败: %v", err)
	}
	rt.SetListErr(errors.New("docker daemon unavailable"))

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	if _, ok := e.Status()["default/nginx"]; !ok {
		t.Fatal("List 失败时不应删除无法确认的条目（保守保留，P2-1）")
	}

	// 恢复后：孤儿清理成功 → 条目进入 Absent 终态保留（P1 修复）
	rt.SetListErr(nil)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第三次 reconcile 失败: %v", err)
	}
	if st, ok := e.Status()["default/nginx"]; !ok {
		t.Fatal("恢复后条目应进入 Absent 终态保留（P1 修复），实际被清理")
	} else if st.RemovedAt.IsZero() {
		t.Error("终态保留条目应记录 RemovedAt，实际为零值")
	}
}
