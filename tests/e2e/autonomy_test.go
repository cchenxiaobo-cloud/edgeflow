package e2e

// 自治场景端到端测试（WBS 8.3 验收缺口 G1）：
//   启动 cloudcore + edgecore → 下发 Pod → 停 cloudcore → 等待 60s
//   （短时模拟 30min 自治语义）→ 验证容器仍运行 → 重启 cloudcore →
//   验证节点重连与状态恢复同步。
//
// 30min 时长说明：断网自治的验收语义是"云端不可达期间容器持续运行、
// 云端恢复后状态重新同步"。本用例以 60s 短时窗口验证该语义（判定逻辑
// 与 30min 完全一致）；完整 30min 时长需在真实长期运行环境中验证
// （见 docs/PERFORMANCE-BASELINE.md 的说明）。
//
// 前置条件：Docker daemon 可用且本地缓存 nginx:1.25-alpine
// （不满足时 t.Skip；镜像缓存避免测试期间拉取的耗时与网络依赖）。
// 运行：go test -v -timeout 20m ./tests/e2e/ -run TestAutonomy
import (
	"fmt"
	"testing"
	"time"

	"edgeflow/edge/pkg/edged"
)

// TestAutonomyCloudDisconnect 验证断网自治与恢复同步。
func TestAutonomyCloudDisconnect(t *testing.T) {
	// 前置条件：容器生命周期断言依赖 Docker daemon 与本地缓存镜像
	if !dockerOK() {
		t.Skip("需要 Docker daemon（容器运行状态断言）；无 Docker 环境请用 -short 跳过容器用例")
	}
	if !dockerImageCached(e2eImage) {
		t.Skipf("需要本地缓存镜像 %s（避免测试期间拉取）；请先执行 docker pull %s", e2eImage, e2eImage)
	}
	buildBinaries(t)
	root := repoRoot(t)

	// 1. 启动 cloudcore + edgecore
	cloud, httpPort, hubPort := startCloudcore(t, root)
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeID := "e2e-auto-1"
	startEdgecore(t, root, nodeID, hubPort)

	// 2. 节点注册
	waitNodeRegistered(t, base, nodeID)
	t.Logf("节点 %s 已注册", nodeID)

	// 3. 下发 Pod（nginx 长驻容器）→ 云端确认 + docker 层确认运行
	//    Pod 名带唯一后缀：并发/残留 edgecore 无法用同名容器干扰本用例
	podName := podNameWithSuffix("e2e-autonomy")
	syncPod(t, base, nodeID, "add", podName, e2eImage, 1)
	cname := edged.ContainerName("default", podName, 0)
	waitContainerRunning(t, cname)
	waitPodPhase(t, base, nodeID, podName, "Running")
	t.Logf("Pod %s 已下发并运行（容器 %s）", podName, cname)

	// 4. 停 cloudcore → 自治窗口开始
	cloud.stop()
	t.Logf("cloudcore 已停止，自治窗口 60s（短时模拟 30min 语义）...")
	time.Sleep(60 * time.Second)

	// 5. 自治窗口内：容器仍运行（edged 本地调谐不受云端不可达影响）
	if !dockerContainerRunning(t, cname) {
		t.Fatalf("自治窗口内容器 %s 未运行（云端断开 60s 后容器应持续运行）", cname)
	}
	t.Logf("自治窗口通过：云端断开 60s 后容器 %s 仍运行", cname)

	// 6. 重启 cloudcore（复用原端口，edgecore 重连地址不变）→ 节点重连注册
	cloud2 := startCloudcoreOnPorts(t, root, httpPort, hubPort)
	_ = cloud2
	waitNodeRegistered(t, base, nodeID)
	t.Logf("cloudcore 已重启，节点 %s 重新注册", nodeID)

	// 7. 状态恢复同步：Pod 状态重新上报为 Running（edgecore 全程未重启，
	//    恢复同步 = 重连后状态上报闭环恢复）
	waitPodPhase(t, base, nodeID, podName, "Running")
	t.Logf("状态恢复同步：Pod %s 重新上报 Running", podName)

	// 8. 收尾：删除 Pod → 边缘孤儿清理 → 容器移除（docker 层确认）
	syncPod(t, base, nodeID, "delete", podName, "", 0)
	waitContainerGone(t, cname)
	t.Logf("删除收敛：容器 %s 已移除", cname)
}
