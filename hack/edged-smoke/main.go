// edged-smoke 是 Edged DockerRuntime 的真实环境冒烟工具（开发用）。
// 用法：go run ./hack/edged-smoke（需要 Docker daemon 运行）
package main

import (
	"fmt"
	"os"

	"edgeflow/edge/pkg/edged"
	"edgeflow/edge/pkg/metamanager"
)

func main() {
	pod := metamanager.Pod{Name: "smoke-nginx", Namespace: "default", Image: "nginx:1.25-alpine", Replicas: 1}
	rt := edged.NewDockerRuntime()

	fmt.Println("== 1. EnsureRunning（创建并启动）==")
	if err := rt.EnsureRunning(pod); err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("ok")

	fmt.Println("== 2. Inspect（应 Running）==")
	state, err := rt.Inspect(pod)
	if err != nil || state != edged.StateRunning {
		fmt.Printf("FAIL: state=%v err=%v\n", state, err)
		os.Exit(1)
	}
	fmt.Println("ok: state=", state)

	fmt.Println("== 3. EnsureRunning 幂等（重复调用不报错）==")
	if err := rt.EnsureRunning(pod); err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("ok")

	fmt.Println("== 4. List（标签发现）==")
	pods, err := rt.List()
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	found := false
	for _, p := range pods {
		if p.Name == pod.Name && p.Namespace == pod.Namespace {
			found = true
		}
	}
	fmt.Printf("ok: 发现 %d 个 edged 容器, smoke-nginx 存在=%v\n", len(pods), found)

	fmt.Println("== 5. EnsureStopped（停止并移除）==")
	if err := rt.EnsureStopped(pod); err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("ok")

	fmt.Println("== 6. EnsureStopped 幂等（容器已不存在不报错）==")
	if err := rt.EnsureStopped(pod); err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("ok")

	fmt.Println("SMOKE PASS")
}
