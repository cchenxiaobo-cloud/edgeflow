// T-10（CHN-05）外部 docker 干预容错单测（v0.22.0）。
//
// 场景：外部 docker 客户端在调谐周期之间直接干预容器（docker rm -f /
// docker stop / daemon 抖动），边缘调谐循环的 List 视图随之变化。验收目标：
// reconcile 对「视图与实际状态瞬时不一致」具备容错性——
//   - 容器从 List 视图消失（外部删除）→ 下轮调谐按「副本缺失」幂等补齐；
//   - 外部停止容器 → 健康检查自愈重启；
//   - List 失败（daemon 不可用）→ 跳过孤儿清理但不得基于空视图误杀容器
//     （CHN-05 核心：固化/失真快照最危险路径），下轮恢复后正常收敛；
//   - 创建失败（镜像拉取失败等外部原因）→ 记入状态表 StateUnknown 且
//     保留错误，恢复后自愈。
//
// 驱动方式：Edged 导出装配 API（New + Start/Trigger/Stop + Status，
// reconcileOnce 未导出故走真实循环）；运行时用 MockRuntime 的
// SetState/DeleteState/SetListErr/SetFail 模拟外部干预（与
// edge/pkg/edged/mock_runtime.go 既有能力一致，零新 mock 代码）。
// 不改动任何既有测试文件。
//
// ⚠️ 锚点说明（T-10 验收 ①②）：任务描述锚点 cmd/edgecore/device_mapper.go
// 实为设备 Mapper 装配层，与 docker 无关；docker 迁移循环（Index<0）真实
// 位置 edge/pkg/edged/edged.go reconcileOnce 第 3d 步，不在本次白名单内。
// 「90s 固化 List 快照」在代码库中不存在（DockerRuntime.List 每次 exec
// docker ps 即时执行；90s 实为 DefaultRemovedRetention Absent 保留窗口）。
// 验收 ①② 需改 edged.go，越界报主线裁决（详见 subagent_02.md）。
package e2e

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"edgeflow/edge/pkg/edged"
	"edgeflow/edge/pkg/metamanager"
)

// newReconcileTestEdged 构造独立 SQLite + MockRuntime 的 Edged（仅本文件用例）。
// 调谐周期取极小值，用例通过 Trigger + 轮询驱动真实循环。
func newReconcileTestEdged(t *testing.T) (*edged.Edged, *edged.MockRuntime, *metamanager.Store) {
	t.Helper()
	s, err := metamanager.Open(filepath.Join(t.TempDir(), "edgeflow.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	rt := edged.NewMockRuntime()
	e := edged.New(s, rt, 10*time.Millisecond)
	return e, rt, s
}

// mustSavePod 构造并落盘一个期望 Pod（replicas=1，无资源 limit）。
func mustSavePod(t *testing.T, s *metamanager.Store, ns, name string) {
	t.Helper()
	pod := map[string]any{
		"name":      name,
		"namespace": ns,
		"image":     "nginx:1.25-alpine",
		"replicas":  1,
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("序列化 Pod 失败: %v", err)
	}
	if err := s.SavePod(string(raw)); err != nil {
		t.Fatalf("SavePod 失败: %v", err)
	}
}

// waitRuntimeState 轮询等待实例达到期望状态（真实循环异步收敛）。
func waitRuntimeState(t *testing.T, rt *edged.MockRuntime, key string, want edged.RuntimeState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rt.State(key) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %s 达到 %s 超时（实际 %s）", key, want, rt.State(key))
}

// TestReconcileToleratesExternalContainerRemoval 验收③主用例：
// 外部 docker 删除容器（容器从 List 视图消失）→ 下轮调谐幂等补齐重建。
func TestReconcileToleratesExternalContainerRemoval(t *testing.T) {
	e, rt, s := newReconcileTestEdged(t)
	mustSavePod(t, s, "default", "web")
	e.Start()
	defer e.Stop()

	key0 := "default/web#0"
	waitRuntimeState(t, rt, key0, edged.StateRunning)

	// 外部 docker 干预：直接删除容器（DeleteState 等价于容器从 List 视图
	// 消失，List 不再返回该实例、Inspect 报 Absent）
	rt.DeleteState(key0)

	// 下轮调谐：声明式收敛应把缺失副本重新拉起
	waitRuntimeState(t, rt, key0, edged.StateRunning)
}

// TestReconcileToleratesExternalStop 验收③：外部 docker stop 容器 →
// 健康检查自愈重启（Stopped → EnsureRunning → Running）。
func TestReconcileToleratesExternalStop(t *testing.T) {
	e, rt, s := newReconcileTestEdged(t)
	mustSavePod(t, s, "default", "web")
	e.Start()
	defer e.Stop()

	key0 := "default/web#0"
	waitRuntimeState(t, rt, key0, edged.StateRunning)

	rt.SetState(key0, edged.StateStopped) // 模拟外部 docker stop
	waitRuntimeState(t, rt, key0, edged.StateRunning)
}

// TestReconcileListFailureNoOrphanKill 验收③（CHN-05 核心安全断言）：
// List 失败（daemon 不可用）→ 本轮跳过孤儿清理，绝不基于失真视图删除
// 容器（EnsureStopped 计数不增）；恢复后正常收敛。
func TestReconcileListFailureNoOrphanKill(t *testing.T) {
	e, rt, s := newReconcileTestEdged(t)
	mustSavePod(t, s, "default", "web")
	e.Start()
	defer e.Stop()

	key0 := "default/web#0"
	waitRuntimeState(t, rt, key0, edged.StateRunning)

	rt.SetListErr(errors.New("docker ps 失败（模拟 daemon 不可用）"))
	e.Trigger()
	time.Sleep(100 * time.Millisecond) // 留出至少一轮调谐
	if n := rt.EnsureStoppedCount(key0); n != 0 {
		t.Fatalf("List 失败轮次不得基于空视图清理容器，EnsureStopped 被调用 %d 次", n)
	}

	rt.SetListErr(nil) // daemon 恢复
	waitRuntimeState(t, rt, key0, edged.StateRunning)
}

// TestReconcileCreateFailureRecordedAndRecovers 验收③：外部原因导致创建
// 失败（镜像拉取失败等）→ 记入状态表 StateUnknown 且保留错误（可诊断），
// 干预解除后自愈。
func TestReconcileCreateFailureRecordedAndRecovers(t *testing.T) {
	e, rt, s := newReconcileTestEdged(t)
	mustSavePod(t, s, "default", "web")

	rt.SetFail("default/web#0", errors.New("拉取镜像失败（模拟外部干预）"))
	e.Start()
	defer e.Stop()

	// 创建失败：状态表记录 Unknown + 错误（轮询等待循环至少跑一轮）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := e.Status()["default/web"]; ok && st.Err != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, ok := e.Status()["default/web"]
	if !ok {
		t.Fatal("状态表应包含 default/web 条目")
	}
	if st.Err == nil {
		t.Fatal("创建失败应记入状态表错误（可诊断性）")
	}

	// 干预解除：下一轮自愈为 Running
	rt.SetFail("default/web#0", nil)
	waitRuntimeState(t, rt, "default/web#0", edged.StateRunning)
}
