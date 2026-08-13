package edged

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"edgeflow/edge/pkg/metamanager"
)

// ---------- 假命令执行器辅助 ----------

// fakeRunner 构造一个按调用顺序返回预设结果的假 docker 执行器。
// 每对 (out, err) 消费一次调用；超出的调用返回 ("", nil)（视为成功空输出）。
func fakeRunner(responses ...[]any) cmdRunner {
	i := 0
	return func(_ context.Context, _ ...string) (string, error) {
		if i < len(responses) {
			out, _ := responses[i][0].(string)
			err, _ := responses[i][1].(error)
			i++
			return out, err
		}
		return "", nil
	}
}

// 常用假错误：文本与 docker CLI 真实报错一致（首字母大写是 docker 原文，
// 不是 Go 错误规范要求——这是故意模拟，故加 nolint 豁免 staticcheck ST1005）。
var (
	errFakeDaemonDown = errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?") //nolint:staticcheck // 模拟 docker CLI 真实报错文本
	errFakeNoObject   = errors.New("Error: No such object: edgeflow-default-nginx")                                              //nolint:staticcheck // 模拟 docker CLI 真实报错文本
	errFakeNoCtr      = errors.New("Error: No such container: edgeflow-default-nginx")                                          //nolint:staticcheck // 模拟 docker CLI 真实报错文本
)

// newTestDocker 构造带假执行器的 DockerRuntime（超时缩短加速测试）。
func newTestDocker(runner cmdRunner) *DockerRuntime {
	return &DockerRuntime{timeout: time.Second, runner: runner}
}

// ---------- 错误归一化测试 ----------

// TestDockerDaemonUnavailablePrefix 验证：daemon 不可用时错误带
// "docker daemon unavailable: " 前缀（任务验收点）。
func TestDockerDaemonUnavailablePrefix(t *testing.T) {
	d := newTestDocker(fakeRunner(
		[]any{"", errFakeDaemonDown},
		[]any{"", errFakeDaemonDown},
		[]any{"", errFakeDaemonDown},
	))

	// EnsureRunning（内部先 Inspect）
	if err := d.EnsureRunning(podNginx()); err == nil {
		t.Fatal("daemon 不可用时 EnsureRunning 应报错")
	} else if !strings.HasPrefix(err.Error(), "docker daemon unavailable: ") {
		t.Errorf("错误缺少前缀: %v", err)
	}
	// Inspect
	if _, err := d.Inspect(podNginx()); err == nil || !strings.HasPrefix(err.Error(), "docker daemon unavailable: ") {
		t.Errorf("Inspect 错误缺少前缀: %v", err)
	}
	// List
	if _, err := d.List(); err == nil || !strings.HasPrefix(err.Error(), "docker daemon unavailable: ") {
		t.Errorf("List 错误缺少前缀: %v", err)
	}
}

// TestDockerInspectStates 验证 inspect 输出解析：true/false/异常输出。
func TestDockerInspectStates(t *testing.T) {
	d := newTestDocker(fakeRunner(
		[]any{"true\n", nil},
		[]any{"false\n", nil},
		[]any{"???", nil},
		[]any{"", errFakeNoObject},
		[]any{"", errFakeDaemonDown},
	))

	if st, err := d.Inspect(podNginx()); err != nil || st != StateRunning {
		t.Errorf("true → %s, err=%v，期望 running", st, err)
	}
	if st, err := d.Inspect(podNginx()); err != nil || st != StateStopped {
		t.Errorf("false → %s, err=%v，期望 stopped", st, err)
	}
	if st, err := d.Inspect(podNginx()); err == nil || st != StateUnknown {
		t.Errorf("异常输出 → %s, err=%v，期望 unknown+错误", st, err)
	}
	if st, err := d.Inspect(podNginx()); err != nil || st != StateAbsent {
		t.Errorf("No such object → %s, err=%v，期望 absent", st, err)
	}
	if _, err := d.Inspect(podNginx()); err == nil || !strings.HasPrefix(err.Error(), "docker daemon unavailable: ") {
		t.Errorf("daemon 不可用应透传错误: %v", err)
	}
}

