package edged

// 本文件覆盖 WBS 6.4 镜像更新滚动策略与 WBS 6.5 资源漂移的单元测试：
//   - MockRuntime 镜像/资源漂移语义（SetImage/SetResources 注入 → EnsureRunning 重建）；
//   - ImageMatches / ResourcesMatch 接口行为；
//   - reconcile 的滚动重建：按 index 顺序 0→N-1，每轮最多 1 个（镜像与资源共门控）。

import (
	"encoding/json"
	"testing"
	"time"

	"edgeflow/edge/pkg/metamanager"
)

// savePodWithResourcesN 落盘一条带资源 limit 的 Pod（副本数 replicas）。
// 走真实 metamanager.SavePod 解析路径（资源漂移测试用）。
func savePodWithResourcesN(t *testing.T, s *metamanager.Store, ns, name, image string, res metamanager.ResourceRequirements, replicas int) {
	t.Helper()
	b, err := json.Marshal(metamanager.Pod{Namespace: ns, Name: name, Image: image, Replicas: replicas, Resources: res})
	if err != nil {
		t.Fatalf("序列化 Pod 失败: %v", err)
	}
	if err := s.SavePod(string(b)); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
}

// TestMockEnsureRunningImageDrift 验证 MockRuntime 的镜像漂移处理：
// 镜像一致 → no-op（不重建）；镜像不一致 → 重建（EnsureStoppedCount 与
// EnsureRunningCount 同时增加，镜像恢复为期望值）。
func TestMockEnsureRunningImageDrift(t *testing.T) {
	rt := NewMockRuntime()
	pod := podNginx() // image=nginx:1.27
	key := instanceKey("default", "nginx", 0)

	// 首次创建：记录镜像 nginx:1.27
	if err := rt.EnsureRunning(pod, 0); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if rt.CreateCount(key) != 1 || rt.EnsureStoppedCount(key) != 0 {
		t.Fatalf("创建后计数异常: create=%d stopped=%d", rt.CreateCount(key), rt.EnsureStoppedCount(key))
	}

	// 镜像一致 → no-op：不触发删除/重建
	if err := rt.EnsureRunning(pod, 0); err != nil {
		t.Fatalf("镜像一致 EnsureRunning 失败: %v", err)
	}
	if rt.EnsureRunningCount(key) != 2 {
		t.Errorf("EnsureRunningCount = %d，期望 2（调用计数仍应增加）", rt.EnsureRunningCount(key))
	}
	if rt.EnsureStoppedCount(key) != 0 || rt.CreateCount(key) != 1 {
		t.Errorf("镜像一致不应重建: stopped=%d create=%d", rt.EnsureStoppedCount(key), rt.CreateCount(key))
	}

	// 注入镜像漂移：外部把容器镜像改成 nginx:1.26
	rt.SetImage(key, "nginx:1.26")
	if err := rt.EnsureRunning(pod, 0); err != nil {
		t.Fatalf("漂移重建失败: %v", err)
	}
	// 重建：先停（EnsureStoppedCount+1）再建（CreateCount+1），镜像恢复期望值
	if rt.EnsureStoppedCount(key) != 1 {
		t.Errorf("漂移后 EnsureStoppedCount = %d，期望 1（先停旧容器）", rt.EnsureStoppedCount(key))
	}
	if rt.EnsureRunningCount(key) != 3 {
		t.Errorf("漂移后 EnsureRunningCount = %d，期望 3", rt.EnsureRunningCount(key))
	}
	if rt.CreateCount(key) != 2 {
		t.Errorf("漂移后 CreateCount = %d，期望 2（重建=重新创建）", rt.CreateCount(key))
	}
	if img := rt.Image(key); img != "nginx:1.27" {
		t.Errorf("重建后镜像 = %q，期望恢复 nginx:1.27", img)
	}
	if st := rt.State(key); st != StateRunning {
		t.Errorf("重建后状态 = %s，期望 running", st)
	}

	// 重建后再调：镜像已一致 → no-op
	if err := rt.EnsureRunning(pod, 0); err != nil {
		t.Fatalf("重建后 EnsureRunning 失败: %v", err)
	}
	if rt.EnsureStoppedCount(key) != 1 || rt.CreateCount(key) != 2 {
		t.Errorf("重建后不应再重建: stopped=%d create=%d", rt.EnsureStoppedCount(key), rt.CreateCount(key))
	}
}

