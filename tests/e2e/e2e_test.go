// Package e2e 是 EdgeFlow 端到端测试套件（WBS 8.3 验收缺口 G1）。
//
// 运行方式：
//
//	# 全部用例（真实进程：编译 cloudcore/edgecore 后启动子进程）
//	go test -v -timeout 20m ./tests/e2e/
//	# 跳过容器相关用例（无 Docker 环境）
//	go test -v -timeout 20m -short ./tests/e2e/
//
// 前置条件（按用例区分，不满足时对应用例 t.Skip 并说明）：
//   - 本机 Go 工具链（编译组件二进制，所有用例必需）；
//   - 容器生命周期用例（自治/多节点）：Docker daemon 可用，且本地缓存
//     镜像 nginx:1.25-alpine（避免测试期间拉取镜像的耗时与网络依赖）；
//   - 设备链路用例（device_e2e）：无需 Docker，默认执行。
//
// 设计说明：
//   - 用例以「真实进程」方式运行：go build 编译 cmd/cloudcore 与
//     cmd/edgecore 二进制，以子进程启动并喂入环境变量（端口/数据库路径/
//     调谐周期等全部可覆盖），通过云端 HTTP API（/api/v1/...）与
//     docker CLI 断言端到端行为——验证的是真实装配路径，不是 mock；
//   - 每个用例使用独立端口与临时数据库目录，互不干扰；进程在用例
//     结束（含失败）时统一清理，不残留；
//   - 自治 30min 语义：真实时长需长时间运行环境验证，用例以 60s 短时
//     窗口模拟（判定逻辑一致），文档注明（见 docs/PERFORMANCE-BASELINE.md）。
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// e2eImage 是容器用例使用的镜像：本地缓存、长驻运行（入口 nginx 不退出），
// 满足"容器保持 Running"的断言语义（busybox 等退出型镜像会触发
// CrashLoopBackOff，不适合自治/多节点用例）。
const e2eImage = "nginx:1.25-alpine"

// uniqueSuffix 是本进程（一次 go test 调用）内的唯一后缀：追加到 Pod 名，
// 保证容器名全局唯一。背景：E2E 套件可能在共享 Docker daemon 上并发运行
// （并行 Agent / 多分支 CI / 上次运行残留的 edgecore 进程），若 Pod 名固定，
// 残留 edgecore 会用同名容器持续调谐，导致删除断言超时（容器被反复重建）；
// 唯一后缀让每次运行的容器互不重叠，残留进程只能调谐它自己那批容器。
var uniqueSuffix = fmt.Sprintf("%d", time.Now().UnixNano())

// podNameWithSuffix 生成带唯一后缀的 Pod 名：<base>-<unixnano>。
func podNameWithSuffix(base string) string {
	return base + "-" + uniqueSuffix
}

// ---------- 二进制构建（进程级一次） ----------

var (
	buildOnce sync.Once
	binDir    string // 编译产物目录
	buildErr  error  // 构建失败原因（首个用例失败后其余用例直接 Skip）
)

// repoRoot 定位仓库根目录：本文件位于 <root>/tests/e2e/。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位本文件路径")
	}
	// 本文件位于 <root>/tests/e2e/，向上两级到 tests/，再一级到仓库根
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("解析仓库根目录失败: %v", err)
	}
	return abs
}

// buildBinaries 编译 cloudcore/edgecore 二进制（仅一次，供全部用例复用）。
func buildBinaries(t *testing.T) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "edgeflow-e2e-bin-")
		if err != nil {
			buildErr = fmt.Errorf("创建二进制目录失败: %w", err)
			return
		}
		binDir = dir
		root := repoRoot(t)
		for _, pkg := range []string{"./cmd/cloudcore", "./cmd/edgecore"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(dir, filepath.Base(pkg)), pkg)
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if err != nil {
				buildErr = fmt.Errorf("编译 %s 失败: %v\n%s", pkg, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("组件二进制构建失败（后续用例将跳过）: %v", buildErr)
	}
}

// ---------- 子进程生命周期 ----------

// proc 是测试管理的子进程（统一清理与日志缓冲）。
type proc struct {
	t      *testing.T
	name   string
	cmd    *exec.Cmd
	mu     sync.Mutex
	buf    bytes.Buffer // 输出缓冲（stdout+stderr），失败时转储
	doneCh chan struct{}
}

// logTail 返回进程输出缓冲（最近 200 行），供失败诊断。
func (p *proc) logTail() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	lines := strings.Split(p.buf.String(), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	return strings.Join(lines, "\n")
}