// TestDockerEnsureRunningFlows 验证 EnsureRunning 三分支：
// 已运行 no-op、停止则 start、不存在则 run -d。
func TestDockerEnsureRunningFlows(t *testing.T) {
	var calls []string
	d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch {
		case args[0] == "inspect" && len(calls) == 1:
			return "true\n", nil // 已运行
		case args[0] == "inspect" && len(calls) == 2:
			return "false\n", nil // 存在但停止
		case args[0] == "start":
			return "", nil
		case args[0] == "inspect" && len(calls) == 4:
			return "", errFakeNoObject // 不存在
		case args[0] == "run":
			return "abc123\n", nil
		}
		return "", nil
	})

	// 1. 已运行 → 不执行 run/start
	if err := d.EnsureRunning(podNginx()); err != nil {
		t.Fatalf("已运行 EnsureRunning 失败: %v", err)
	}
	// 2. 停止 → docker start
	if err := d.EnsureRunning(podNginx()); err != nil {
		t.Fatalf("停止态 EnsureRunning 失败: %v", err)
	}
	// 3. 不存在 → docker run -d（带名称与标签）
	if err := d.EnsureRunning(podNginx()); err != nil {
		t.Fatalf("不存在 EnsureRunning 失败: %v", err)
	}

	runArgs := calls[len(calls)-1]
	for _, want := range []string{
		"run -d",
		"--name edgeflow-default-nginx",
		"--label " + labelPod + "=nginx",
		"--label " + labelNamespace + "=default",
		"nginx:1.27",
	} {
		if !strings.Contains(runArgs, want) {
			t.Errorf("docker run 参数缺少 %q: %s", want, runArgs)
		}
	}
	// start 被调用了一次（分支 2）
	if n := countPrefix(calls, "start "); n != 1 {
		t.Errorf("start 调用次数 = %d，期望 1", n)
	}
}

// TestDockerEnsureRunningConflictFallback 验证：并发创建导致名字冲突时
// 重新查询按已存在处理，不报错。
func TestDockerEnsureRunningConflictFallback(t *testing.T) {
	d := newTestDocker(fakeRunner(
		[]any{"", errFakeNoObject},                          // Inspect：不存在
		[]any{"", errors.New("Error response from daemon: Conflict. The container name \"/edgeflow-default-nginx\" is already in use by container abc")}, // run 冲突
		[]any{"true\n", nil},                                // 重新 Inspect：已运行
	))
	if err := d.EnsureRunning(podNginx()); err != nil {
		t.Errorf("名字冲突兜底应成功: %v", err)
	}
}

// TestDockerEnsureStoppedIdempotent 验证：rm -f 对不存在容器 no-op（幂等）。
func TestDockerEnsureStoppedIdempotent(t *testing.T) {
	d := newTestDocker(fakeRunner(
		[]any{"", errFakeNoCtr},
	))
	if err := d.EnsureStopped(podNginx()); err != nil {
		t.Errorf("对不存在容器 EnsureStopped 应 no-op: %v", err)
	}

	// daemon 不可用 ≠ 不存在：必须透传错误
	d2 := newTestDocker(fakeRunner([]any{"", errFakeDaemonDown}))
	if err := d2.EnsureStopped(podNginx()); err == nil || !strings.HasPrefix(err.Error(), "docker daemon unavailable: ") {
		t.Errorf("daemon 不可用应透传错误: %v", err)
	}
}

// TestDockerExecTimeout 验证：单次命令超时（context 到期）返回明确错误。
func TestDockerExecTimeout(t *testing.T) {
	d := newTestDocker(func(ctx context.Context, _ ...string) (string, error) {
		<-ctx.Done() // 阻塞到超时
		return "", ctx.Err()
	})
	d.timeout = 50 * time.Millisecond

	_, err := d.exec("ps")
	if err == nil {
		t.Fatal("超时场景应报错")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("超时错误应明确提示: %v", err)
	}
}

// TestDockerListParse 验证 List 解析：按标签反解 namespace/name。
func TestDockerListParse(t *testing.T) {
	out := strings.Join([]string{
		"abc\tnginx\tdefault",  // 正常
		"def\tredis\tprod",
		"",                      // 空行忽略
		"odd-line",              // 字段数不符忽略
	}, "\n") + "\n"
	d := newTestDocker(fakeRunner([]any{out, nil}))

	pods, err := d.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(pods) != 2 {
		t.Fatalf("List 结果数 = %d，期望 2: %+v", len(pods), pods)
	}
	if pods[0].Namespace != "default" || pods[0].Name != "nginx" {
		t.Errorf("pods[0] = %+v，期望 default/nginx", pods[0])
	}
	if pods[1].Namespace != "prod" || pods[1].Name != "redis" {
		t.Errorf("pods[1] = %+v，期望 prod/redis", pods[1])
	}
}

