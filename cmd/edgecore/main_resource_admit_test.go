package main

import (
	"strings"
	"testing"

	"edgeflow/pkg/resource"
)

// setNodeResourceEnv 为测试固定节点容量与超卖率（环境变量注入），
// 测后恢复原值。容量：4 核 / 8GiB，超卖率 150%/150% →
// CPU 上限 6000m，内存上限 12GiB。
func setNodeResourceEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"EDGEFLOW_EDGECORE_NODE_CPU_MILLI":         "4000",
		"EDGEFLOW_EDGECORE_NODE_MEMORY_BYTES":      "8589934592",
		"EDGEFLOW_EDGECORE_OVERCOMMIT_CPU_RATE":    "1.5",
		"EDGEFLOW_EDGECORE_OVERCOMMIT_MEMORY_RATE": "1.5",
	} {
		t.Setenv(k, v)
	}
}

// TestHandlePodSyncOvercommitReject 验证：已部署 request 总和 + 新 request
// 超过超卖率上限时 add 被拒绝，错误带 ErrResourceExhausted 标记，
// 且 Pod 不落盘（拒绝部署）。
func TestHandlePodSyncOvercommitReject(t *testing.T) {
	setNodeResourceEnv(t)
	s := newPodSyncStore(t)

	// 已有部署：5500m CPU + 10GiB 内存（2 副本 × 2750m = 5500m）
	existing := `{"name":"big","namespace":"default","image":"nginx:1.25","replicas":2,` +
		`"resources":{"cpuRequest":"2750m","cpuLimit":"2750m","memoryRequest":"5Gi","memoryLimit":"5Gi"}}`
	if err := handlePodSync(s, podSyncMsg(t, "add", existing)); err != nil {
		t.Fatalf("已有部署 add 失败: %v", err)
	}

	// 新部署：CPU 250m → 5500+250=5750 ≤ 6000 允许；
	//         内存 3Gi → 10+3=13GiB > 12GiB 拒绝（Memory 维度超卖）
	newPod := `{"name":"newpod","namespace":"default","image":"nginx:1.25",` +
		`"resources":{"cpuRequest":"250m","cpuLimit":"250m","memoryRequest":"3Gi","memoryLimit":"3Gi"}}`
	err := handlePodSync(s, podSyncMsg(t, "add", newPod))
	if err == nil {
		t.Fatal("超卖应拒绝，实际成功")
	}
	if !strings.HasPrefix(err.Error(), resource.ErrResourceExhausted) {
		t.Errorf("拒绝错误缺少 %s 标记: %v", resource.ErrResourceExhausted, err)
	}
	if !strings.Contains(err.Error(), "超出节点资源") {
		t.Errorf("拒绝错误缺少「超出节点资源」语义: %v", err)
	}
	if !strings.Contains(err.Error(), "Memory") {
		t.Errorf("拒绝理由应含 Memory 维度: %v", err)
	}

	// 拒绝的 Pod 不落盘
	pods, lerr := s.ListPods()
	if lerr != nil {
		t.Fatalf("ListPods 失败: %v", lerr)
	}
	if len(pods) != 1 {
		t.Errorf("拒绝后 ListPods 长度 = %d，期望 1（新 Pod 未落盘）: %v", len(pods), pods)
	}
}

// TestHandlePodSyncOvercommitBoundaryEqual 验证边界：总和 == 上限 → 允许。
func TestHandlePodSyncOvercommitBoundaryEqual(t *testing.T) {
	setNodeResourceEnv(t)
	s := newPodSyncStore(t)

	// 已有 5750m CPU → 新 250m 使总和 = 6000m（= 上限 4000×150%）→ 允许
	existing := `{"name":"a","namespace":"default","image":"nginx:1.25",` +
		`"resources":{"cpuRequest":"5750m","cpuLimit":"5750m"}}`
	if err := handlePodSync(s, podSyncMsg(t, "add", existing)); err != nil {
		t.Fatalf("已有部署 add 失败: %v", err)
	}
	newPod := `{"name":"b","namespace":"default","image":"nginx:1.25",` +
		`"resources":{"cpuRequest":"250m","cpuLimit":"250m"}}`
	if err := handlePodSync(s, podSyncMsg(t, "add", newPod)); err != nil {
		t.Fatalf("等于上限应允许，实际拒绝: %v", err)
	}
}

