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
// 已运行（且镜像一致）no-op、停止则 start、不存在则 run -d（容器名带 -0 后缀）。
// 已运行分支会多一次镜像比对 inspect（WBS 6.4），此处用独立计数器区分。
func TestDockerEnsureRunningFlows(t *testing.T) {
	var calls []string
	stateInspects := 0
	d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		// 镜像比对 inspect：与状态 inspect 区分（模板含 Config.Image）
		if args[0] == "inspect" && strings.Contains(strings.Join(args, " "), "Config.Image") {
			return "nginx:1.27\n", nil // 镜像与期望一致
		}
		// 状态 inspect：按调用顺序返回 已运行/已停止/不存在
		switch args[0] {
		case "inspect":
			stateInspects++
			switch stateInspects {
			case 1:
				return "true\n", nil // 已运行
			case 2:
				return "false\n", nil // 存在但停止
			case 3:
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

// TestDockerEnsureRunningImageDrift 验证 WBS 6.4 镜像漂移：
// 已运行但镜像与期望不一致 → 先 rm -f（EnsureStopped）再重新 run -d（重建）；
// 镜像一致 → 只有 inspect，无 rm/run（no-op）。
func TestDockerEnsureRunningImageDrift(t *testing.T) {
	t.Run("镜像一致→no-op", func(t *testing.T) {
		var calls []string
		d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			switch args[0] {
			case "inspect":
				if strings.Contains(strings.Join(args, " "), "Config.Image") {
					return "nginx:1.27\n", nil // 镜像一致
				}
				return "true\n", nil // 状态：已运行
			}
			return "", nil
		})

		if err := d.EnsureRunning(podNginx(), 0); err != nil {
			t.Fatalf("镜像一致 EnsureRunning 失败: %v", err)
		}
		if n := countPrefix(calls, "rm "); n != 0 {
			t.Errorf("镜像一致不应 rm，实际 %d 次: %v", n, calls)
		}
		if n := countPrefix(calls, "run "); n != 0 {
			t.Errorf("镜像一致不应 run，实际 %d 次: %v", n, calls)
		}
	})

	t.Run("镜像不一致→重建（顺序断言）", func(t *testing.T) {
		var calls []string
		d := newTestDocker(func(_ context.Context, args ...string) (string, error) {
			calls = append(calls, args[0])
			switch {
			case len(calls) == 1: // 状态 inspect
				return "true\n", nil
			case len(calls) == 2: // 镜像 inspect
				return "nginx:1.26\n", nil
			case len(calls) == 3: // rm -f
				return "", nil
			case len(calls) == 4: // 重建前状态 inspect
				return "", errFakeNoObject
			default: // run -d
				return "abc123\n", nil
			}
		})

		if err := d.EnsureRunning(podNginx(), 0); err != nil {
			t.Fatalf("镜像漂移重建失败: %v", err)
		}
		want := []string{"inspect", "inspect", "rm", "inspect", "run"}
		if len(calls) != len(want) {
			t.Fatalf("调用次数 = %d，期望 %d: %v", len(calls), len(want), calls)
		}
		for i, w := range want {
			if calls[i] != w {
				t.Errorf("第 %d 次调用 = %q，期望 %q（顺序：先停再建）", i+1, calls[i], w)
			}
		}
	})
}

// TestDockerImageMatches 验证 ImageMatches 各分支：
// 镜像一致→true；不一致→false；容器不存在→(false, nil)；daemon 不可用→错误。
func TestDockerImageMatches(t *testing.T) {
	d := newTestDocker(fakeRunner(
		[]any{"nginx:1.27\n", nil},
		[]any{"nginx:1.26\n", nil},
		[]any{"", errFakeNoObject},
		[]any{"", errFakeDaemonDown},
	))

	// 一致
	if ok, err := d.ImageMatches(podNginx(), 0); err != nil || !ok {
		t.Errorf("镜像一致 → ok=%v, err=%v，期望 true,nil", ok, err)
	}
	// 不一致
	if ok, err := d.ImageMatches(podNginx(), 0); err != nil || ok {
		t.Errorf("镜像不一致 → ok=%v, err=%v，期望 false,nil", ok, err)
	}
	// 容器不存在：不算漂移
	if ok, err := d.ImageMatches(podNginx(), 0); err != nil || ok {
		t.Errorf("容器不存在 → ok=%v, err=%v，期望 false,nil", ok, err)
	}
	// daemon 不可用：错误透传
	if _, err := d.ImageMatches(podNginx(), 0); err == nil || !strings.HasPrefix(err.Error(), "docker daemon unavailable: ") {
		t.Errorf("daemon 不可用应透传错误: %v", err)
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
		"edgeflow-default-legacy\tlegacy\tdefault", // 旧式命名（无序号）→ Index=-1（P1-1）
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
		{Namespace: "default", Name: "legacy", Index: -1},
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
// 无序号/非法序号返回 -1（M4A P1-1：旧式命名实例由 reconcile 优先清理）。
func TestParseIndexFromName(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"edgeflow-default-nginx-0", 0},
		{"edgeflow-default-nginx-1", 1},
		{"edgeflow-default-nginx-10", 10},
		{"edgeflow-default-nginx", -1},     // 旧式命名（无序号，P1-1 语义）
		{"edgeflow-default-nginx-x", -1},   // 末段非数字
		{"edgeflow-default-nginx-", -1},    // 以 '-' 结尾（末段为空）
		{"edgeflow-default-nginx-abc", -1}, // 末段为字母
		{"abc", -1},
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

// TestResourcesMatchErrorPaths 验证 resourcesMatch 的失败路径：
// HostConfig 输出字段数不对/数值解析失败 → 报错（不误判为漂移/匹配）；
// 容器不存在与无 limit 期望 → (true, nil)（交给创建分支/跳过检查）。
func TestResourcesMatchErrorPaths(t *testing.T) {
	pod := podNginx()
	pod.Resources = metamanager.ResourceRequirements{CPULimit: "250m", MemoryLimit: "64Mi"}

	t.Run("HostConfig 输出垃圾（字段数不对）→报错", func(t *testing.T) {
		d := newTestDocker(fakeRunner([]any{"only-one-field\n", nil}))
		ok, err := d.ResourcesMatch(pod, 0)
		if err == nil {
			t.Fatal("字段数不对应报错")
		}
		if ok {
			t.Error("报错时不应返回匹配")
		}
		if !strings.Contains(err.Error(), "无法解析") {
			t.Errorf("错误文案不符: %v", err)
		}
	})

	t.Run("HostConfig 数值解析失败→报错", func(t *testing.T) {
		d := newTestDocker(fakeRunner([]any{"abc def\n", nil}))
		ok, err := d.ResourcesMatch(pod, 0)
		if err == nil {
			t.Fatal("数值解析失败应报错")
		}
		if ok {
			t.Error("报错时不应返回匹配")
		}
		if !strings.Contains(err.Error(), "数值解析失败") {
			t.Errorf("错误文案不符: %v", err)
		}
	})

	t.Run("容器不存在→(true,nil) 交给创建分支", func(t *testing.T) {
		d := newTestDocker(fakeRunner([]any{"", errFakeNoObject}))
		ok, err := d.ResourcesMatch(pod, 0)
		if err != nil || !ok {
			t.Errorf("容器不存在应返回 (true, nil)，实际 (%v, %v)", ok, err)
		}
	})

	t.Run("期望无 limit→(true,nil) 且不做 inspect", func(t *testing.T) {
		called := false
		d := newTestDocker(func(_ context.Context, _ ...string) (string, error) {
			called = true
			return "", nil
		})
		ok, err := d.ResourcesMatch(podNginx(), 0)
		if err != nil || !ok {
			t.Errorf("无 limit 应返回 (true, nil)，实际 (%v, %v)", ok, err)
		}
		if called {
			t.Error("无 limit 期望时不应发起 HostConfig inspect")
		}
	})

	t.Run("daemon 不可用→错误透传", func(t *testing.T) {
		d := newTestDocker(fakeRunner([]any{"", errFakeDaemonDown}))
		ok, err := d.ResourcesMatch(pod, 0)
		if err == nil || ok {
			t.Errorf("daemon 不可用应透传错误，实际 (%v, %v)", ok, err)
		}
	})
}

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

// ---------- v0.12.0 R-1++ 运行时 digest 测试 ----------

// TestFirstSha256RepoDigest 验证 RepoDigests JSON 数组解析：
// 单条 / 多条取首个 sha256 / 空数组 / 非法 JSON / 非 sha256 条目跳过。
func TestFirstSha256RepoDigest(t *testing.T) {
	const d1 = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const d2 = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"单条", `["reg/ns/model@` + d1 + `"]`, d1},
		{"多条取首个 sha256", `["reg/ns/model@sha512:xxxx","reg/ns/model@` + d1 + `","reg/ns/model@` + d2 + `"]`, d1},
		{"空数组", `[]`, ""},
		{"非法 JSON", `not-json`, ""},
		{"非 sha256 条目跳过", `["reg/ns/model@sha512:xxxx"]`, ""},
		{"空 JSON null", `null`, ""},
	}
	for _, tc := range cases {
		if got := firstSha256RepoDigest(tc.raw); got != tc.want {
			t.Errorf("%s: firstSha256RepoDigest(%q) = %q，期望 %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

// TestDockerImageDigest 验证运行时 digest 查询（v0.12.0，R-1++）：
// 容器不存在 → ("", nil)；Config.Image → RepoDigests 查询 → 返回 sha256；
// 镜像不存在 → ("", nil)；daemon 不可用 → error。
func TestDockerImageDigest(t *testing.T) {
	const d = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	t.Run("容器不存在", func(t *testing.T) {
		docker := newTestDocker(fakeRunner(
			[]any{"", errFakeNoCtr},
		))
		got, err := docker.ImageDigest(metamanager.Pod{Namespace: "default", Name: "web"}, 0)
		if err != nil || got != "" {
			t.Errorf("容器不存在: got=%q err=%v，期望 (\"\", nil)", got, err)
		}
	})
	t.Run("正常查询返回 sha256", func(t *testing.T) {
		docker := newTestDocker(fakeRunner(
			[]any{"reg.io/ns/model:v1", nil},            // inspect Config.Image
			[]any{`["reg.io/ns/model@` + d + `"]`, nil}, // image inspect RepoDigests
		))
		got, err := docker.ImageDigest(metamanager.Pod{Namespace: "default", Name: "web"}, 0)
		if err != nil || got != d {
			t.Errorf("正常查询: got=%q err=%v，期望 %q", got, err, d)
		}
	})
	t.Run("镜像无 RepoDigests → 空", func(t *testing.T) {
		docker := newTestDocker(fakeRunner(
			[]any{"reg.io/ns/model:v1", nil},
			[]any{`[]`, nil},
		))
		got, err := docker.ImageDigest(metamanager.Pod{Namespace: "default", Name: "web"}, 0)
		if err != nil || got != "" {
			t.Errorf("镜像无 RepoDigests: got=%q err=%v，期望 (\"\", nil)", got, err)
		}
	})
	t.Run("镜像不存在 → 空", func(t *testing.T) {
		docker := newTestDocker(fakeRunner(
			[]any{"reg.io/ns/model:v1", nil},
			[]any{"", errors.New("Error: No such image: reg.io/ns/model:v1")},
		))
		got, err := docker.ImageDigest(metamanager.Pod{Namespace: "default", Name: "web"}, 0)
		if err != nil || got != "" {
			t.Errorf("镜像不存在: got=%q err=%v，期望 (\"\", nil)", got, err)
		}
	})
	t.Run("daemon 不可用 → error", func(t *testing.T) {
		docker := newTestDocker(fakeRunner(
			[]any{"", errFakeDaemonDown},
		))
		_, err := docker.ImageDigest(metamanager.Pod{Namespace: "default", Name: "web"}, 0)
		if err == nil {
			t.Error("daemon 不可用应返回 error")
		}
	})
}
