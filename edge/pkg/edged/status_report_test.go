// Pod 状态上报（WBS 6.3）与状态表清理（P2-1）测试。
package edged

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"edgeflow/edge/pkg/metamanager"
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

// TestEdgedLegacyContainerMigration 验证 M4A P1-1 修复：
// 旧式无序号命名实例（Index=-1）优先被移除，并由规范副本名重建；无 churn。
func TestEdgedLegacyContainerMigration(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, time.Millisecond)

	if err := s.SavePod(`{"name":"nginx","namespace":"default","image":"nginx:1.25","replicas":1}`); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
	// 模拟旧命名实例（升级前遗留的无序号容器）
	legacyKey := "default/nginx#-1"
	rt.SetState(legacyKey, StateRunning)

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcile 失败: %v", err)
	}
	// 旧实例被移除
	if rt.EnsureStoppedCount(legacyKey) < 1 {
		t.Error("旧命名实例应被 EnsureStopped 移除（P1-1），实际未移除")
	}
	// 规范副本已创建
	if rt.EnsureRunningCount("default/nginx#0") < 1 {
		t.Error("应创建规范命名副本 -0（P1-1），实际未创建")
	}

	// 第二轮 reconcile：无 churn（旧实例已清，不再有停止操作）
	stoppedBefore := rt.EnsureStoppedCount(legacyKey)
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("第二次 reconcile 失败: %v", err)
	}
	if rt.EnsureStoppedCount(legacyKey) != stoppedBefore {
		t.Error("第二轮 reconcile 不应再次操作旧实例（churn），实际仍在清理")
	}
}

// TestEdgedCrashLoopBackOff 验证 M4A P1-2 修复：
// 连续重启达到阈值后进入退避窗口，不再每轮无限重启。
func TestEdgedCrashLoopBackOff(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, time.Millisecond)

	if err := s.SavePod(`{"name":"crashy","namespace":"default","image":"busybox:1.36","replicas":1}`); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
	key := "default/crashy#0"

	// 模拟退出型镜像：每次 reconcile 后容器立即退出（手动置 Stopped）。
	// 第 1 轮是创建（不计重启），之后每轮健康重启 +1：共需
	// maxRestartBeforeBackoff+1 轮让 count 达到阈值。
	for i := 0; i <= maxRestartBeforeBackoff; i++ {
		if err := e.reconcileOnce(); err != nil {
			t.Fatalf("第 %d 次 reconcile 失败: %v", i+1, err)
		}
		rt.SetState(key, StateStopped)
	}
	restartsBefore := rt.EnsureRunningCount(key)
	if restartsBefore < maxRestartBeforeBackoff {
		t.Fatalf("重启次数应达到 %d，实际 %d", maxRestartBeforeBackoff, restartsBefore)
	}

	// 第 4 轮：进入退避，不应重启
	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("退避轮 reconcile 失败: %v", err)
	}
	if rt.EnsureRunningCount(key) != restartsBefore {
		t.Error("退避期内不应重启容器（P1-2），实际仍重启")
	}
	// 退避状态记录在 PodStatus
	st, ok := e.Status()["default/crashy"]
	if !ok || st.Err == nil {
		t.Fatalf("退避状态应记录到 PodStatus.Err，实际 %+v", st)
	}
}

// ---------- v0.12.0 R-1++ digest 采集测试 ----------

// TestDigestOfImageRef 验证声明式 digest 解析规则（v0.12.0，R-1++）：
// 取最后一个 '@' 之后部分，匹配 ^sha256:[0-9a-f]{64}$ 才返回（统一小写）；
// 其余（无 @ / 非 sha256 / 长度不足 / 非法字符 / 尾部空）→ ""。
func TestDigestOfImageRef(t *testing.T) {
	const ok = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"tag+digest", "reg.example.com/ns/model:v1.2.0@" + ok, ok},
		{"bare+digest", "reg.example.com/ns/model@" + ok, ok},
		{"uppercase hex 归一", "reg/model@sha256:0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF", ok},
		{"多 @ 取末", "reg/model@@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ok},
		{"无 digest", "reg.example.com/ns/model:v1.2.0", ""},
		{"无 @", "reg/model", ""},
		{"长度不足", "reg/model@sha256:abc", ""},
		{"非法字符", "reg/model@sha256:zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", ""},
		{"sha512 拒绝", "reg/model@sha512:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ""},
		{"@extra 拒绝", "reg/model@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef@extra", ""},
		{"尾 @ 拒绝", "reg/model@", ""},
		{"空串", "", ""},
		{"仅 sha256 前缀", "reg/model@sha256:", ""},
	}
	for _, tc := range cases {
		if got := digestOfImageRef(tc.ref); got != tc.want {
			t.Errorf("%s: digestOfImageRef(%q) = %q，期望 %q", tc.name, tc.ref, got, tc.want)
		}
	}
}

