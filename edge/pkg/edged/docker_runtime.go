package edged

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"edgeflow/edge/pkg/metamanager"
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
//   - 容器命名规范 edgeflow-<namespace>-<name>，并打两个标签
//     （edgeflow.pod / edgeflow.namespace），List 按标签过滤，只管理自己的容器；
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

	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
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

// EnsureRunning 确保容器存在且运行：
//   - 不存在 → docker run -d（后台创建并启动）；
//   - 存在但停止 → docker start；
//   - 已运行 → no-op。
//
// 并发兜底：若 run 因"名字已被占用"失败（并发 reconcile/外部创建），
// 重新 Inspect 按实际状态处理，不报错。
func (d *DockerRuntime) EnsureRunning(pod metamanager.Pod) error {
	state, err := d.Inspect(pod)
	if err != nil {
		return err
	}
	switch state {
	case StateRunning:
		return nil // 幂等：已运行
	case StateStopped:
		if _, err := d.exec("start", ContainerName(pod.Namespace, pod.Name)); err != nil {
			// daemon 不可用类错误已带明确前缀，直接透传避免二次包装
			if isDaemonUnavailable(err.Error()) {
				return err
			}
			return fmt.Errorf("启动容器 %s 失败: %w", ContainerName(pod.Namespace, pod.Name), err)
		}
		return nil
	case StateAbsent:
		if _, err := d.exec("run", "-d",
			"--name", ContainerName(pod.Namespace, pod.Name),
			"--label", labelPod+"="+pod.Name,
			"--label", labelNamespace+"="+pod.Namespace,
			pod.Image); err != nil {
			// 竞态兜底：并发创建导致名字冲突 → 按已存在处理
			lower := strings.ToLower(err.Error())
			if strings.Contains(lower, "conflict") && strings.Contains(lower, "already in use") {
				if _, ierr := d.Inspect(pod); ierr != nil {
					return fmt.Errorf("创建容器 %s 冲突后重新查询失败: %w", ContainerName(pod.Namespace, pod.Name), ierr)
				}
				return nil
			}
			// daemon 不可用类错误已带明确前缀，直接透传避免二次包装
			if isDaemonUnavailable(err.Error()) {
				return err
			}
			return fmt.Errorf("创建容器 %s 失败: %w", ContainerName(pod.Namespace, pod.Name), err)
		}
		return nil
	default:
		return fmt.Errorf("查询容器 %s 状态未知，拒绝操作", ContainerName(pod.Namespace, pod.Name))
	}
}

// EnsureStopped 强制停止并移除容器；容器不存在时 no-op（幂等）。
func (d *DockerRuntime) EnsureStopped(pod metamanager.Pod) error {
	name := ContainerName(pod.Namespace, pod.Name)
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

// Inspect 查询容器状态：
//   - docker inspect --format '{{.State.Running}}' → true/false；
//   - "No such object" → Absent；
//   - daemon 不可用等 → Unknown + 错误。
func (d *DockerRuntime) Inspect(pod metamanager.Pod) (RuntimeState, error) {
	name := ContainerName(pod.Namespace, pod.Name)
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

// List 列出本机由 Edged 管理的全部容器（含已停止的），返回其 Pod 标识。
// docker ps -a --filter label=edgeflow.pod，按标签反解 namespace/name
// （不解析容器名，避免名字中的 '-' 造成歧义）。
func (d *DockerRuntime) List() ([]metamanager.Pod, error) {
	out, err := d.exec("ps", "-a",
		"--filter", "label="+labelPod,
		"--format", "{{.Names}}\t{{.Label \""+labelPod+"\"}}\t{{.Label \""+labelNamespace+"\"}}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	pods := make([]metamanager.Pod, 0, 4)
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
		pods = append(pods, metamanager.Pod{Namespace: ns, Name: name})
	}
	return pods, nil
}

// Close 无持久资源（每次操作独立 exec），按接口要求实现。
func (d *DockerRuntime) Close() error {
	return nil
}
