package edged

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/edge/pkg/metamanager"
)

// ---------- 测试辅助 ----------

// newTestStore 在临时目录打开 metamanager.Store（与 metamanager 包测试同模式）。
func newTestStore(t *testing.T) *metamanager.Store {
	t.Helper()
	s, err := metamanager.Open(filepath.Join(t.TempDir(), "edgeflow.db"))
	if err != nil {
		t.Fatalf("打开测试 Store 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// savePod 用 metamanager.SavePod 落盘一条单副本 Pod（走真实解析路径）。
func savePod(t *testing.T, s *metamanager.Store, ns, name, image string) {
	t.Helper()
	savePodN(t, s, ns, name, image, 1)
}

// savePodN 用 metamanager.SavePod 落盘一条指定副本数的 Pod（走真实解析路径）。
func savePodN(t *testing.T, s *metamanager.Store, ns, name, image string, replicas int) {
	t.Helper()
	b, err := json.Marshal(metamanager.Pod{Namespace: ns, Name: name, Image: image, Replicas: replicas})
	if err != nil {
		t.Fatalf("序列化 Pod 失败: %v", err)
	}
	if err := s.SavePod(string(b)); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
}

// waitUntil 轮询等待条件成立（默认 2s 超时），供异步断言使用。
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// waitRounds 等待状态表的调谐时间戳至少前进 n 轮（Start/Stop 生命周期测试用）：
// 健康副本不再每轮触发 EnsureRunning，LastReconcile 是"循环仍在调谐"的心跳。
func waitRounds(t *testing.T, e *Edged, key string, n int) {
	t.Helper()
	seen := make(map[time.Time]struct{})
	waitUntil(t, fmt.Sprintf("至少 %d 轮调谐", n), func() bool {
		st := e.Status()[key]
		if st.LastReconcile.IsZero() {
			return false
		}
		seen[st.LastReconcile] = struct{}{}
		return len(seen) >= n
	})
}

// ---------- reconciler 状态机测试 ----------

// TestEdgedReconcileCreatesPod 验证：期望集合新增 Pod → 调谐后
// EnsureRunning 被调用、状态为 Running、Status() 可见。
func TestEdgedReconcileCreatesPod(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	savePod(t, s, "prod", "redis", "redis:7")

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce 失败: %v", err)
	}

	for _, key := range []string{"default/nginx#0", "prod/redis#0"} {
		if rt.EnsureRunningCount(key) != 1 {
			t.Errorf("%s: EnsureRunning 调用次数 = %d，期望 1", key, rt.EnsureRunningCount(key))
		}
		if rt.CreateCount(key) != 1 {
			t.Errorf("%s: 创建次数 = %d，期望 1", key, rt.CreateCount(key))
		}
		if got := rt.State(key); got != StateRunning {
			t.Errorf("%s: 状态 = %s，期望 running", key, got)
		}
		podKey := strings.SplitN(key, "#", 2)[0]
		st, ok := e.Status()[podKey]
		if !ok || st.State != StateRunning || st.Err != nil {
			t.Errorf("%s: Status() = %+v，期望 running 且无错误", podKey, st)
		}
	}
}

// TestEdgedReconcileRemovesDeletedPod 验证：期望集合删除 Pod →
// 调谐后 EnsureStopped 被调用、本地容器被清理（状态 Absent）。
func TestEdgedReconcileRemovesDeletedPod(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}
	if rt.State("default/nginx#0") != StateRunning {
		t.Fatalf("前置条件：nginx 应已运行")
	}

	// 云端删除 Pod（MetaManager 删除元数据）
	if err := s.DeletePod("default", "nginx"); err != nil {
		t.Fatalf("DeletePod 失败: %v", err)
	}
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}

	if rt.EnsureStoppedCount("default/nginx#0") != 1 {
		t.Errorf("EnsureStopped 调用次数 = %d，期望 1", rt.EnsureStoppedCount("default/nginx#0"))
	}
	if got := rt.State("default/nginx#0"); got != StateAbsent {
		t.Errorf("清理后状态 = %s，期望 absent", got)
	}
	// 孤儿清理后，List 不再返回该 Pod
	insts, err := rt.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(insts) != 0 {
		t.Errorf("清理后本地容器 = %v，期望为空", insts)
	}
	// P2-1（M2B 审查 P1 修复）：孤儿清理成功后条目进入 Absent 终态保留
	// （RemovedAt 非零），供上报循环把删除终态送达云端；窗口期满后才清理。
	st, ok := e.Status()["default/nginx"]
	if !ok {
		t.Fatal("删除后条目应保留（Absent 终态，P1 修复），实际已被清理")
	}
	if st.RemovedAt.IsZero() {
		t.Error("条目应记录 RemovedAt（Absent 终态保留），实际为零值")
	}
}