// TestMockEnsureRunningStoppedNoRebuild 验证：Stopped 态重启是"启动"语义，
// 不做镜像检查/重建（与 DockerRuntime 的 docker start 分支对齐）。
func TestMockEnsureRunningStoppedNoRebuild(t *testing.T) {
	rt := NewMockRuntime()
	pod := podNginx()
	key := instanceKey("default", "nginx", 0)

	if err := rt.EnsureRunning(pod, 0); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	rt.SetState(key, StateStopped) // 外部停止容器
	rt.SetImage(key, "nginx:1.26") // 同时存在镜像漂移（Stopped 态不处理）

	if err := rt.EnsureRunning(pod, 0); err != nil {
		t.Fatalf("Stopped 态 EnsureRunning 失败: %v", err)
	}
	if rt.EnsureStoppedCount(key) != 0 {
		t.Errorf("Stopped 态不应触发删除: stopped=%d", rt.EnsureStoppedCount(key))
	}
	if rt.CreateCount(key) != 1 {
		t.Errorf("Stopped 态不应重建: create=%d", rt.CreateCount(key))
	}
	if st := rt.State(key); st != StateRunning {
		t.Errorf("重启后状态 = %s，期望 running", st)
	}
}

// TestMockImageMatches 验证 MockRuntime.ImageMatches 语义：
// 未记录镜像视为一致；记录后精确比对。
func TestMockImageMatches(t *testing.T) {
	rt := NewMockRuntime()
	pod := podNginx()
	key := instanceKey("default", "nginx", 0)

	// 未记录（未创建/SetState 注入状态）→ 一致
	if ok, err := rt.ImageMatches(pod, 0); err != nil || !ok {
		t.Errorf("未记录镜像 → ok=%v, err=%v，期望 true,nil", ok, err)
	}
	// 创建后记录期望镜像 → 一致
	if err := rt.EnsureRunning(pod, 0); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if ok, err := rt.ImageMatches(pod, 0); err != nil || !ok {
		t.Errorf("镜像一致 → ok=%v, err=%v，期望 true,nil", ok, err)
	}
	// 漂移 → 不一致
	rt.SetImage(key, "nginx:1.26")
	if ok, err := rt.ImageMatches(pod, 0); err != nil || ok {
		t.Errorf("镜像漂移 → ok=%v, err=%v，期望 false,nil", ok, err)
	}
}