// TestBuildStatusPayloadFillsDeclarativeDigest 验证声明式通道（v0.12.0）：
// Desired Pod 镜像含 @sha256: 且 Pod Running → 上报该 digest；Stopped /
// 无 pin 的 Running → 不填（老行为）；Absent → 不填。
func TestBuildStatusPayloadFillsDeclarativeDigest(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, time.Hour)

	const ok = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// 期望集合：三个 Pod（一个 pin、一个 tag 无 pin、一个 pin 但将置 Stopped）
	savePod(t, s, "default", "pinned", "reg.io/ns/model:v1@"+ok)
	savePod(t, s, "default", "tagged", "reg.io/ns/model:v1")
	savePod(t, s, "default", "stopped", "reg.io/ns/model:v1@"+ok)

	now := time.UnixMilli(1755168000000)
	e.setStatus("default/pinned", StateRunning, nil, now)
	e.setStatus("default/tagged", StateRunning, nil, now)
	e.setStatus("default/stopped", StateStopped, nil, now)

	payloads := e.BuildStatusPayload("edge-001")
	got := map[string]string{}
	for _, p := range payloads {
		got[p.Namespace+"/"+p.PodName] = p.ImageDigest
	}
	if got["default/pinned"] != ok {
		t.Errorf("pinned（Running+声明式）digest = %q，期望 %q", got["default/pinned"], ok)
	}
	if got["default/tagged"] != "" {
		t.Errorf("tagged（Running 无 pin，Mock 无运行时注入）digest = %q，期望空串（声明式空→运行时空→跳过）", got["default/tagged"])
	}
	if got["default/stopped"] != "" {
		t.Errorf("stopped（非 Running）digest = %q，期望空串（仅 Running 上报）", got["default/stopped"])
	}
}

// TestBuildStatusPayloadRuntimeFallback 验证运行时通道（v0.12.0）：无声明式
// pin 且 Running → 取 MockRuntime 注入 digest；注入为空 → 不填。
func TestBuildStatusPayloadRuntimeFallback(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, time.Hour)

	const rtDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	savePod(t, s, "default", "web", "reg.io/ns/model:v1") // 无 pin → 运行时通道
	savePod(t, s, "default", "web2", "reg.io/ns/model:v2")
	rt.SetImageDigest("default", "web", 0, rtDigest)

	now := time.UnixMilli(1755168000000)
	e.setStatus("default/web", StateRunning, nil, now)
	e.setStatus("default/web2", StateRunning, nil, now)

	payloads := e.BuildStatusPayload("edge-001")
	got := map[string]string{}
	for _, p := range payloads {
		got[p.Namespace+"/"+p.PodName] = p.ImageDigest
	}
	if got["default/web"] != rtDigest {
		t.Errorf("web（运行时注入）digest = %q，期望 %q", got["default/web"], rtDigest)
	}
	if got["default/web2"] != "" {
		t.Errorf("web2（未注入）digest = %q，期望空串", got["default/web2"])
	}
}

// TestBuildStatusPayloadRuntimeFailureSkips 验证运行时失败降级（v0.12.0）：
// 无声明式 pin 且运行时查询报错 → 空串 + 不 panic + 上报继续。
func TestBuildStatusPayloadRuntimeFailureSkips(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, time.Hour)

	savePod(t, s, "default", "web", "reg.io/ns/model:v1")
	// 用 ImageDigest 注入一个会返回错误的实现：MockRuntime 无法注入 error，
	// 用自定义 failingRuntime 包装验证降级路径。
	e.rt = &failingDigestRuntime{inner: rt}

	now := time.UnixMilli(1755168000000)
	e.setStatus("default/web", StateRunning, nil, now)

	payloads := e.BuildStatusPayload("edge-001")
	if len(payloads) != 1 {
		t.Fatalf("上报条数 = %d，期望 1（失败不阻塞）", len(payloads))
	}
	if payloads[0].ImageDigest != "" {
		t.Errorf("运行时失败 digest = %q，期望空串（降级跳过）", payloads[0].ImageDigest)
	}
}

// failingDigestRuntime 包装 MockRuntime，ImageDigest 恒返回错误（模拟
// daemon 不可用），用于验证 digest 采集失败降级不阻塞上报。
type failingDigestRuntime struct {
	inner *MockRuntime
}

func (f *failingDigestRuntime) EnsureRunning(pod metamanager.Pod, index int) error {
	return f.inner.EnsureRunning(pod, index)
}
func (f *failingDigestRuntime) EnsureStopped(pod metamanager.Pod, index int) error {
	return f.inner.EnsureStopped(pod, index)
}
func (f *failingDigestRuntime) Inspect(pod metamanager.Pod, index int) (RuntimeState, error) {
	return f.inner.Inspect(pod, index)
}
func (f *failingDigestRuntime) ImageMatches(pod metamanager.Pod, index int) (bool, error) {
	return f.inner.ImageMatches(pod, index)
}
func (f *failingDigestRuntime) ResourcesMatch(pod metamanager.Pod, index int) (bool, error) {
	return f.inner.ResourcesMatch(pod, index)
}
func (f *failingDigestRuntime) ImageDigest(pod metamanager.Pod, index int) (string, error) {
	return "", errors.New("docker daemon unavailable: test")
}
func (f *failingDigestRuntime) List() ([]InstanceRef, error) {
	return f.inner.List()
}
func (f *failingDigestRuntime) Close() error {
	return f.inner.Close()
}