// stop 优雅停止进程（SIGINT → 最多等 15s → SIGKILL），并等待退出。
// 全程有界：即使进程对信号无响应（极端情况），也不阻塞测试清理。
func (p *proc) stop() {
	if p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.doneCh:
		return
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.doneCh:
	case <-time.After(5 * time.Second):
		// 兜底：SIGKILL 后仍未退出（极端异常），不再阻塞清理流程
		p.t.Logf("警告: %s 进程 SIGKILL 后仍未退出", p.name)
	}
}

// startProcess 启动子进程并注册清理。stdout/stderr 汇入缓冲。
func startProcess(t *testing.T, name, bin string, args []string, env []string) *proc {
	t.Helper()
	p := &proc{t: t, name: name, doneCh: make(chan struct{})}
	p.cmd = exec.Command(bin, args...)
	p.cmd.Env = env
	p.cmd.Stdout = &syncWriter{w: &p.buf, mu: &p.mu}
	p.cmd.Stderr = &syncWriter{w: &p.buf, mu: &p.mu}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("启动 %s 失败: %v", name, err)
	}
	go func() {
		_ = p.cmd.Wait()
		close(p.doneCh)
	}()
	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			t.Logf("===== %s 进程日志尾部 =====\n%s", name, p.logTail())
		}
	})
	return p
}

// syncWriter 是并发安全的 io.Writer（子进程输出与日志转储并发访问）。
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// ---------- 端口与地址 ----------

// reservePort 申请一个空闲 TCP 端口（绑定后立即释放，供子进程监听；
// 存在极小竞态窗口，cloudcore 启动失败时由调用方换端口重试）。
func reservePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 10; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		return port
	}
	t.Fatal("无法申请空闲端口")
	return 0
}

// cloudEnv 构造 cloudcore 子进程环境：HTTP 端口 + CloudHub 端口。
func cloudEnv(httpPort, hubPort int) []string {
	return append(os.Environ(),
		"EDGEFLOW_CLOUDCORE_PORT="+strconv.Itoa(httpPort),
		"EDGEFLOW_CLOUDCORE_HUB_PORT="+strconv.Itoa(hubPort),
	)
}

// edgeEnv 构造 edgecore 子进程环境：节点 ID、云地址、独立数据库路径，
// 以及缩短的调谐/上报周期（加速断言收敛）与 MQTT 快速失败（无 broker）。
func edgeEnv(nodeID, cloudAddr, dbPath string) []string {
	return append(os.Environ(),
		"EDGEFLOW_EDGECORE_NODE_ID="+nodeID,
		"EDGEFLOW_EDGECORE_CLOUD_ADDR="+cloudAddr,
		"EDGEFLOW_EDGECORE_DB_PATH="+dbPath,
		"EDGEFLOW_EDGECORE_RECONCILE_INTERVAL=300ms",
		"EDGEFLOW_EDGECORE_REPORT_INTERVAL=300ms",
		"EDGEFLOW_EDGECORE_DEVICE_REPORT_INTERVAL=300ms",
		"EDGEFLOW_EDGECORE_MQTT_CONNECT_TIMEOUT=300ms",
		"EDGEFLOW_EDGECORE_MQTT_ADDR=tcp://127.0.0.1:1",
	)
}

// startCloudcore 启动 cloudcore 子进程（自动挑选空闲端口）并等待 /healthz 就绪。
// 端口竞态导致启动失败时自动换端口重试（最多 3 次）。
// 返回进程与生效的 HTTP/CloudHub 端口。
func startCloudcore(t *testing.T, root string) (*proc, int, int) {
	t.Helper()
	httpPort, hubPort := reservePort(t), reservePort(t)
	p := startCloudcoreOnPorts(t, root, httpPort, hubPort)
	return p, httpPort, hubPort
}

// startCloudcoreOnPorts 在指定端口启动 cloudcore（重启场景复用原端口，
// 保证 edgecore 重连地址不变）并等待 /healthz 就绪。
func startCloudcoreOnPorts(t *testing.T, root string, httpPort, hubPort int) *proc {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		p := startProcess(t, "cloudcore", filepath.Join(binDir, "cloudcore"),
			[]string{"--port", strconv.Itoa(httpPort)}, cloudEnv(httpPort, hubPort))
		base := "http://127.0.0.1:" + strconv.Itoa(httpPort)
		if waitHTTP(t, 15*time.Second, base+"/healthz", nil) {
			return p
		}
		// 启动失败：可能是端口竞态（time-wait 等），重试同端口
		lastErr = fmt.Errorf("第 %d 次尝试 cloudcore 未就绪（端口 %d/%d）", attempt, httpPort, hubPort)
		p.stop()
	}
	t.Fatalf("cloudcore 启动失败（3 次尝试）: %v", lastErr)
	return nil
}

