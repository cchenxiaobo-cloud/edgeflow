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

// savePod 用 metamanager.SavePod 落盘一条 Pod（走真实解析路径）。
func savePod(t *testing.T, s *metamanager.Store, ns, name, image string) {
	t.Helper()
	b, err := json.Marshal(metamanager.Pod{Namespace: ns, Name: name, Image: image, Replicas: 1})
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

	for _, key := range []string{"default/nginx", "prod/redis"} {
		if rt.EnsureRunningCount(key) != 1 {
			t.Errorf("%s: EnsureRunning 调用次数 = %d，期望 1", key, rt.EnsureRunningCount(key))
		}
		if rt.CreateCount(key) != 1 {
			t.Errorf("%s: 创建次数 = %d，期望 1", key, rt.CreateCount(key))
		}
		if got := rt.State(key); got != StateRunning {
			t.Errorf("%s: 状态 = %s，期望 running", key, got)
		}
		st, ok := e.Status()[key]
		if !ok || st.State != StateRunning || st.Err != nil {
			t.Errorf("%s: Status() = %+v，期望 running 且无错误", key, st)
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
	if rt.State("default/nginx") != StateRunning {
		t.Fatalf("前置条件：nginx 应已运行")
	}

	// 云端删除 Pod（MetaManager 删除元数据）
	if err := s.DeletePod("default", "nginx"); err != nil {
		t.Fatalf("DeletePod 失败: %v", err)
	}
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}

	if rt.EnsureStoppedCount("default/nginx") != 1 {
		t.Errorf("EnsureStopped 调用次数 = %d，期望 1", rt.EnsureStoppedCount("default/nginx"))
	}
	if got := rt.State("default/nginx"); got != StateAbsent {
		t.Errorf("清理后状态 = %s，期望 absent", got)
	}
	// 孤儿清理后，List 不再返回该 Pod
	pods, err := rt.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(pods) != 0 {
		t.Errorf("清理后本地容器 = %v，期望为空", pods)
	}
	// P2-1：孤儿清理成功后，状态条目一并移除（不保留 Absent 历史，
	// 避免 status map 无界增长与过期条目误上报）
	if _, ok := e.Status()["default/nginx"]; ok {
		t.Errorf("Status() 仍含已删除 Pod 的条目，期望已清理（P2-1）")
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
	rt.mu.Lock()
	rt.states["default/nginx"] = StateStopped
	rt.mu.Unlock()

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	if got := rt.State("default/nginx"); got != StateRunning {
		t.Errorf("停止后重新调谐，状态 = %s，期望 running", got)
	}
	// 重新拉起不应算"创建"（幂等语义：start 而非 recreate）
	if rt.CreateCount("default/nginx") != 1 {
		t.Errorf("CreateCount = %d，期望 1（应 start 而非重建）", rt.CreateCount("default/nginx"))
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
	rt.SetFail("default/nginx", errors.New("boom: 注入的运行时故障"))

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
	if rt.State("default/nginx") != StateAbsent {
		t.Errorf("失败轮不应改变 mock 状态")
	}

	// 故障恢复 → 下一轮重试成功
	rt.SetFail("default/nginx", nil)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("恢复后 reconcile 失败: %v", err)
	}
	if rt.State("default/nginx") != StateRunning {
		t.Errorf("恢复后状态 = %s，期望 running", rt.State("default/nginx"))
	}
	if st := e.Status()["default/nginx"]; st.Err != nil || st.State != StateRunning {
		t.Errorf("恢复后 Status() = %+v，期望 running 且无错误", st)
	}
}

// TestEdgedReconcileIdempotent 验证幂等：连续两次调谐不重复创建容器。
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

	if rt.CreateCount("default/nginx") != 1 {
		t.Errorf("两次调谐后 CreateCount = %d，期望 1（幂等）", rt.CreateCount("default/nginx"))
	}
	if rt.EnsureRunningCount("default/nginx") != 2 {
		t.Errorf("EnsureRunning 调用次数 = %d，期望 2（每轮都调，但创建只发生一次）",
			rt.EnsureRunningCount("default/nginx"))
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
	if rt.State("default/nginx") != StateRunning {
		t.Errorf("List 失败时 EnsureRunning 仍应执行，状态 = %s", rt.State("default/nginx"))
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
	if rt.State("default/nginx") != StateRunning {
		t.Errorf("合法 Pod 应正常运行，状态 = %s", rt.State("default/nginx"))
	}
}

// ---------- Start/Stop 生命周期测试 ----------

// TestEdgedStopStopsLoop 验证：Stop() 后调谐循环停止（调用计数不再增长），
// 且 Stop 可重复调用不 panic。
func TestEdgedStopStopsLoop(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 10*time.Millisecond) // 短周期加速测试

	savePod(t, s, "default", "nginx", "nginx:1.27")
	e.Start()

	// 等循环跑起来（至少 2 次 EnsureRunning）
	waitUntil(t, "循环开始调谐", func() bool { return rt.EnsureRunningCount("default/nginx") >= 2 })

	e.Stop()
	e.Stop() // 重复 Stop 应 no-op

	count := rt.EnsureRunningCount("default/nginx")
	time.Sleep(60 * time.Millisecond) // 超过 5 个周期
	if got := rt.EnsureRunningCount("default/nginx"); got != count {
		t.Errorf("Stop 后 EnsureRunning 调用次数仍在增长: %d → %d", count, got)
	}

	// Stop 后可重新 Start（edgecore 生命周期场景），重启后调谐恢复
	e.Start()
	waitUntil(t, "重新 Start 后恢复调谐", func() bool {
		return rt.EnsureRunningCount("default/nginx") > count
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
	waitUntil(t, "循环运行", func() bool { return rt.EnsureRunningCount("default/nginx") >= 3 })
	e.Stop()

	// 仅一个循环：Stop 后计数立即冻结
	count := rt.EnsureRunningCount("default/nginx")
	time.Sleep(50 * time.Millisecond)
	if got := rt.EnsureRunningCount("default/nginx"); got != count {
		t.Errorf("重复 Start 产生多个循环，Stop 后计数仍在增长: %d → %d", count, got)
	}
}

// TestEdgedStartReconcilesImmediately 验证：Start 后立即调谐一轮（不等第一个 tick）。
func TestEdgedStartReconcilesImmediately(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, time.Hour) // 周期拉长：若等 tick 则永不调谐

	savePod(t, s, "default", "nginx", "nginx:1.27")
	e.Start()
	waitUntil(t, "Start 后立即调谐", func() bool { return rt.State("default/nginx") == StateRunning })
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
				_ = rt.EnsureRunning(pod)
				_, _ = rt.Inspect(pod)
				_, _ = rt.List()
			}
		}()
	}
	wg.Wait()
	if got := rt.CreateCount("default/nginx"); got != 1 {
		t.Errorf("并发 EnsureRunning 后 CreateCount = %d，期望 1", got)
	}
	if got := rt.State("default/nginx"); got != StateRunning {
		t.Errorf("并发后状态 = %s，期望 running", got)
	}
}