// TestEdgedReconcileStoppedContainerIsRestarted 验证：
// 容器存在但停止（如节点重启后容器未自启）→ 调谐把状态收敛回 Running。
func TestEdgedReconcileStoppedContainerIsRestarted(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}

	// 模拟容器被外部停止（状态直接置 Stopped，模拟运行时重启后容器未运行）
	rt.SetState("default/nginx#0", StateStopped)

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	if got := rt.State("default/nginx#0"); got != StateRunning {
		t.Errorf("停止后重新调谐，状态 = %s，期望 running", got)
	}
	// 重新拉起不应算"创建"（幂等语义：start 而非 recreate）
	if rt.CreateCount("default/nginx#0") != 1 {
		t.Errorf("CreateCount = %d，期望 1（应 start 而非重建）", rt.CreateCount("default/nginx#0"))
	}
}

// TestEdgedReconcileFailureRetries 验证：容器异常（运行时注入失败）→
// 调谐记录错误、不 panic、不误报 Running；故障恢复后重试成功。
func TestEdgedReconcileFailureRetries(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")

	// 注入失败：模拟容器运行时异常（如 daemon 不可用/镜像拉取失败）
	rt.SetFail("default/nginx#0", errors.New("boom: 注入的运行时故障"))

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce 不应因单 Pod 失败而返回错误: %v", err)
	}
	st := e.Status()["default/nginx"]
	if st.Err == nil {
		t.Fatalf("期望 Status 记录错误，实际无错误")
	}
	if !strings.Contains(st.Err.Error(), "boom") {
		t.Errorf("错误内容不符: %v", st.Err)
	}
	if st.State == StateRunning {
		t.Errorf("失败轮不应标记为 running")
	}
	if rt.State("default/nginx#0") != StateAbsent {
		t.Errorf("失败轮不应改变 mock 状态")
	}

	// 故障恢复 → 下一轮重试成功
	rt.SetFail("default/nginx#0", nil)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("恢复后 reconcile 失败: %v", err)
	}
	if rt.State("default/nginx#0") != StateRunning {
		t.Errorf("恢复后状态 = %s，期望 running", rt.State("default/nginx#0"))
	}
	if st := e.Status()["default/nginx"]; st.Err != nil || st.State != StateRunning {
		t.Errorf("恢复后 Status() = %+v，期望 running 且无错误", st)
	}
}

// TestEdgedReconcileIdempotent 验证幂等：连续两次调谐不重复创建容器。
// 注意：健康检查先行——副本已 Running 时不再调用 EnsureRunning（检查优先，
// 避免无谓的运行时操作），因此两轮调谐合计只调用 1 次。
func TestEdgedReconcileIdempotent(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}

	if rt.CreateCount("default/nginx#0") != 1 {
		t.Errorf("两次调谐后 CreateCount = %d，期望 1（幂等）", rt.CreateCount("default/nginx#0"))
	}
	// 副本已 Running 时第二轮不再触发 EnsureRunning（健康检查先行）
	if rt.EnsureRunningCount("default/nginx#0") != 1 {
		t.Errorf("EnsureRunning 调用次数 = %d，期望 1（健康副本不再重复调）",
			rt.EnsureRunningCount("default/nginx#0"))
	}
}

// TestEdgedReconcileListFailureContinues 验证：List 失败（运行时不可用）时
// 不 panic、期望 Pod 仍尝试 EnsureRunning、错误被记录。
func TestEdgedReconcileListFailureContinues(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	rt.SetListErr(errors.New("docker daemon unavailable: 模拟 daemon 未启动"))

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("List 失败不应让 reconcileOnce 返回错误: %v", err)
	}
	if rt.State("default/nginx#0") != StateRunning {
		t.Errorf("List 失败时 EnsureRunning 仍应执行，状态 = %s", rt.State("default/nginx#0"))
	}
}

