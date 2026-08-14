// edged-smoke 是 Edged DockerRuntime 的真实环境端到端冒烟工具（开发用）。
//
// 覆盖（WBS 6.4/6.5）：
//  1. 多副本：落盘 replicas=2 的 Pod → 调谐后 docker ps 可见
//     edgeflow-default-smoke-nginx-0 与 -1 两个副本容器；
//  2. 健康检查自愈：外部 docker stop 掉副本 1 → 调谐自动重启它
//     （Status().RestartCount 累加）；
//  3. 清理：删除期望 Pod → 全部副本容器被移除。
//
// 用法：go run ./hack/edged-smoke（需要 Docker daemon 运行；nginx:1.25-alpine
// 建议本机已缓存；调谐周期 1s，等待超时 15s）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"edgeflow/edge/pkg/edged"
	"edgeflow/edge/pkg/metamanager"
)

// fail 打印错误并退出（冒烟工具约定：任何一步失败即 FAIL 退出）。
func fail(format string, args ...any) {
	fmt.Printf("FAIL: "+format+"\n", args...)
	os.Exit(1)
}

// runDocker 执行一条 docker CLI 命令（真实 daemon）。
func runDocker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// waitUntil 轮询等待条件成立（默认 15s 超时，覆盖 1s 调谐周期 + docker 操作耗时）。
func waitUntil(what string, cond func() bool) bool {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("FAIL: 等待超时: %s\n", what)
	return false
}

// waitInstances 轮询等 List 结果达到期望实例数，返回最终结果。
func waitInstances(rt *edged.DockerRuntime, want int) ([]edged.InstanceRef, bool) {
	var insts []edged.InstanceRef
	ok := waitUntil(fmt.Sprintf("实例数=%d", want), func() bool {
		list, err := rt.List()
		if err != nil {
			return false
		}
		insts = list
		return len(list) == want
	})
	return insts, ok
}

// waitRunning 轮询等副本恢复 Running（健康自愈断言）。
func waitRunning(rt *edged.DockerRuntime, pod metamanager.Pod, index int) bool {
	return waitUntil(fmt.Sprintf("副本 %d Running", index), func() bool {
		st, err := rt.Inspect(pod, index)
		return err == nil && st == edged.StateRunning
	})
}

func main() {
	// 0. 装配：临时目录 Store + DockerRuntime + Edged（1s 调谐周期）
	dir, err := os.MkdirTemp("", "edged-smoke-*")
	if err != nil {
		fail("创建临时目录失败: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	store, err := metamanager.Open(filepath.Join(dir, "edgeflow.db"))
	if err != nil {
		fail("打开 Store 失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	pod := metamanager.Pod{Name: "smoke-nginx", Namespace: "default", Image: "nginx:1.25-alpine", Replicas: 2}
	podJSON, err := json.Marshal(pod)
	if err != nil {
		fail("序列化 Pod 失败: %v", err)
	}
	if err := store.SavePod(string(podJSON)); err != nil {
		fail("SavePod 失败: %v", err)
	}

	rt := edged.NewDockerRuntime()
	e := edged.New(store, rt, time.Second)
	e.Start()
	defer e.Stop()

	// 1. 多副本：等 reconcile 收敛到 2 个副本容器
	fmt.Println("== 1. 多副本（replicas=2）：等待 2 个副本容器 ==")
	insts, ok := waitInstances(rt, 2)
	if !ok {
		fail("未收敛到 2 个副本，当前: %+v", insts)
	}
	for _, inst := range insts {
		fmt.Printf("   发现实例: %s/%s 副本 %d\n", inst.Namespace, inst.Name, inst.Index)
	}
	// docker ps 应能看到 edgeflow-default-smoke-nginx-0 / -1
	out, err := runDocker("ps", "-a", "--filter", "name=edgeflow-default-smoke-nginx", "--format", "{{.Names}}\t{{.Status}}")
	if err != nil {
		fail("docker ps 失败: %v (%s)", err, out)
	}
	fmt.Printf("   docker ps 输出:\n%s\n", out)
	for _, want := range []string{"edgeflow-default-smoke-nginx-0", "edgeflow-default-smoke-nginx-1"} {
		if !containsLine(out, want) {
			fail("docker ps 未找到期望容器 %s:\n%s", want, out)
		}
	}
	fmt.Println("ok: 2 个副本容器均已创建（edgeflow-default-smoke-nginx-0 / -1）")

	// 2. 健康检查自愈：外部停掉副本 1 → 等 reconcile 自动重启
	fmt.Println("== 2. 健康检查自愈：docker stop 副本 1 ==")
	name1 := edged.ContainerName(pod.Namespace, pod.Name, 1)
	if out, err := runDocker("stop", name1); err != nil {
		fail("docker stop %s 失败: %v (%s)", name1, err, out)
	}
	if st, _ := rt.Inspect(pod, 1); st != edged.StateStopped {
		fail("前置条件：副本 1 应处于 stopped，实际 %s", st)
	}
	fmt.Printf("   副本 1 已停止，等待 reconcile 自动重启...\n")
	if !waitRunning(rt, pod, 1) {
		fail("副本 1 未被自动重启")
	}
	status := e.Status()
	st := status[pod.Namespace+"/"+pod.Name]
	if st.RestartCount < 1 {
		fail("期望 RestartCount >= 1，实际 %+v", st)
	}
	fmt.Printf("ok: 副本 1 已自动重启（RestartCount=%d）\n", st.RestartCount)

	// 3. 清理：删除期望 Pod → 全部副本容器被移除
	fmt.Println("== 3. 清理：删除期望 Pod ==")
	if err := store.DeletePod(pod.Namespace, pod.Name); err != nil {
		fail("DeletePod 失败: %v", err)
	}
	insts, ok = waitInstances(rt, 0)
	if !ok {
		fail("副本容器未清理干净，剩余: %+v", insts)
	}
	if out, err := runDocker("ps", "-a", "--filter", "name=edgeflow-default-smoke-nginx", "--format", "{{.Names}}"); err != nil {
		fail("docker ps(残留检查) 失败: %v (%s)", err, out)
	} else if strings.TrimSpace(out) != "" {
		fail("docker ps 仍有残留容器:\n%s", out)
	}
	fmt.Println("ok: 所有副本容器已清理，docker ps 无残留")

	fmt.Println("SMOKE PASS")
}

// containsLine 判断多行输出中是否有行包含目标子串（docker ps 输出为
// "<容器名>\t<状态>"，按容器名子串匹配）。
func containsLine(out, want string) bool {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(strings.TrimSpace(line), want) {
			return true
		}
	}
	return false
}
