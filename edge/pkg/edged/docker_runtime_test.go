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
	errFakeNoObject   = errors.New("Error: No such object: edgeflow-default-nginx-0")                                                   //nolint:staticcheck // 模拟 docker CLI 真实报错文本
	errFakeNoCtr      = errors.New("Error: No such container: edgeflow-default-nginx-0")                                                //nolint:staticcheck // 模拟 docker CLI 真实报错文本
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
	if err := d.EnsureRunning(podNginx(), 0); err == nil {
		t.Fatal("daemon 不可用时 EnsureRunning 应报错")
	} else if !strings.HasPrefix(err.Error(), "docker daemon unavailable: ") {
		t.Errorf("错误缺少前缀: %v", err)
	}
	// Inspect
	if _, err := d.Inspect(podNginx(), 0); err == nil || !strings.HasPrefix(err.Error(), "docker daemon unavailable: ") {
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

	if st, err := d.Inspect(podNginx(), 0); err != nil || st != StateRunning {
		t.Errorf("true → %s, err=%v，期望 running", st, err)
	}
	if st, err := d.Inspect(podNginx(), 0); err != nil || st != StateStopped {
		t.Errorf("false → %s, err=%v，期望 stopped", st, err)
	}
	if st, err := d.Inspect(podNginx(), 0); err == nil || st != StateUnknown {
		t.Errorf("异常输出 → %s, err=%v，期望 unknown+错误", st, err)
	}
	if st, err := d.Inspect(podNginx(), 0); err != nil || st != StateAbsent {
		t.Errorf("No such object → %s, err=%v，期望 absent", st, err)
	}
	if _, err := d.Inspect(podNginx(), 0); err == nil || !strings.HasPrefix(err.Error(), "docker daemon unavailable: ") {
		t.Errorf("daemon 不可用应透传错误: %v", err)
	}
}

// TestDockerEnsureRunningFlows 验证 EnsureRunning 三分支（副本 0）：
// 已运行 no-op、停止则 start、不存在则 run -d（容器名带 -0 后缀）。
func TestDockerEnsureRunningFlows(t *testing.T) {
	var calls []string
	d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		// 调用序列：inspect(1)→true；inspect(2)→false；start(3)；inspect(4)→不存在；run(5)
		switch args[0] {
		case "inspect":
			switch len(calls) {
			case 1:
				return "true\n", nil // 已运行
			case 2:
				return "false\n", nil // 存在但停止
			case 4:
				return "", errFakeNoObject // 不存在
			}
		case "start":
			return "", nil
		case "run":
			return "abc123\n", nil
		}
		return "", nil
	})

	// 1. 已运行 → 不执行 run/start
	if err := d.EnsureRunning(podNginx(), 0); err != nil {
		t.Fatalf("已运行 EnsureRunning 失败: %v", err)
	}
	// 2. 停止 → docker start
	if err := d.EnsureRunning(podNginx(), 0); err != nil {
		t.Fatalf("停止态 EnsureRunning 失败: %v", err)
	}
	// 3. 不存在 → docker run -d（带名称与标签）
	if err := d.EnsureRunning(podNginx(), 0); err != nil {
		t.Fatalf("不存在 EnsureRunning 失败: %v", err)
	}

	runArgs := calls[len(calls)-1]
	for _, want := range []string{
		"run -d",
		"--name edgeflow-default-nginx-0",
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

// TestDockerEnsureRunningReplicaIndex 验证多副本：EnsureRunning(pod, 1)
// 操作的是带 -1 后缀的独立容器。
func TestDockerEnsureRunningReplicaIndex(t *testing.T) {
	var calls []string
	d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "inspect":
			return "", errFakeNoObject // 副本 1 不存在
		case "run":
			return "abc123\n", nil
		}
		return "", nil
	})

	if err := d.EnsureRunning(podNginx(), 1); err != nil {
		t.Fatalf("EnsureRunning(副本 1) 失败: %v", err)
	}
	runArgs := calls[len(calls)-1]
	if !strings.Contains(runArgs, "--name edgeflow-default-nginx-1") {
		t.Errorf("副本 1 容器名应为 edgeflow-default-nginx-1: %s", runArgs)
	}
}

// TestDockerEnsureRunningConflictFallback 验证：并发创建导致名字冲突时
// 重新查询按已存在处理，不报错。
func TestDockerEnsureRunningConflictFallback(t *testing.T) {
	d := newTestDocker(fakeRunner(
		[]any{"", errFakeNoObject}, // Inspect：不存在
		[]any{"", errors.New("Error response from daemon: Conflict. The container name \"/edgeflow-default-nginx-0\" is already in use by container abc")}, // run 冲突
		[]any{"true\n", nil}, // 重新 Inspect：已运行
	))
	if err := d.EnsureRunning(podNginx(), 0); err != nil {
		t.Errorf("名字冲突兜底应成功: %v", err)
	}
}