// TestEdgedReconcileInvalidPodJSONSkipped 验证：脏数据（非法 Pod JSON）被跳过，
// 不中断整体调谐。
func TestEdgedReconcileInvalidPodJSONSkipped(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	// 直接塞一条非法 JSON（绕过 SavePod 校验，模拟历史脏数据）
	if err := s.Put("pods/default/bad", "{not-json"); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}
	savePod(t, s, "default", "nginx", "nginx:1.27")

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce 失败: %v", err)
	}
	if rt.State("default/nginx#0") != StateRunning {
		t.Errorf("合法 Pod 应正常运行，状态 = %s", rt.State("default/nginx#0"))
	}
}

// ---------- 多副本（WBS 6.4）测试 ----------

// TestEdgedReconcileReplicasScalesUp 验证多副本展开：replicas=3 →
// 调谐后 3 个副本容器（index 0/1/2）全部运行；再次调谐幂等不重复创建。
func TestEdgedReconcileReplicasScalesUp(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePodN(t, s, "default", "nginx", "nginx:1.27", 3)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce 失败: %v", err)
	}

	for idx := 0; idx < 3; idx++ {
		key := fmt.Sprintf("default/nginx#%d", idx)
		if got := rt.State(key); got != StateRunning {
			t.Errorf("副本 %d 状态 = %s，期望 running", idx, got)
		}
		if rt.CreateCount(key) != 1 {
			t.Errorf("副本 %d 创建次数 = %d，期望 1", idx, rt.CreateCount(key))
		}
	}
	// List 视角：恰好 3 个实例
	insts, err := rt.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(insts) != 3 {
		t.Errorf("List 实例数 = %d，期望 3: %+v", len(insts), insts)
	}

	// 幂等：再调谐一轮不重复创建任何副本
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	for idx := 0; idx < 3; idx++ {
		key := fmt.Sprintf("default/nginx#%d", idx)
		if rt.CreateCount(key) != 1 {
			t.Errorf("副本 %d 幂等轮创建次数 = %d，期望 1", idx, rt.CreateCount(key))
		}
	}
}

// TestEdgedReconcileReplicasScalesDown 验证缩容：replicas 3→1 →
// 停止多余副本（从最大 index 开始），副本 0 保持运行，List 只剩 1 个实例。
func TestEdgedReconcileReplicasScalesDown(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePodN(t, s, "default", "nginx", "nginx:1.27", 3)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}

	// 期望缩到 1 副本（云端 update replicas=1）
	savePodN(t, s, "default", "nginx", "nginx:1.27", 1)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}

	if got := rt.State("default/nginx#0"); got != StateRunning {
		t.Errorf("副本 0 应保持运行，实际 %s", got)
	}
	for _, idx := range []int{1, 2} {
		key := fmt.Sprintf("default/nginx#%d", idx)
		if got := rt.State(key); got != StateAbsent {
			t.Errorf("副本 %d 应被停止，实际 %s", idx, got)
		}
		if rt.EnsureStoppedCount(key) != 1 {
			t.Errorf("副本 %d EnsureStopped 次数 = %d，期望 1", idx, rt.EnsureStoppedCount(key))
		}
	}
	// 收敛后 List 只剩 1 个实例（副本 0）
	insts, err := rt.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(insts) != 1 || insts[0].Index != 0 {
		t.Errorf("缩容后 List = %+v，期望只剩副本 0", insts)
	}
}

// TestEdgedReconcileReplicasDefaultOne 验证 Replicas 缺省（0/未下发）→ 单副本。
func TestEdgedReconcileReplicasDefaultOne(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePodN(t, s, "default", "nginx", "nginx:1.27", 0) // replicas=0 视为缺省
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce 失败: %v", err)
	}
	if got := rt.State("default/nginx#0"); got != StateRunning {
		t.Errorf("缺省副本应创建 -0，状态 = %s", got)
	}
	insts, err := rt.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(insts) != 1 {
		t.Errorf("缺省副本数实例数 = %d，期望 1: %+v", len(insts), insts)
	}
}

// ---------- 健康检查自愈（WBS 6.5）测试 ----------