// ---------- 命名/键派生测试 ----------

// TestContainerName 验证容器命名规范与合法化处理。
func TestContainerName(t *testing.T) {
	cases := []struct {
		ns, name string
		want     string
	}{
		{"default", "nginx", "edgeflow-default-nginx"},
		{"", "nginx", "edgeflow-default-nginx"},    // namespace 缺省补 default
		{"prod", "my-app", "edgeflow-prod-my-app"}, // 连字符保留
		{"prod", "My_App", "edgeflow-prod-my_app"}, // 大写转小写、下划线保留
		{"ns x", "nginx", "edgeflow-ns-x-nginx"},   // 非法字符替换为 '-'
	}
	for _, c := range cases {
		if got := ContainerName(c.ns, c.name); got != c.want {
			t.Errorf("ContainerName(%q, %q) = %q，期望 %q", c.ns, c.name, got, c.want)
		}
	}

	// 超长名：截断 + hash 后缀，且不超上限、长度 > 100
	long := ContainerName("default", strings.Repeat("a", 500))
	if len(long) > maxContainerNameLen {
		t.Errorf("超长名截断后仍超限: %d", len(long))
	}
	if !strings.HasSuffix(long, "-") && len(long) < 100 {
		t.Errorf("超长名应带 hash 后缀且保持较长: %q", long)
	}
	// 不同超长名截断后不同（hash 防碰撞）
	long2 := ContainerName("default", strings.Repeat("b", 500))
	if long == long2 {
		t.Errorf("不同超长名不应碰撞: %q", long)
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