// TestEdgedReconcileImageDriftRebuild 验证 reconcile 闭环：
// 期望 Pod 镜像被云端更新（SavePod 覆盖）后，漂移副本在下一轮调谐被重建，
// 且重建不计入 RestartCount（主动更新，非崩溃重启）。
func TestEdgedReconcileImageDriftRebuild(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}
	key := "default/nginx#0"
	if rt.CreateCount(key) != 1 || rt.EnsureStoppedCount(key) != 0 {
		t.Fatalf("初始状态异常: create=%d stopped=%d", rt.CreateCount(key), rt.EnsureStoppedCount(key))
	}

	// 模拟外部把容器镜像改掉（docker 层漂移，期望未变）
	rt.SetImage(key, "nginx:1.26")

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("漂移后 reconcile 失败: %v", err)
	}
	// 重建完成：先停再建
	if rt.EnsureStoppedCount(key) != 1 {
		t.Errorf("EnsureStoppedCount = %d，期望 1", rt.EnsureStoppedCount(key))
	}
	if rt.EnsureRunningCount(key) != 2 {
		t.Errorf("EnsureRunningCount = %d，期望 2", rt.EnsureRunningCount(key))
	}
	if rt.CreateCount(key) != 2 {
		t.Errorf("CreateCount = %d，期望 2", rt.CreateCount(key))
	}
	if img := rt.Image(key); img != "nginx:1.27" {
		t.Errorf("重建后镜像 = %q，期望恢复 nginx:1.27", img)
	}

	// 下一轮：镜像已一致 → 不再重建
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第三轮 reconcile 失败: %v", err)
	}
	if rt.EnsureStoppedCount(key) != 1 || rt.EnsureRunningCount(key) != 2 {
		t.Errorf("一致后不应再重建: stopped=%d running=%d",
			rt.EnsureStoppedCount(key), rt.EnsureRunningCount(key))
	}
	// 主动重建不计入 RestartCount（CrashLoopBackOff 语义）
	if st := e.Status()["default/nginx"]; st.RestartCount != 0 {
		t.Errorf("镜像重建不应计入 RestartCount，实际 %d", st.RestartCount)
	}
}

// TestEdgedReconcileRollingRebuild 验证滚动策略（WBS 6.4）：
// 3 副本全部镜像漂移时，按 index 顺序 0→2 逐轮重建，每轮恰好 1 个；
// 全部重建完成后不再有重建动作。
func TestEdgedReconcileRollingRebuild(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePodN(t, s, "default", "web", "nginx:1.27", 3)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("初始 reconcile 失败: %v", err)
	}
	keys := []string{
		instanceKey("default", "web", 0),
		instanceKey("default", "web", 1),
		instanceKey("default", "web", 2),
	}
	for _, k := range keys {
		if rt.CreateCount(k) != 1 {
			t.Fatalf("%s 初始创建次数 = %d，期望 1", k, rt.CreateCount(k))
		}
	}

	// 全部副本镜像漂移（模拟批量镜像更新）
	for _, k := range keys {
		rt.SetImage(k, "nginx:1.26")
	}

	// 第 1 轮：只重建 index 0
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第 1 轮 reconcile 失败: %v", err)
	}
	if got := rt.EnsureStoppedCount(keys[0]); got != 1 {
		t.Errorf("第 1 轮 index 0 应重建（stopped=%d，期望 1）", got)
	}
	if got := rt.EnsureStoppedCount(keys[1]); got != 0 {
		t.Errorf("第 1 轮 index 1 不应重建（stopped=%d，期望 0）", got)
	}
	if got := rt.EnsureStoppedCount(keys[2]); got != 0 {
		t.Errorf("第 1 轮 index 2 不应重建（stopped=%d，期望 0）", got)
	}

	// 第 2 轮：index 1（index 0 已恢复一致，不再动）
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第 2 轮 reconcile 失败: %v", err)
	}
	if got := rt.EnsureStoppedCount(keys[0]); got != 1 {
		t.Errorf("第 2 轮 index 0 不应再重建（stopped=%d，期望 1）", got)
	}
	if got := rt.EnsureStoppedCount(keys[1]); got != 1 {
		t.Errorf("第 2 轮 index 1 应重建（stopped=%d，期望 1）", got)
	}
	if got := rt.EnsureStoppedCount(keys[2]); got != 0 {
		t.Errorf("第 2 轮 index 2 不应重建（stopped=%d，期望 0）", got)
	}

	// 第 3 轮：index 2
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第 3 轮 reconcile 失败: %v", err)
	}
	if got := rt.EnsureStoppedCount(keys[2]); got != 1 {
		t.Errorf("第 3 轮 index 2 应重建（stopped=%d，期望 1）", got)
	}

	// 第 4 轮：全部一致 → 无任何重建
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第 4 轮 reconcile 失败: %v", err)
	}
	for i, k := range keys {
		if got := rt.EnsureStoppedCount(k); got != 1 {
			t.Errorf("第 4 轮 %s 不应再重建（stopped=%d，期望 1）", k, got)
		}
		if img := rt.Image(k); img != "nginx:1.27" {
			t.Errorf("%s 重建后镜像 = %q，期望 nginx:1.27", k, img)
		}
		_ = i
	}
}

