package edged

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"edgeflow/edge/pkg/metamanager"
	"edgeflow/pkg/resource"
)

// defaultExecTimeout 是每次 docker 命令调用的超时上限。
// 覆盖 inspect/start/rm 等轻量命令；docker run 首次拉镜像可能超时，
// 超时后容器可能在 daemon 侧仍在创建——reconcile 下一轮会收敛（见 EDGED-POC.md 风险清单）。
const defaultExecTimeout = 30 * time.Second

// cmdRunner 是 docker 命令执行器抽象：执行 `docker <args...>`，
// 返回合并输出（stdout+stderr）与错误。默认走 os/exec，测试可注入假实现。
type cmdRunner func(ctx context.Context, args ...string) (string, error)

// defaultCmdRunner 用 exec.CommandContext 执行 docker CLI，带上下文取消。
func defaultCmdRunner(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// DockerRuntime 是基于 Docker CLI 的 ContainerRuntime 实现（方案 A 的"真实"实现）。
//
// 设计：
//   - 每次操作独立 exec docker CLI（无长连接），daemon 不可用时错误带
//     "docker daemon unavailable: " 前缀，便于识别与告警；
//   - 容器命名规范 edgeflow-<namespace>-<name>-<index>（index 为副本序号），
//     并打两个标签（edgeflow.pod / edgeflow.namespace），List 按标签过滤，
//     只管理自己的容器；副本序号从容器名末段反解（序号始终在名字末尾，
//     超长名截断只作用于基础名，不影响序号解析）；
//   - 所有方法幂等：inspect 判断存在性，rm 对不存在容器视为成功。
type DockerRuntime struct {
	// timeout 是单次 docker 命令的超时；默认 defaultExecTimeout。
	timeout time.Duration

	// runner 是命令执行器；nil 时用 defaultCmdRunner（生产路径）。
	runner cmdRunner
}

// NewDockerRuntime 创建 DockerRuntime。
func NewDockerRuntime() *DockerRuntime {
	return &DockerRuntime{timeout: defaultExecTimeout}
}

// exec 执行 docker 命令并做错误归一化：
//   - CLI 缺失 / daemon 不可用 → 错误带 "docker daemon unavailable: " 前缀；
//   - 超时 → 明确提示超时；
//   - 其他失败 → 附带命令与首行输出。
//
// 错误文本来源：真实 docker CLI 把报错打在 stderr（CombinedOutput 已合并），
// 但程序化错误（测试注入/exec 包装）可能只有 err 带文本，因此 msg 为空时
// 回退用 err.Error() 做判定。
func (d *DockerRuntime) exec(args ...string) (string, error) {
	// runner 为 nil 时走生产路径（真实 docker CLI）；测试注入假执行器
	runner := d.runner
	if runner == nil {
		runner = defaultCmdRunner
	}

	timeout := d.timeout
	if timeout <= 0 {
		timeout = defaultExecTimeout // 零值回落：&DockerRuntime{} 也应有默认超时
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, err := runner(ctx, args...)
	msg := strings.TrimSpace(out)
	if err == nil {
		return msg, nil
	}
	if msg == "" {
		msg = err.Error()
	}

	// CLI 不存在（PATH 中找不到 docker 可执行文件）
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(msg, "docker: command not found") {
		return "", fmt.Errorf("docker daemon unavailable: docker CLI 不可用: %w", err)
	}
	// 命令超时（context 到期）
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return "", fmt.Errorf("docker 命令超时（%s）: docker %s", d.timeout, strings.Join(args, " "))
	}
	// daemon 不可用（未启动/连接被拒/启动中）
	if isDaemonUnavailable(msg) {
		return "", fmt.Errorf("docker daemon unavailable: %s", firstLine(msg))
	}
	// 其他 docker 错误：附带完整输出便于排查
	return "", fmt.Errorf("docker 命令失败（docker %s）: %v: %s", strings.Join(args, " "), err, firstLine(msg))
}

// isDaemonUnavailable 识别 daemon 不可用的典型报错文本。
func isDaemonUnavailable(msg string) bool {
	keywords := []string{
		"cannot connect to the docker daemon",
		"error during connect",
		"connection refused",
		"is the docker daemon running",
		"dial unix /var/run/docker.sock",
		"docker desktop is starting",
		"docker desktop starting",
	}
	lower := strings.ToLower(msg)
	for _, k := range keywords {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

// isNoSuchContainer 识别"容器不存在"类错误（docker inspect / rm 的幂等依据）。
// 与 daemon 不可用互斥判断，避免把 daemon 故障误判为"容器不存在"。
// 输入可能是 docker CLI 的 stderr 文本或包装后的 error 字符串，统一小写后匹配。
func isNoSuchContainer(msg string) bool {
	lower := strings.ToLower(msg)
	if isDaemonUnavailable(lower) {
		return false
	}
	return strings.Contains(lower, "no such object") || strings.Contains(lower, "no such container")
}

// firstLine 取多行输出的第一行（错误摘要）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// EnsureRunning 确保 Pod 第 index 个副本的容器存在且运行：
//   - 不存在 → docker run -d（后台创建并启动）；
//   - 存在但停止 → docker start；
//   - 已运行 → 检查镜像是否与期望一致（WBS 6.4 镜像漂移检测）：
//     一致 → no-op；不一致 → 先 EnsureStopped（rm -f）再重建（重新 run -d）。
//
// 并发兜底：若 run 因"名字已被占用"失败（并发 reconcile/外部创建），
// 重新 Inspect 按实际状态处理，不报错。
func (d *DockerRuntime) EnsureRunning(pod metamanager.Pod, index int) error {
	name := ContainerName(pod.Namespace, pod.Name, index)
	state, err := d.Inspect(pod, index)
	if err != nil {
		return err
	}
	switch state {
	case StateRunning:
		// 镜像漂移检测（WBS 6.4 滚动更新）：已运行 ≠ 镜像正确。
		// 云端更新 Pod 镜像后，旧镜像容器必须重建才能生效——
		// 不一致时先停（rm -f）再走下方 Absent 分支重新 run -d，
		// 保证"镜像与期望一致"的幂等收敛。
		ok, ierr := d.ImageMatches(pod, index)
		if ierr != nil {
			return ierr
		}
		if !ok {
			if err := d.EnsureStopped(pod, index); err != nil {
				return fmt.Errorf("镜像漂移重建 %s 时停止旧容器失败: %w", name, err)
			}
			return d.EnsureRunning(pod, index) // 递归：容器已删除，走 Absent 分支重建
		}
		// 资源漂移检测（WBS 6.5）：镜像一致但资源 limit 不一致 → 同样重建。
		// 只在期望带 limit 时检查（不带 limit 的 Pod 不触发额外 inspect，
		// 兼容既有镜像漂移语义与旧测试）。
		rok, rerr := d.resourcesMatch(pod, index)
		if rerr != nil {
			return rerr
		}
		if !rok {
			if err := d.EnsureStopped(pod, index); err != nil {
				return fmt.Errorf("资源漂移重建 %s 时停止旧容器失败: %w", name, err)
			}
			return d.EnsureRunning(pod, index) // 递归：重建后带新资源参数 run -d
		}
		return nil // 幂等：已运行、镜像一致、资源一致
	case StateStopped:
		if _, err := d.exec("start", name); err != nil {
			// daemon 不可用类错误已带明确前缀，直接透传避免二次包装
			if isDaemonUnavailable(err.Error()) {
				return err
			}
			return fmt.Errorf("启动容器 %s 失败: %w", name, err)
		}
		return nil
	case StateAbsent:
		// WBS 6.5 资源限制：resources.limit 非空时转换为 docker 参数
		// （--cpus / --memory / --memory-swap）。零值 = 不限制，不传参数。
		resArgs, err := DockerResourceArgs(pod.Resources)
		if err != nil {
			return fmt.Errorf("Pod %s 资源参数解析失败: %w", name, err)
		}
		runArgs := append([]string{"run", "-d",
			"--name", name,
			"--label", labelPod + "=" + pod.Name,
			"--label", labelNamespace + "=" + pod.Namespace},
			resArgs...)
		runArgs = append(runArgs, pod.Image)
		if _, err := d.exec(runArgs...); err != nil {
			// 竞态兜底：并发创建导致名字冲突 → 按已存在处理
			lower := strings.ToLower(err.Error())
			if strings.Contains(lower, "conflict") && strings.Contains(lower, "already in use") {
				if _, ierr := d.Inspect(pod, index); ierr != nil {
					return fmt.Errorf("创建容器 %s 冲突后重新查询失败: %w", name, ierr)
				}
				return nil
			}
			// daemon 不可用类错误已带明确前缀，直接透传避免二次包装
			if isDaemonUnavailable(err.Error()) {
				return err
			}
			return fmt.Errorf("创建容器 %s 失败: %w", name, err)
		}
		return nil
	default:
		return fmt.Errorf("查询容器 %s 状态未知，拒绝操作", name)
	}
}

// ImageMatches 检查容器当前镜像是否与期望一致（WBS 6.4 镜像漂移检测）：
// docker inspect --format '{{.Config.Image}}' 取创建时镜像引用，与 pod.Image
// 精确比对。容器不存在 → (false, nil)（不算漂移，由 EnsureRunning 创建分支
// 补齐）；daemon 不可用等查询失败 → 错误透传（调用方记错误，下一轮重试）。
func (d *DockerRuntime) ImageMatches(pod metamanager.Pod, index int) (bool, error) {
	name := ContainerName(pod.Namespace, pod.Name, index)
	out, err := d.exec("inspect", "--format", "{{.Config.Image}}", name)
	if err != nil {
		if isNoSuchContainer(err.Error()) {
			return false, nil // 容器不存在：不是漂移，交给创建分支
		}
		return false, err
	}
	// Docker 的 Config.Image 保留创建时的镜像引用（如 "nginx:1.27"），
	// 与下发期望值同格式时精确相等。跨格式等价（如 digest 引用）暂不
	// 归一化——云端下发统一引用格式即可（POC 简化，注释标明演进方向：
	// 需要时改为解析 digest 后比对）。
	return strings.TrimSpace(out) == pod.Image, nil
}

// resourcesMatch 检查已运行容器的资源 limit 是否与期望一致（WBS 6.5 资源漂移）。
// 期望不带任何 limit 时不检查（返回 true，不触发重建——避免给每个 Pod
// 增加一次 inspect，兼容既有镜像漂移语义）；带 limit 时取
// {{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} 与期望值精确比对。
// 容器不存在 → (true, nil)（由创建分支处理，不误报漂移）。
func (d *DockerRuntime) resourcesMatch(pod metamanager.Pod, index int) (bool, error) {
	hasLimit := pod.Resources.CPULimit != "" || pod.Resources.MemoryLimit != ""
	if !hasLimit {
		return true, nil
	}
	name := ContainerName(pod.Namespace, pod.Name, index)
	out, err := d.exec("inspect", "--format", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}", name)
	if err != nil {
		if isNoSuchContainer(err.Error()) {
			return true, nil // 容器不存在：交给创建分支
		}
		return false, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return false, fmt.Errorf("docker inspect HostConfig 输出无法解析: %q", out)
	}
	actualNano, aerr := strconv.ParseInt(fields[0], 10, 64)
	actualMem, merr := strconv.ParseInt(fields[1], 10, 64)
	if aerr != nil || merr != nil {
		return false, fmt.Errorf("docker inspect HostConfig 数值解析失败: %q", out)
	}

	// 期望值：CPU limit（毫核 × 1e6 = nano）；Memory limit（字节）。
	wantNano, wantMem := int64(0), int64(0)
	if pod.Resources.CPULimit != "" {
		cpu, cerr := resource.ParseCPU(pod.Resources.CPULimit)
		if cerr != nil {
			return false, fmt.Errorf("CPU limit 解析失败: %w", cerr)
		}
		wantNano = cpu * 1e6
	}
	if pod.Resources.MemoryLimit != "" {
		mem, merr := resource.ParseMemory(pod.Resources.MemoryLimit)
		if merr != nil {
			return false, fmt.Errorf("Memory limit 解析失败: %w", merr)
		}
		wantMem = mem
	}
	return actualNano == wantNano && actualMem == wantMem, nil
}

// EnsureStopped 强制停止并移除 Pod 第 index 个副本的容器；容器不存在时 no-op（幂等）。
func (d *DockerRuntime) EnsureStopped(pod metamanager.Pod, index int) error {
	// index=-1 表示旧式无序号命名容器（M4A P1-1）：用无序号名删除，
	// 与 List 的 parseIndexFromName 返回 -1 对应。
	name := ContainerName(pod.Namespace, pod.Name, index)
	if index < 0 {
		name = LegacyContainerName(pod.Namespace, pod.Name)
	}
	_, err := d.exec("rm", "-f", name)
	if err == nil {
		return nil
	}
	// 不存在即视为已达成目标（幂等）
	if isNoSuchContainer(err.Error()) {
		return nil
	}
	// daemon 不可用类错误已带明确前缀，直接透传避免二次包装
	if isDaemonUnavailable(err.Error()) {
		return err
	}
	return fmt.Errorf("删除容器 %s 失败: %w", name, err)
}

// Inspect 查询 Pod 第 index 个副本容器的状态：
//   - docker inspect --format '{{.State.Running}}' → true/false；
//   - "No such object" → Absent；
//   - daemon 不可用等 → Unknown + 错误。
func (d *DockerRuntime) Inspect(pod metamanager.Pod, index int) (RuntimeState, error) {
	name := ContainerName(pod.Namespace, pod.Name, index)
	out, err := d.exec("inspect", "--format", "{{.State.Running}}", name)
	if err == nil {
		switch out {
		case "true":
			return StateRunning, nil
		case "false":
			return StateStopped, nil
		default:
			// 输出异常（如模板被转义）兜底为 Unknown，不误报
			return StateUnknown, fmt.Errorf("docker inspect 输出无法解析: %q", out)
		}
	}
	if isNoSuchContainer(err.Error()) {
		return StateAbsent, nil
	}
	return StateUnknown, err
}

// List 列出本机由 Edged 管理的全部容器实例（含已停止的），返回 Pod 标识 + 副本序号。
// docker ps -a --filter label=edgeflow.pod，按标签反解 namespace/name
// （不解析容器名，避免名字中的 '-' 造成歧义）；副本序号从容器名末段解析
// （-<index> 恒在末尾；旧式无序号命名兜底为副本 0）。
func (d *DockerRuntime) List() ([]InstanceRef, error) {
	out, err := d.exec("ps", "-a",
		"--filter", "label="+labelPod,
		"--format", "{{.Names}}\t{{.Label \""+labelPod+"\"}}\t{{.Label \""+labelNamespace+"\"}}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	insts := make([]InstanceRef, 0, 4)
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			continue // 格式异常行跳过（防御）
		}
		name := strings.TrimSpace(parts[1])
		if name == "" {
			continue // 无 pod 标签的容器（理论上被 filter 排除，防御性跳过）
		}
		ns := strings.TrimSpace(parts[2])
		if ns == "" {
			ns = "default"
		}
		insts = append(insts, InstanceRef{
			Namespace: ns,
			Name:      name,
			Index:     parseIndexFromName(strings.TrimSpace(parts[0])),
		})
	}
	return insts, nil
}

// parseIndexFromName 从容器名末段反解副本序号：
// 命名规范 edgeflow-<ns>-<name>-<index>，-<index> 恒在末尾
// （超长名截断只作用于基础名）。解析失败（如旧式无序号命名）返回 -1，
// 由 reconcile 作为"不规范旧实例"优先清理（M4A P1-1 修复：避免与新版
// -0 槽位冲突导致每轮调谐创建/删除无限循环）。
func parseIndexFromName(containerName string) int {
	if i := strings.LastIndexByte(containerName, '-'); i >= 0 {
		idx, err := strconv.Atoi(containerName[i+1:])
		if err == nil && idx >= 0 {
			return idx
		}
	}
	return -1
}

// Close 无持久资源（每次操作独立 exec），按接口要求实现。
func (d *DockerRuntime) Close() error {
	return nil
}