// TestEdgedHealthCheckRestartsStoppedReplica 验证健康自愈：
// 期望副本处于 Stopped（无错误）→ 调谐重启并累加 RestartCount；
// 恢复正常后幂等轮不产生额外重启。
func TestEdgedHealthCheckRestartsStoppedReplica(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}
	if got := e.Status()["default/nginx"].RestartCount; got != 0 {
		t.Fatalf("初始 RestartCount = %d，期望 0", got)
	}

	// 模拟容器进程崩溃（容器存在但停止，如进程退出/外部 docker stop）
	rt.SetState("default/nginx#0", StateStopped)

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	if got := rt.State("default/nginx#0"); got != StateRunning {
		t.Errorf("健康检查后状态 = %s，期望 running", got)
	}
	st := e.Status()["default/nginx"]
	if st.State != StateRunning || st.Err != nil {
		t.Errorf("Status = %+v，期望 running 且无错误", st)
	}
	if st.RestartCount != 1 {
		t.Errorf("RestartCount = %d，期望 1", st.RestartCount)
	}

	// 再次崩溃 → RestartCount 累加（POC 无退避，每轮重试）
	rt.SetState("default/nginx#0", StateStopped)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第三次 reconcile 失败: %v", err)
	}
	if got := e.Status()["default/nginx"].RestartCount; got != 2 {
		t.Errorf("二次崩溃后 RestartCount = %d，期望 2", got)
	}

	// 幂等：Running 状态下调谐不产生重启
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第四次 reconcile 失败: %v", err)
	}
	if got := e.Status()["default/nginx"].RestartCount; got != 2 {
		t.Errorf("健康轮 RestartCount = %d，期望仍为 2", got)
	}
}

// TestEdgedHealthCheckRestartsSingleReplicaOfMany 验证多副本下健康检查
// 只重启异常副本：replicas=3 时停掉副本 1，其余副本不受影响。
func TestEdgedHealthCheckRestartsSingleReplicaOfMany(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePodN(t, s, "default", "nginx", "nginx:1.27", 3)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}

	rt.SetState("default/nginx#1", StateStopped)

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	for idx := 0; idx < 3; idx++ {
		key := fmt.Sprintf("default/nginx#%d", idx)
		if got := rt.State(key); got != StateRunning {
			t.Errorf("副本 %d 状态 = %s，期望 running", idx, got)
		}
	}
	if got := e.Status()["default/nginx"].RestartCount; got != 1 {
		t.Errorf("RestartCount = %d，期望 1（只重启异常副本）", got)
	}
	// 健康副本不应被重建
	for _, idx := range []int{0, 2} {
		if rt.CreateCount(fmt.Sprintf("default/nginx#%d", idx)) != 1 {
			t.Errorf("健康副本 %d 不应被重建", idx)
		}
	}
}

// TestEdgedHealthCheckRestartsMissingReplica 验证缺失副本自愈：
// 期望副本被外部删除（Absent）→ 调谐补齐创建（缺口场景兜底）。
func TestEdgedHealthCheckRestartsMissingReplica(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	savePodN(t, s, "default", "nginx", "nginx:1.27", 2)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第一次 reconcile 失败: %v", err)
	}

	// 外部删除副本 1（模拟 docker rm），列表出现缺口
	rt.DeleteState("default/nginx#1")

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	if got := rt.State("default/nginx#1"); got != StateRunning {
		t.Errorf("缺失副本 1 应被补齐，状态 = %s", got)
	}
	if got := rt.State("default/nginx#0"); got != StateRunning {
		t.Errorf("副本 0 应保持运行，状态 = %s", got)
	}
	// 副本 1 被重新创建（Absent→Running）
	if rt.CreateCount("default/nginx#1") != 2 {
		t.Errorf("副本 1 创建次数 = %d，期望 2（首次 + 补齐）", rt.CreateCount("default/nginx#1"))
	}
}

// ---------- Start/Stop 生命周期测试 ----------

