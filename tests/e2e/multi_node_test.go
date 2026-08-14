package e2e

// 多节点端到端测试（WBS 8.3 验收缺口 G1）：
//   2 个 edgecore 实例（独立节点 ID / 独立本地数据库）注册到同一 cloudcore
//   → 云端 /api/v1/nodes 出现 2 个节点 → 各自下发 Pod → 各自容器创建并
//   上报 Running → 节点间状态隔离（互不串扰）。
//
// 前置条件：Docker daemon 可用且本地缓存 nginx:1.25-alpine（Pod 容器
// 生命周期断言需要真实运行时；不满足时 t.Skip）。
// 运行：go test -v -timeout 20m ./tests/e2e/ -run TestMultiNode
import (
	"fmt"
	"testing"

	"edgeflow/edge/pkg/edged"
)

// TestMultiNodeRegistrationAndPodSync 验证多节点注册与按节点下发 Pod。
func TestMultiNodeRegistrationAndPodSync(t *testing.T) {
	if !dockerOK() {
		t.Skip("需要 Docker daemon（容器创建断言）；无 Docker 环境请用 -short 跳过容器用例")
	}
	if !dockerImageCached(e2eImage) {
		t.Skipf("需要本地缓存镜像 %s（避免测试期间拉取）；请先执行 docker pull %s", e2eImage, e2eImage)
	}
	buildBinaries(t)
	root := repoRoot(t)

	// 1. 启动 cloudcore + 2 个 edgecore（独立 nodeID 与独立 DB 路径）
	cloud, httpPort, hubPort := startCloudcore(t, root)
	base := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	nodeA, nodeB := "e2e-mn-a", "e2e-mn-b"
	startEdgecore(t, root, nodeA, hubPort)
	startEdgecore(t, root, nodeB, hubPort)

	// 2. 两个节点都注册到云端
	waitNodeRegistered(t, base, nodeA)
	waitNodeRegistered(t, base, nodeB)
	t.Logf("节点 %s / %s 均已注册", nodeA, nodeB)

	// 3. 各自下发独立 Pod（nginx 长驻容器；Pod 名带唯一后缀，避免与并发/残留
	//    edgecore 的容器互相干扰——共享 Docker daemon 上的孤儿清理只针对
	//    不属于本节点的容器，名称唯一即可避免删除断言被外部进程干扰）
	podA := podNameWithSuffix("e2e-mn-a-pod")
	podB := podNameWithSuffix("e2e-mn-b-pod")
	syncPod(t, base, nodeA, "add", podA, e2eImage, 1)
	syncPod(t, base, nodeB, "add", podB, e2eImage, 1)

	// 4. 容器创建：docker 层确认 + 云端状态上报 Running
	cnameA := edged.ContainerName("default", podA, 0)
	cnameB := edged.ContainerName("default", podB, 0)
	waitContainerRunning(t, cnameA)
	waitContainerRunning(t, cnameB)
	t.Logf("容器已创建并运行：%s / %s", cnameA, cnameB)
	waitPodPhase(t, base, nodeA, podA, "Running")
	waitPodPhase(t, base, nodeB, podB, "Running")

	// 5. 节点隔离：nodeA 的 Pod 列表只含 podA（不含 podB），反之亦然
	var listA, listB podStatusList
	getJSON(t, base+"/api/v1/nodes/"+nodeA+"/pods", &listA)
	getJSON(t, base+"/api/v1/nodes/"+nodeB+"/pods", &listB)
	if len(listA.Items) != 1 || listA.Items[0].PodName != podA {
		t.Errorf("nodeA Pod 列表 = %+v，期望只含 %s（多节点隔离）", listA.Items, podA)
	}
	if len(listB.Items) != 1 || listB.Items[0].PodName != podB {
		t.Errorf("nodeB Pod 列表 = %+v，期望只含 %s（多节点隔离）", listB.Items, podB)
	}
	t.Logf("节点隔离验证通过：%s→%s，%s→%s", nodeA, podA, nodeB, podB)

	// 6. 收尾：删除两个 Pod → 各自容器移除
	syncPod(t, base, nodeA, "delete", podA, "", 0)
	syncPod(t, base, nodeB, "delete", podB, "", 0)
	waitContainerGone(t, cnameA)
	waitContainerGone(t, cnameB)
	t.Logf("删除收敛：两个容器均已移除")
	_ = cloud
}
