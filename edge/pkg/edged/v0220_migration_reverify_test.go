package edged

import (
	"errors"
	"strings"
	"testing"
	"time"

	"edgeflow/edge/pkg/metamanager"
)

// T-10（CHN-05，v0.22.0）验收 1 单测：Index<0 旧命名实例迁移后的 Inspect 复核。
//
// 背景：审计 CHN-05 指出迁移操作基于 List 快照，外部 docker 干预后操作结果
// 可能与实际状态背离。v0.22.0 在 3d 步 EnsureStopped 成功后追加 Inspect 复核：
// 容器未消失（外部重建/手工拉起）→ 记 Unknown、下轮重试，不误标迁移完成。

// TestLegacyMigrationInspectReverify 验收 1：迁移后 Inspect 复核通过 → 记 Absent。
func TestLegacyMigrationInspectReverify(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	e := New(s, rt, 100*time.Millisecond)

	// 期望集合：新版规范副本（replicas=1）；本地：旧命名容器（Index=-1）
	savePod(t, s, "default", "nginx", "nginx:1.27")
	rt.SetState("default/nginx#-1", StateRunning)
	// List 由 mock 状态推导：SetState 即视为本地存在该容器（见 mock List 实现）
	rt.SetState("default/nginx#0", StateAbsent)

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce 失败: %v", err)
	}
	if rt.EnsureStoppedCount("default/nginx#-1") != 1 {
		t.Fatalf("旧命名容器应被 EnsureStopped 一次，实际 %d", rt.EnsureStoppedCount("default/nginx#-1"))
	}
	// EnsureStopped 后 mock 将状态置为 Absent（删除语义）→ 复核应通过：
	// 状态表不出现 Unknown（迁移复核未通过）字样
	for key, st := range e.Status() {
		if st.State == StateUnknown && st.Err != nil && strings.Contains(st.Err.Error(), "迁移复核未通过") {
			t.Errorf("%s: 迁移复核不应失败（mock 已置 Absent）: %v", key, st.Err)
		}
	}
	// 规范副本被第 3 步重建为 Running
	if got := rt.State("default/nginx#0"); got != StateRunning {
		t.Errorf("规范副本 #0 状态 = %s，期望 running（迁移后重建）", got)
	}
}

// TestLegacyMigrationInspectBlocked 验收 1 反向：外部 docker 干预——
// EnsureStopped 返回成功但 Inspect 发现容器仍在（外部重建）→ 状态记
// Unknown + 迁移复核错误，不误标迁移完成；下一轮会重试迁移。
func TestLegacyMigrationInspectBlocked(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()

	savePod(t, s, "default", "nginx", "nginx:1.27")
	rt.SetState("default/nginx#-1", StateRunning)
	rt.SetState("default/nginx#0", StateAbsent)
	// 注入"外部干预"：Inspect 恒返回 Running（模拟容器被外部重建拉起），
	// 用包装 runtime 覆写 Inspect 语义。
	wrapper := &inspectOverrideRuntime{MockRuntime: rt, override: StateRunning}
	e2 := New(s, wrapper, 100*time.Millisecond)

	if err := e2.reconcileOnce(); err != nil {
		t.Fatalf("reconcileOnce 失败: %v", err)
	}
	if rt.EnsureStoppedCount("default/nginx#-1") != 1 {
		t.Fatalf("旧命名容器应被 EnsureStopped 一次，实际 %d", rt.EnsureStoppedCount("default/nginx#-1"))
	}
	found := false
	for key, st := range e2.Status() {
		if st.State == StateUnknown && st.Err != nil && strings.Contains(st.Err.Error(), "迁移复核未通过") {
			found = true
			_ = key
		}
	}
	if !found {
		t.Errorf("外部干预下迁移复核应失败并记 Unknown（迁移复核未通过），状态表: %+v", e2.Status())
	}
}

// TestLegacyMigrationInspectError 外部 docker 异常（Inspect 自身报错）→
// 同样记 Unknown 不误杀，错误不上抛（调谐循环继续）。
func TestLegacyMigrationInspectError(t *testing.T) {
	s := newTestStore(t)
	rt := NewMockRuntime()
	savePod(t, s, "default", "nginx", "nginx:1.27")
	rt.SetState("default/nginx#-1", StateRunning)
	rt.SetState("default/nginx#0", StateAbsent)

	boom := errors.New("docker daemon 不可达")
	wrapper := &inspectOverrideRuntime{MockRuntime: rt, err: boom}
	e := New(s, wrapper, 100*time.Millisecond)

	if err := e.reconcileOnce(); err != nil {
		t.Fatalf("Inspect 错误不应上抛（调谐继续），实际: %v", err)
	}
	found := false
	for _, st := range e.Status() {
		if st.State == StateUnknown && st.Err != nil && strings.Contains(st.Err.Error(), "迁移复核未通过") {
			found = true
		}
	}
	if !found {
		t.Errorf("Inspect 报错时应记 Unknown（迁移复核未通过），状态表: %+v", e.Status())
	}
}

// inspectOverrideRuntime 在 MockRuntime 上覆写 Inspect 行为（模拟外部干预）。
type inspectOverrideRuntime struct {
	*MockRuntime
	override RuntimeState // 非零值时 Inspect 恒返回该状态
	err      error        // 非 nil 时 Inspect 恒返回该错误
}

func (w *inspectOverrideRuntime) Inspect(pod metamanager.Pod, index int) (RuntimeState, error) {
	if w.err != nil {
		return StateUnknown, w.err
	}
	if w.override != 0 {
		return w.override, nil
	}
	return w.MockRuntime.Inspect(pod, index)
}