// TestHandlePodSyncOvercommitBoundaryExceed 验证边界：总和 = 上限+1m → 拒绝。
func TestHandlePodSyncOvercommitBoundaryExceed(t *testing.T) {
	setNodeResourceEnv(t)
	s := newPodSyncStore(t)

	existing := `{"name":"a","namespace":"default","image":"nginx:1.25",` +
		`"resources":{"cpuRequest":"5750m","cpuLimit":"5750m"}}`
	if err := handlePodSync(s, podSyncMsg(t, "add", existing)); err != nil {
		t.Fatalf("已有部署 add 失败: %v", err)
	}
	newPod := `{"name":"b","namespace":"default","image":"nginx:1.25",` +
		`"resources":{"cpuRequest":"251m","cpuLimit":"251m"}}`
	err := handlePodSync(s, podSyncMsg(t, "add", newPod))
	if err == nil || !strings.HasPrefix(err.Error(), resource.ErrResourceExhausted) {
		t.Fatalf("超上限 1m 应拒绝（带标记），实际: %v", err)
	}
}

// TestHandlePodSyncUpdateExcludesSelf 验证 update：校验时排除同名 Pod 旧值，
// 同 Pod 扩大 request 不因旧值重复计入而被误拒。
func TestHandlePodSyncUpdateExcludesSelf(t *testing.T) {
	setNodeResourceEnv(t)
	s := newPodSyncStore(t)

	// 旧版本：5500m CPU（2 副本）；新版本：5900m（2 副本），
	// 校验时旧值 5500 被排除，5900 ≤ 6000 → 允许
	oldPod := `{"name":"big","namespace":"default","image":"nginx:1.25","replicas":2,` +
		`"resources":{"cpuRequest":"2750m","cpuLimit":"2750m"}}`
	if err := handlePodSync(s, podSyncMsg(t, "add", oldPod)); err != nil {
		t.Fatalf("add 失败: %v", err)
	}
	newPod := `{"name":"big","namespace":"default","image":"nginx:1.26","replicas":2,` +
		`"resources":{"cpuRequest":"2950m","cpuLimit":"2950m"}}`
	if err := handlePodSync(s, podSyncMsg(t, "update", newPod)); err != nil {
		t.Fatalf("update 排除旧值后应允许，实际: %v", err)
	}
}

// TestHandlePodSyncRequestExceedsLimit 验证：request > limit 被拒绝（边缘兜底）。
func TestHandlePodSyncRequestExceedsLimit(t *testing.T) {
	setNodeResourceEnv(t)
	s := newPodSyncStore(t)

	pod := `{"name":"bad","namespace":"default","image":"nginx:1.25",` +
		`"resources":{"cpuRequest":"1","cpuLimit":"250m"}}`
	err := handlePodSync(s, podSyncMsg(t, "add", pod))
	if err == nil {
		t.Fatal("request>limit 应拒绝，实际成功")
	}
	if !strings.Contains(err.Error(), "request") || !strings.Contains(err.Error(), "limit") {
		t.Errorf("错误应指明 request/limit 语义: %v", err)
	}
	// 不落盘
	pods, lerr := s.ListPods()
	if lerr != nil {
		t.Fatalf("ListPods 失败: %v", lerr)
	}
	if len(pods) != 0 {
		t.Errorf("拒绝后应无落盘数据: %v", pods)
	}
}

// TestHandlePodSyncNoResourcesPasses 验证：无资源字段的 Pod 不受准入影响
// （向后兼容：旧云端/旧客户端零值 = 不限制）。
func TestHandlePodSyncNoResourcesPasses(t *testing.T) {
	setNodeResourceEnv(t)
	s := newPodSyncStore(t)

	// 已超卖（CPU 8000m 请求直接落盘，模拟历史数据/高超卖率时代部署的存量）
	old := `{"name":"huge","namespace":"default","image":"nginx:1.25",` +
		`"resources":{"cpuRequest":"8000m","cpuLimit":"8000m"}}`
	if err := handlePodSync(s, podSyncMsg(t, "add", old)); err == nil {
		// 8000m > 6000m 上限：这次 add 应该被拒。换个方式：直接 SavePod 模拟存量
		t.Log("8000m 直接 add 被拒（符合预期），改用 SavePod 模拟存量数据")
	}
	if err := s.SavePod(old); err != nil {
		t.Fatalf("SavePod 存量失败: %v", err)
	}

	// 无资源字段的新 Pod：不占资源 → 允许
	plain := `{"name":"plain","namespace":"default","image":"nginx:1.25"}`
	if err := handlePodSync(s, podSyncMsg(t, "add", plain)); err != nil {
		t.Fatalf("无资源字段 Pod 应允许: %v", err)
	}
}

// TestHandlePodSyncDeleteSkipsAdmission 验证：delete 不做资源校验（无需资源字段）。
func TestHandlePodSyncDeleteSkipsAdmission(t *testing.T) {
	setNodeResourceEnv(t)
	s := newPodSyncStore(t)
	if err := handlePodSync(s, podSyncMsg(t, "delete", `{"name":"x","namespace":"default"}`)); err != nil {
		t.Fatalf("delete 应跳过资源校验: %v", err)
	}
}

// podSyncMsg 与 newPodSyncStore 复用 main_podsync_test.go 中的定义。