// ---------- 资源漂移（WBS 6.5）reconcile 闭环测试 ----------

// TestEdgedReconcileResourceDriftRebuild 验证资源漂移闭环：容器健康 Running
// 但资源 limit 被外部改动（docker update --cpus 0.25，镜像不变）→
// 下一轮调谐触发重建（先停再建），资源恢复期望值，且不计入 RestartCount
// （主动更新，非崩溃重启）。
func TestEdgedReconcileResourceDriftRebuild(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	res := metamanager.ResourceRequirements{CPULimit: "500m", MemoryLimit: "64Mi"}
	savePodWithResourcesN(t, s, "default", "nginx", "nginx:1.27", res, 1)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}
	key := "default/nginx#0"
	if rt.CreateCount(key) != 1 || rt.EnsureStoppedCount(key) != 0 {
		t.Fatalf("初始状态异常: create=%d stopped=%d", rt.CreateCount(key), rt.EnsureStoppedCount(key))
	}
	// 创建后记录期望资源：500m → 500000000 nano；64Mi → 67108864 bytes
	if cpu, mem := rt.Resources(key); cpu != 500_000_000 || mem != 64<<20 {
		t.Fatalf("创建后记录的资源 = (%d, %d)，期望 (500000000, 67108864)", cpu, mem)
	}

	// 模拟外部 docker update --cpus 0.25（CPU limit 漂移，容器仍在运行）
	rt.SetResources(key, 250_000_000, 64<<20)

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("漂移后 reconcile 失败: %v", err)
	}
	// 重建完成：先停再建
	if rt.EnsureStoppedCount(key) != 1 {
		t.Errorf("EnsureStoppedCount = %d，期望 1", rt.EnsureStoppedCount(key))
	}
	if rt.CreateCount(key) != 2 {
		t.Errorf("CreateCount = %d，期望 2", rt.CreateCount(key))
	}
	if st := rt.State(key); st != StateRunning {
		t.Errorf("重建后状态 = %s，期望 running", st)
	}
	// 重建后资源恢复期望值
	if cpu, mem := rt.Resources(key); cpu != 500_000_000 || mem != 64<<20 {
		t.Errorf("重建后资源 = (%d, %d)，期望 (500000000, 67108864)", cpu, mem)
	}
	// 主动重建不计入 RestartCount（与镜像漂移同语义）
	if st := e.Status()["default/nginx"]; st.RestartCount != 0 {
		t.Errorf("资源重建不应计入 RestartCount，实际 %d", st.RestartCount)
	}

	// 下一轮：资源已一致 → 不再重建
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第三轮 reconcile 失败: %v", err)
	}
	if rt.EnsureStoppedCount(key) != 1 || rt.CreateCount(key) != 2 {
		t.Errorf("一致后不应再重建: stopped=%d create=%d",
			rt.EnsureStoppedCount(key), rt.CreateCount(key))
	}
}

// TestEdgedReconcileResourceMatchNoRebuild 验证：容器资源与期望一致时
// 调谐不触发重建（ResourcesMatch 通过 → no-op），与镜像一致的幂等语义并列。
func TestEdgedReconcileResourceMatchNoRebuild(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	res := metamanager.ResourceRequirements{CPULimit: "500m", MemoryLimit: "64Mi"}
	savePodWithResourcesN(t, s, "default", "nginx", "nginx:1.27", res, 1)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}
	key := "default/nginx#0"

	// 资源与镜像都一致 → 健康副本不再触发任何重建/重启
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	if rt.EnsureStoppedCount(key) != 0 {
		t.Errorf("资源一致不应重建: stopped=%d", rt.EnsureStoppedCount(key))
	}
	if rt.CreateCount(key) != 1 {
		t.Errorf("资源一致不应重建: create=%d", rt.CreateCount(key))
	}
	if rt.EnsureRunningCount(key) != 1 {
		t.Errorf("健康副本不应再调 EnsureRunning: running=%d，期望 1", rt.EnsureRunningCount(key))
	}
	if st := e.Status()["default/nginx"]; st.State != StateRunning || st.Err != nil {
		t.Errorf("Status = %+v，期望 running 且无错误", st)
	}
}