// TestDockerEnsureStoppedIdempotent 验证：rm -f 对不存在容器 no-op（幂等）。
func TestDockerEnsureStoppedIdempotent(t *testing.T) {
	d := newTestDocker(fakeRunner(
		[]any{"", errFakeNoCtr},
	))
	if err := d.EnsureStopped(podNginx(), 0); err != nil {
		t.Errorf("对不存在容器 EnsureStopped 应 no-op: %v", err)
	}

	// daemon 不可用 ≠ 不存在：必须透传错误
	d2 := newTestDocker(fakeRunner([]any{"", errFakeDaemonDown}))
	if err := d2.EnsureStopped(podNginx(), 0); err == nil || !strings.HasPrefix(err.Error(), "docker daemon unavailable: ") {
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

// TestDockerListParse 验证 List 解析：按标签反解 namespace/name，
// 副本序号从容器名末段解析；旧式无序号命名兜底 0。
func TestDockerListParse(t *testing.T) {
	out := strings.Join([]string{
		"edgeflow-default-nginx-0\tnginx\tdefault", // 副本 0
		"edgeflow-default-nginx-1\tnginx\tdefault", // 副本 1
		"edgeflow-prod-redis-3\tredis\tprod",       // 副本 3（缩容前的残余也正确识别）
		"edgeflow-default-legacy\tlegacy\tdefault", // 旧式命名（无序号）→ 兜底 0
		"",         // 空行忽略
		"odd-line", // 字段数不符忽略
	}, "\n") + "\n"
	d := newTestDocker(fakeRunner([]any{out, nil}))

	insts, err := d.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(insts) != 4 {
		t.Fatalf("List 结果数 = %d，期望 4: %+v", len(insts), insts)
	}
	want := []InstanceRef{
		{Namespace: "default", Name: "nginx", Index: 0},
		{Namespace: "default", Name: "nginx", Index: 1},
		{Namespace: "prod", Name: "redis", Index: 3},
		{Namespace: "default", Name: "legacy", Index: 0},
	}
	for i, w := range want {
		if insts[i] != w {
			t.Errorf("insts[%d] = %+v，期望 %+v", i, insts[i], w)
		}
	}
}

// TestDockerListEmpty 验证无容器时返回空切片（非 nil 错误）。
func TestDockerListEmpty(t *testing.T) {
	d := newTestDocker(fakeRunner([]any{"", nil}))
	insts, err := d.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(insts) != 0 {
		t.Errorf("空列表应返回空切片: %+v", insts)
	}
}

// TestParseIndexFromName 验证副本序号反解：-<index> 恒在末尾可解析；
// 无序号/非法序号兜底 0。
func TestParseIndexFromName(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"edgeflow-default-nginx-0", 0},
		{"edgeflow-default-nginx-1", 1},
		{"edgeflow-default-nginx-10", 10},
		{"edgeflow-default-nginx", 0},     // 旧式命名（无序号）
		{"edgeflow-default-nginx-x", 0},   // 末段非数字
		{"edgeflow-default-nginx-", 0},    // 以 '-' 结尾（末段为空）
		{"edgeflow-default-nginx-abc", 0}, // 末段为字母
		{"abc", 0},
	}
	for _, c := range cases {
		if got := parseIndexFromName(c.name); got != c.want {
			t.Errorf("parseIndexFromName(%q) = %d，期望 %d", c.name, got, c.want)
		}
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
	if err := d.EnsureStopped(pod, 0); err != nil {
		t.Fatalf("预清理失败: %v", err)
	}

	// 1. 创建：容器存在（busybox 默认命令立即退出，状态应为 stopped/created，
	//    但绝不是 absent——这正好验证"存在性"判断而非"运行中"判断）
	if err := d.EnsureRunning(pod, 0); err != nil {
		t.Fatalf("EnsureRunning(创建) 失败: %v", err)
	}
	st, err := d.Inspect(pod, 0)
	if err != nil {
		t.Fatalf("Inspect 失败: %v", err)
	}
	if st == StateAbsent {
		t.Fatalf("创建后状态 = absent，容器应存在")
	}
	t.Logf("创建后状态: %s", st)

	// 2. 幂等：再次 EnsureRunning 不报错、不产生第二个容器
	if err := d.EnsureRunning(pod, 0); err != nil {
		t.Fatalf("EnsureRunning(幂等重试) 失败: %v", err)
	}

	// 3. List 标签发现：容器可被 Edged 的标签过滤找到，且 namespace/name 反解正确
	found := false
	insts, err := d.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	for _, inst := range insts {
		if inst.Name == pod.Name && inst.Namespace == pod.Namespace && inst.Index == 0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("List 未找到冒烟容器（%s）: %+v", pod.Name, insts)
	}

	// 4. 删除：状态收敛回 Absent
	if err := d.EnsureStopped(pod, 0); err != nil {
		t.Fatalf("EnsureStopped 失败: %v", err)
	}
	st, err = d.Inspect(pod, 0)
	if err != nil {
		t.Fatalf("删除后 Inspect 失败: %v", err)
	}
	if st != StateAbsent {
		t.Fatalf("删除后状态 = %s，期望 absent", st)
	}

	// 5. 幂等删除：不存在时 no-op
	if err := d.EnsureStopped(pod, 0); err != nil {
		t.Fatalf("EnsureStopped(幂等删除) 失败: %v", err)
	}

	// 6. 残留验证：List 不再包含该容器
	insts, err = d.List()
	if err != nil {
		t.Fatalf("List(残留检查) 失败: %v", err)
	}
	for _, inst := range insts {
		if inst.Name == pod.Name {
			t.Fatalf("删除后 List 仍包含容器: %+v", inst)
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