// startEdgecore 启动 edgecore 子进程并返回。
func startEdgecore(t *testing.T, root, nodeID string, hubPort int) *proc {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "edgecore.db")
	cloudAddr := "ws://127.0.0.1:" + strconv.Itoa(hubPort)
	return startProcess(t, "edgecore-"+nodeID, filepath.Join(binDir, "edgecore"),
		nil, edgeEnv(nodeID, cloudAddr, dbPath))
}

// ---------- HTTP 与等待辅助 ----------

// waitHTTP 轮询 GET url 直到 200（respCheck 可进一步校验响应；nil 表示只
// 要 200）。超时返回 false。
func waitHTTP(t *testing.T, timeout time.Duration, url string, respCheck func(*http.Response) bool) bool {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			ok := respCheck == nil || respCheck(resp)
			_ = resp.Body.Close()
			if ok {
				return true
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// getJSON 拉取 URL 并解析 JSON；非 200 时带状态码报错。
func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("GET %s 解析失败: %v", url, err)
	}
}

// postJSON POST JSON 到 url，期望 200（返回响应体文本供断言）。
func postJSON(t *testing.T, url string, body any) string {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST %s 失败: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s 状态码 = %d，响应: %s", url, resp.StatusCode, buf.String())
	}
	return buf.String()
}

// nodeInfo 是 /api/v1/nodes 的元素（与云端契约一致的子集，够断言用）。
type nodeInfo struct {
	NodeID string `json:"nodeID"`
	Status string `json:"status"`
}

// waitNodeRegistered 轮询云端节点列表，直到 nodeID 出现。
func waitNodeRegistered(t *testing.T, base string, nodeID string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var nodes []nodeInfo
		getJSON(t, base+"/api/v1/nodes", &nodes)
		for _, n := range nodes {
			if n.NodeID == nodeID {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待节点 %s 注册超时（60s）", nodeID)
}

// podStatusItem 是 /api/v1/pods 的元素（云端 podstatus.PodStatus 子集）。
type podStatusItem struct {
	NodeID    string `json:"nodeID"`
	PodName   string `json:"podName"`
	Namespace string `json:"namespace"`
	Phase     string `json:"phase"`
}

// podStatusList 是 /api/v1/pods 的响应形态。
type podStatusList struct {
	Items []podStatusItem `json:"items"`
}

// waitPodPhase 轮询云端 Pod 状态，直到指定节点上 Pod 达到期望 phase。
func waitPodPhase(t *testing.T, base, nodeID, podName, phase string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var list podStatusList
		getJSON(t, base+"/api/v1/pods", &list)
		for _, p := range list.Items {
			if p.NodeID == nodeID && p.PodName == podName && p.Phase == phase {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待 Pod %s/%s phase=%s 超时（90s）", nodeID, podName, phase)
}

// syncPodRequest 是 podsync API 的请求体（与云端契约一致）。
type syncPodRequest struct {
	Operation string      `json:"operation"`
	Pod       syncPodSpec `json:"pod"`
}

// syncPodSpec 是 Pod 最小规格。
type syncPodSpec struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Replicas  int    `json:"replicas"`
}

// syncPod 向云端下发 Pod 变更（add/update/delete），断言 200（边缘已 Ack）。
func syncPod(t *testing.T, base, nodeID, op, name, image string, replicas int) {
	t.Helper()
	url := fmt.Sprintf("%s/api/v1/nodes/%s/podsync", base, nodeID)
	postJSON(t, url, syncPodRequest{
		Operation: op,
		Pod:       syncPodSpec{Name: name, Namespace: "default", Image: image, Replicas: replicas},
	})
}

// ---------- Docker 辅助（容器用例前置条件与断言） ----------

// dockerOK 返回 docker daemon 是否可用。
func dockerOK() bool {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// dockerImageCached 返回镜像是否已在本地缓存。
func dockerImageCached(image string) bool {
	out, err := exec.Command("docker", "image", "inspect", image).CombinedOutput()
	return err == nil && strings.Contains(string(out), "Id")
}

// dockerContainerRunning 返回指定容器是否处于 running 状态。
func dockerContainerRunning(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name).CombinedOutput()
	if err != nil {
		return false // 容器不存在也视为未运行
	}
	return strings.TrimSpace(string(out)) == "true"
}

// waitContainerRunning 轮询等待容器进入 running 状态。
func waitContainerRunning(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if dockerContainerRunning(t, name) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待容器 %s 运行超时（60s）", name)
}

// waitContainerGone 轮询等待容器被删除（孤儿清理/删除收敛断言）。
func waitContainerGone(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, err := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name).CombinedOutput()
		// docker inspect 在容器不存在时返回非零退出码（stderr 形如
		// "No such object: xxx"）——即容器已删除。修复：以 err != nil
		// 作为"已删除"判据（旧实现看输出是否为空，会把 stderr 误判为
		// 容器仍在，导致删除收敛断言超时）。
		if err != nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待容器 %s 删除超时（60s）", name)
}