// TestEdgedReconcileResourceDriftRolling 验证资源漂移同样受滚动门控：
// 3 副本全部资源漂移时，按 index 顺序 0→2 逐轮重建，每轮恰好 1 个；
// 全部重建完成后不再有重建动作（与镜像漂移共用"每轮最多 1 个"门控）。
func TestEdgedReconcileResourceDriftRolling(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	res := metamanager.ResourceRequirements{CPULimit: "500m", MemoryLimit: "64Mi"}
	savePodWithResourcesN(t, s, "default", "web", "nginx:1.27", res, 3)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("初始 reconcile 失败: %v", err)
	}
	keys := []string{
		instanceKey("default", "web", 0),
		instanceKey("default", "web", 1),
		instanceKey("default", "web", 2),
	}
	for _, k := range keys {
		if rt.CreateCount(k) != 1 {
			t.Fatalf("%s 初始创建次数 = %d，期望 1", k, rt.CreateCount(k))
		}
	}

	// 全部副本资源漂移（模拟外部 docker update 批量改动 limit）
	for _, k := range keys {
		rt.SetResources(k, 250_000_000, 32<<20) // 漂移值：250m / 32Mi
	}

	// 第 1 轮：只重建 index 0
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第 1 轮 reconcile 失败: %v", err)
	}
	if got := rt.EnsureStoppedCount(keys[0]); got != 1 {
		t.Errorf("第 1 轮 index 0 应重建（stopped=%d，期望 1）", got)
	}
	if got := rt.EnsureStoppedCount(keys[1]); got != 0 {
		t.Errorf("第 1 轮 index 1 不应重建（stopped=%d，期望 0）", got)
	}
	if got := rt.EnsureStoppedCount(keys[2]); got != 0 {
		t.Errorf("第 1 轮 index 2 不应重建（stopped=%d，期望 0）", got)
	}

	// 第 2 轮：index 1（index 0 已恢复一致，不再动）
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第 2 轮 reconcile 失败: %v", err)
	}
	if got := rt.EnsureStoppedCount(keys[0]); got != 1 {
		t.Errorf("第 2 轮 index 0 不应再重建（stopped=%d，期望 1）", got)
	}
	if got := rt.EnsureStoppedCount(keys[1]); got != 1 {
		t.Errorf("第 2 轮 index 1 应重建（stopped=%d，期望 1）", got)
	}
	if got := rt.EnsureStoppedCount(keys[2]); got != 0 {
		t.Errorf("第 2 轮 index 2 不应重建（stopped=%d，期望 0）", got)
	}

	// 第 3 轮：index 2
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第 3 轮 reconcile 失败: %v", err)
	}
	if got := rt.EnsureStoppedCount(keys[2]); got != 1 {
		t.Errorf("第 3 轮 index 2 应重建（stopped=%d，期望 1）", got)
	}

	// 第 4 轮：全部一致 → 无任何重建
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第 4 轮 reconcile 失败: %v", err)
	}
	for _, k := range keys {
		if got := rt.EnsureStoppedCount(k); got != 1 {
			t.Errorf("第 4 轮 %s 不应再重建（stopped=%d，期望 1）", k, got)
		}
		if cpu, mem := rt.Resources(k); cpu != 500_000_000 || mem != 64<<20 {
			t.Errorf("%s 重建后资源 = (%d, %d)，期望 (500000000, 67108864)", k, cpu, mem)
		}
	}
}