// TestEdgedStopStopsLoop 验证：Stop() 后调谐循环停止（状态表的调谐时间戳
// 不再前进），且 Stop 可重复调用不 panic。
//
// 说明：健康副本不再每轮触发 EnsureRunning（检查先行），故用 Status() 的
// LastReconcile 时间戳作为"循环仍在调谐"的心跳（每轮必更新）。
func TestEdgedStopStopsLoop(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 10*time.Millisecond) // 短周期加速测试

	savePod(t, s, "default", "nginx", "nginx:1.27")
	e.Start()

	// 等循环跑起来（至少 2 轮调谐）
	waitRounds(t, e, "default/nginx", 2)

	e.Stop()
	e.Stop() // 重复 Stop 应 no-op

	last := e.Status()["default/nginx"].LastReconcile
	time.Sleep(60 * time.Millisecond) // 超过 5 个周期
	if got := e.Status()["default/nginx"].LastReconcile; got.After(last) {
		t.Errorf("Stop 后仍在调谐: %v → %v", last, got)
	}

	// Stop 后可重新 Start（edgecore 生命周期场景），重启后调谐恢复
	e.Start()
	waitUntil(t, "重新 Start 后恢复调谐", func() bool {
		return e.Status()["default/nginx"].LastReconcile.After(last)
	})
	e.Stop()
}

// TestEdgedStartTwiceIsNoop 验证：未 Stop 时重复 Start 是 no-op（不产生双循环）。
func TestEdgedStartTwiceIsNoop(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 10*time.Millisecond)

	savePod(t, s, "default", "nginx", "nginx:1.27")
	e.Start()
	e.Start() // 重复 Start 应 no-op
	waitRounds(t, e, "default/nginx", 3)
	e.Stop()

	// 仅一个循环：Stop 后调谐时间戳立即冻结
	last := e.Status()["default/nginx"].LastReconcile
	time.Sleep(50 * time.Millisecond)
	if got := e.Status()["default/nginx"].LastReconcile; got.After(last) {
		t.Errorf("重复 Start 产生多个循环，Stop 后仍在调谐: %v → %v", last, got)
	}
}

// TestEdgedStartReconcilesImmediately 验证：Start 后立即调谐一轮（不等第一个 tick）。
func TestEdgedStartReconcilesImmediately(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, time.Hour) // 周期拉长：若等 tick 则永不调谐

	savePod(t, s, "default", "nginx", "nginx:1.27")
	e.Start()
	waitUntil(t, "Start 后立即调谐", func() bool { return rt.State("default/nginx#0") == StateRunning })
	e.Stop()
}

// ---------- MockRuntime 自身测试 ----------

// TestMockRuntimeConcurrency 验证 MockRuntime 线程安全（-race 下运行）。
func TestMockRuntimeConcurrency(t *testing.T) {
	rt := NewMockRuntime()
	pod := metamanager.Pod{Namespace: "default", Name: "nginx", Image: "nginx:1.27"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = rt.EnsureRunning(pod, 0)
				_, _ = rt.Inspect(pod, 0)
				_, _ = rt.List()
			}
		}()
	}
	wg.Wait()
	if got := rt.CreateCount("default/nginx#0"); got != 1 {
		t.Errorf("并发 EnsureRunning 后 CreateCount = %d，期望 1", got)
	}
	if got := rt.State("default/nginx#0"); got != StateRunning {
		t.Errorf("并发后状态 = %s，期望 running", got)
	}
}

// ---------- 命名/键派生测试 ----------

// TestContainerName 验证容器命名规范（多副本：edgeflow-<ns>-<name>-<index>）
// 与合法化处理。
func TestContainerName(t *testing.T) {
	cases := []struct {
		ns, name string
		index    int
		want     string
	}{
		{"default", "nginx", 0, "edgeflow-default-nginx-0"},
		{"", "nginx", 0, "edgeflow-default-nginx-0"},        // namespace 缺省补 default
		{"default", "nginx", 1, "edgeflow-default-nginx-1"}, // 副本 1
		{"default", "nginx", 9, "edgeflow-default-nginx-9"}, // 副本 9
		{"prod", "my-app", 2, "edgeflow-prod-my-app-2"},     // 连字符保留
		{"prod", "My_App", 0, "edgeflow-prod-my_app-0"},     // 大写转小写、下划线保留
		{"ns x", "nginx", 0, "edgeflow-ns-x-nginx-0"},       // 非法字符替换为 '-'
	}
	for _, c := range cases {
		if got := ContainerName(c.ns, c.name, c.index); got != c.want {
			t.Errorf("ContainerName(%q, %q, %d) = %q，期望 %q", c.ns, c.name, c.index, got, c.want)
		}
	}

	// 超长名：基础名截断 + hash 后缀，但副本序号保留在末尾且不超上限
	long0 := ContainerName("default", strings.Repeat("a", 500), 0)
	long1 := ContainerName("default", strings.Repeat("a", 500), 1)
	if len(long0) > maxContainerNameLen {
		t.Errorf("超长名截断后仍超限: %d", len(long0))
	}
	if !strings.HasSuffix(long0, "-0") || !strings.HasSuffix(long1, "-1") {
		t.Errorf("超长名应保留副本序号后缀: %q / %q", long0, long1)
	}
	if len(long0) < 100 {
		t.Errorf("超长名应带 hash 后缀且保持较长: %q", long0)
	}
	// 不同超长名截断后不同（hash 防碰撞）
	long2 := ContainerName("default", strings.Repeat("b", 500), 0)
	if long0 == long2 {
		t.Errorf("不同超长名不应碰撞: %q", long0)
	}
}