// TestDockerListEmpty 验证无容器时返回空切片（非 nil 错误）。
func TestDockerListEmpty(t *testing.T) {
	d := newTestDocker(fakeRunner([]any{"", nil}))
	pods, err := d.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(pods) != 0 {
		t.Errorf("空列表应返回空切片: %+v", pods)
	}
}

// ---------- 真实 Docker daemon 冒烟测试 ----------

// TestDockerRuntimeSmoke 是真实 Docker daemon 的端到端冒烟测试
// （环境变量 EDGED_DOCKER_SMOKE=1 时启用，否则跳过；daemon 不可用时跳过并注明）。
//
// 覆盖：创建容器（docker run -d + 标签）→ 幂等重试 → List 标签发现 →
// 停止/删除（docker rm -f）→ 幂等删除 → 状态机 Absent 收敛。
//
// 运行方式：
//
//	EDGED_DOCKER_SMOKE=1 go test -run TestDockerRuntimeSmoke -v ./edge/pkg/edged/
func TestDockerRuntimeSmoke(t *testing.T) {
	if os.Getenv("EDGED_DOCKER_SMOKE") == "" {
		t.Skip("冒烟测试需要 EDGED_DOCKER_SMOKE=1（真实 Docker daemon）")
	}

	d := NewDockerRuntime()
	if _, err := d.exec("version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skipf("docker daemon 不可用，跳过冒烟（环境未验证）: %v", err)
	}
	t.Logf("docker daemon 可用，开始冒烟")

	// 唯一命名，避免与历史残留冲突
	pod := metamanager.Pod{
		Namespace: "default",
		Name:      fmt.Sprintf("smoke-%d", time.Now().UnixNano()),
		Image:     "busybox:1.37.0", // 本机已缓存（docker images 可见），避免拉取耗时
	}

	// 预清理（幂等，防上次运行残留）
	if err := d.EnsureStopped(pod); err != nil {
		t.Fatalf("预清理失败: %v", err)
	}

	// 1. 创建：容器存在（busybox 默认命令立即退出，状态应为 stopped/created，
	//    但绝不是 absent——这正好验证"存在性"判断而非"运行中"判断）
	if err := d.EnsureRunning(pod); err != nil {
		t.Fatalf("EnsureRunning(创建) 失败: %v", err)
	}
	st, err := d.Inspect(pod)
	if err != nil {
		t.Fatalf("Inspect 失败: %v", err)
	}
	if st == StateAbsent {
		t.Fatalf("创建后状态 = absent，容器应存在")
	}
	t.Logf("创建后状态: %s", st)

	// 2. 幂等：再次 EnsureRunning 不报错、不产生第二个容器
	if err := d.EnsureRunning(pod); err != nil {
		t.Fatalf("EnsureRunning(幂等重试) 失败: %v", err)
	}

	// 3. List 标签发现：容器可被 Edged 的标签过滤找到，且 namespace/name 反解正确
	found := false
	pods, err := d.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	for _, p := range pods {
		if p.Name == pod.Name && p.Namespace == pod.Namespace {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("List 未找到冒烟容器（%s）: %+v", pod.Name, pods)
	}

	// 4. 删除：状态收敛回 Absent
	if err := d.EnsureStopped(pod); err != nil {
		t.Fatalf("EnsureStopped 失败: %v", err)
	}
	st, err = d.Inspect(pod)
	if err != nil {
		t.Fatalf("删除后 Inspect 失败: %v", err)
	}
	if st != StateAbsent {
		t.Fatalf("删除后状态 = %s，期望 absent", st)
	}

	// 5. 幂等删除：不存在时 no-op
	if err := d.EnsureStopped(pod); err != nil {
		t.Fatalf("EnsureStopped(幂等删除) 失败: %v", err)
	}

	// 6. 残留验证：List 不再包含该容器
	pods, err = d.List()
	if err != nil {
		t.Fatalf("List(残留检查) 失败: %v", err)
	}
	for _, p := range pods {
		if p.Name == pod.Name {
			t.Fatalf("删除后 List 仍包含容器: %+v", p)
		}
	}
	t.Logf("冒烟通过：创建→幂等→标签发现→删除→幂等删除 全流程 OK")
}

// ---------- 小工具 ----------

// podNginx 构造测试用 Pod。
func podNginx() metamanager.Pod {
	return metamanager.Pod{Namespace: "default", Name: "nginx", Image: "nginx:1.27"}
}

// countPrefix 统计以指定前缀开头的调用。
func countPrefix(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}