// TestPodKey 验证调谐键派生（namespace 缺省补 default）。
func TestPodKey(t *testing.T) {
	if got := podKey("default", "nginx"); got != "default/nginx" {
		t.Errorf("podKey = %q，期望 default/nginx", got)
	}
	if got := podKey("", "nginx"); got != "default/nginx" {
		t.Errorf("podKey(空 namespace) = %q，期望 default/nginx", got)
	}
	if got := podKey("prod", "nginx"); got != "prod/nginx" {
		t.Errorf("podKey = %q，期望 prod/nginx", got)
	}
}

// TestParseInstanceKey 验证副本实例键可逆拆回 InstanceRef。
func TestParseInstanceKey(t *testing.T) {
	inst := parseInstanceKey("prod/nginx#2")
	if inst.Namespace != "prod" || inst.Name != "nginx" || inst.Index != 2 {
		t.Errorf("parseInstanceKey 结果 = %+v，期望 prod/nginx#2", inst)
	}
	inst = parseInstanceKey("nginx#0")
	if inst.Namespace != "default" || inst.Name != "nginx" || inst.Index != 0 {
		t.Errorf("parseInstanceKey(无 namespace) 结果 = %+v，期望 default/nginx#0", inst)
	}
	// 旧式键（无 #index）兜底副本 0
	inst = parseInstanceKey("default/nginx")
	if inst.Namespace != "default" || inst.Name != "nginx" || inst.Index != 0 {
		t.Errorf("parseInstanceKey(旧式键) 结果 = %+v，期望 default/nginx#0", inst)
	}
	// 非法 index 兜底 0
	inst = parseInstanceKey("default/nginx#x")
	if inst.Index != 0 {
		t.Errorf("parseInstanceKey(非法 index) 结果 = %+v，期望 index=0", inst)
	}
}

// TestParsePodKey 验证 podKey 可逆拆回 Pod。
func TestParsePodKey(t *testing.T) {
	p := parsePodKey("prod/nginx")
	if p.Namespace != "prod" || p.Name != "nginx" {
		t.Errorf("parsePodKey 结果 = %+v，期望 prod/nginx", p)
	}
	p = parsePodKey("nginx") // 无 namespace 兜底
	if p.Namespace != "default" || p.Name != "nginx" {
		t.Errorf("parsePodKey(无 namespace) 结果 = %+v，期望 default/nginx", p)
	}
}

// TestRuntimeStateString 验证状态枚举的可读名。
func TestRuntimeStateString(t *testing.T) {
	cases := []struct {
		st   RuntimeState
		want string
	}{
		{StateAbsent, "absent"},
		{StateRunning, "running"},
		{StateStopped, "stopped"},
		{StateUnknown, "unknown"},
		{RuntimeState(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.st.String(); got != c.want {
			t.Errorf("RuntimeState(%d).String() = %q，期望 %q", c.st, got, c.want)
		}
	}
}

// TestNewDefaultInterval 验证默认周期。
func TestNewDefaultInterval(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 0)
	if e.interval != DefaultReconcileInterval {
		t.Errorf("interval = %v，期望默认 %v", e.interval, DefaultReconcileInterval)
	}
	if fmt.Sprintf("%v", DefaultReconcileInterval) != "5s" {
		t.Errorf("默认周期应为 5s，实际 %v", DefaultReconcileInterval)
	}
}
