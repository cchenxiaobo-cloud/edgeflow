package contract

// 本文件实现 API 兼容性契约的可执行测试（ROADMAP WBS 10.3「无兼容矩阵文档/
// 测试（audit-m35 G4）」），共六组用例：
//
//  1. TestContractRoutesRegisteredAtRuntime —— 真实 cloudcore 子进程运行时探测：
//     契约表每条 method+path 必须已注册（无 404/405）；
//  2. TestContractProbePathsNotRegisteredAtRuntime —— 运行时反向探测：对一组
//     保留前缀的未注册路径断言 404，作为「无契约外路由」的运行时补充断言；
//  3. TestContractRoutesNoExtraRoutesRegistered —— 源码反向断言：遍历
//     cmd/cloudcore/main.go 实际注册的路由表，不允许契约表之外的方法+路径组合
//     （仅白名单内部路径：/healthz 无方法注册、/api/v1/ 前缀挂载）；
//  4. TestDocAPISpecEndpointsMatchContract —— docs/API-SPEC.md §1.1 端点表
//     与契约表逐行比对（method+path 一致）；
//  5. TestDocCompatibilityEndpointsMatchContract —— docs/API-COMPATIBILITY.md §1
//     REST 端点矩阵与契约表比对；
//  6. TestDocCompatibilityMessageTypesMatchProtocol —— docs/API-COMPATIBILITY.md §2
//     消息类型矩阵与 protocol.Type 常量（契约表）双向比对。
//
// 运行方式：go test -race ./tests/contract/
//
// 设计说明（可测入口选择）：cmd/cloudcore 的路由装配内联在 package main 的
// run() 中，nodeAPI 处理器不可从本包导入（Go 禁止 import main 包），因此
// 运行时探测采用与 tests/e2e 相同的「真实进程」约定：go build 编译
// cmd/cloudcore 二进制并以子进程启动（--port + 环境变量覆盖），探测的是真实
// 装配路径而非 mock。反向断言因 net/http 不暴露路由表，改为两道互补防线：
//   - 静态解析（组 3）：读 main.go 路由注册行（全仓 grep 确认
//     HandleFunc/Handle 调用点仅 main.go 一处），定位精确到行号，但只认
//     字面量注册，路由改为循环/变量注册时会漏报；
//   - 运行时反向探测（组 2）：对保留前缀路径断言 404，直接探测真实 ServeMux
//     装配结果，能捕获动态注册的契约外路由；代价是只能抽样探测，无法穷举。

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ============================================================
// 基建：二进制构建 + 子进程生命周期（与 tests/e2e 同约定）
// ============================================================

// repoRoot 定位仓库根目录：本文件位于 <root>/tests/contract/。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位本文件路径")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("解析仓库根目录失败: %v", err)
	}
	return abs
}

var (
	buildOnce sync.Once
	binDir    string // 编译产物目录
	buildErr  error  // 构建失败原因
)

// TestMain 在全部测试结束后清理进程级构建目录 binDir（MkdirTemp 产物）。
//
// 不用 t.Cleanup 的原因：构建是进程级 sync.Once（首个使用二进制的测试触发），
// 而 t.Cleanup 在单个测试结束时执行——首个测试结束即删除目录，会让后续测试的
// exec.Command 找不到二进制（fork/exec: no such file or directory）。
// TestMain 在 m.Run() 返回后才清理，生命周期与目录的创建范围严格一致。
func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// cloudcoreBinary 编译 cmd/cloudcore 二进制（进程级仅一次）。
func cloudcoreBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "edgeflow-contract-bin-")
		if err != nil {
			buildErr = fmt.Errorf("创建二进制目录失败: %w", err)
			return
		}
		binDir = dir
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "cloudcore"), "./cmd/cloudcore")
		cmd.Dir = repoRoot(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("编译 cmd/cloudcore 失败: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("cloudcore 二进制构建失败: %v", buildErr)
	}
	return filepath.Join(binDir, "cloudcore")
}

// cloudcoreProc 是测试管理的 cloudcore 子进程。
type cloudcoreProc struct {
	cmd    *exec.Cmd
	doneCh chan struct{} // Wait 返回后关闭
	mu     sync.Mutex
	buf    bytes.Buffer // stdout+stderr 缓冲，失败时转储
	base   string       // http://127.0.0.1:<port>
}

// logTail 返回进程输出缓冲尾部（失败诊断用）。
func (p *cloudcoreProc) logTail() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	lines := strings.Split(p.buf.String(), "\n")
	if len(lines) > 100 {
		lines = lines[len(lines)-100:]
	}
	return strings.Join(lines, "\n")
}

// stop 停止进程：SIGTERM（优雅）→ 最多等 10s → SIGKILL 兜底。全程有界。
func (p *cloudcoreProc) stop() {
	if p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.doneCh:
		return
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.doneCh:
	case <-time.After(5 * time.Second):
	}
}

// waitExit 等待进程退出（超时返回 ok=false）。
func (p *cloudcoreProc) waitExit(timeout time.Duration) (code int, ok bool) {
	select {
	case <-p.doneCh:
		return p.cmd.ProcessState.ExitCode(), true
	case <-time.After(timeout):
		return -1, false
	}
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

// cloudcoreEnv 构造子进程环境：剔除全部 EDGEFLOW_CLOUDCORE_* 变量后显式设置
// 本测试所需值（防止宿主环境残留变量影响子进程；重复变量在子进程内取值
// 行为未定义，必须先剔除再设置）。
func cloudcoreEnv(t *testing.T) []string {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "EDGEFLOW_CLOUDCORE_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"EDGEFLOW_CLOUDCORE_PORT=",      // 空：--port 优先
		"EDGEFLOW_CLOUDCORE_HUB_PORT=0", // CloudHub 随机端口，避免冲突
		"EDGEFLOW_CLOUDCORE_AUDIT_PATH="+filepath.Join(t.TempDir(), "audit-ledger.jsonl"), // 审计写临时目录
		"EDGEFLOW_CLOUDCORE_TLS=",  // 关闭云边 mTLS
		"EDGEFLOW_CLOUDCORE_AUTH=", // 关闭 API Token 认证（默认即关闭，显式置空防残留）
	)
}

// waitHealthy 轮询 /healthz 直到返回 200 或超时。
func waitHealthy(base string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// startCloudcore 启动真实 cloudcore 子进程并等待 /healthz 就绪。
// 未就绪时清理进程、换端口重试（最多 3 次，端口探测-释放存在竞争窗口）；
// 成功注册 t.Cleanup 统一清理（含失败时转储进程日志）。
func startCloudcore(t *testing.T) *cloudcoreProc {
	t.Helper()
	bin := cloudcoreBinary(t)
	root := repoRoot(t)
	for attempt := 1; attempt <= 3; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("探测空闲端口失败: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()

		p := &cloudcoreProc{doneCh: make(chan struct{})}
		p.base = fmt.Sprintf("http://127.0.0.1:%d", port)
		p.cmd = exec.Command(bin,
			"--port", strconv.Itoa(port),
			"--config", filepath.Join(t.TempDir(), "not-exist.json"))
		p.cmd.Dir = root
		p.cmd.Env = cloudcoreEnv(t)
		p.cmd.Stdout = &syncWriter{w: &p.buf, mu: &p.mu}
		p.cmd.Stderr = &syncWriter{w: &p.buf, mu: &p.mu}
		if err := p.cmd.Start(); err != nil {
			t.Fatalf("启动 cloudcore 失败: %v", err)
		}
		go func() {
			_ = p.cmd.Wait()
			close(p.doneCh)
		}()

		if waitHealthy(p.base, 10*time.Second) {
			t.Cleanup(func() {
				p.stop()
				if t.Failed() {
					t.Logf("===== cloudcore 进程日志尾部 =====\n%s", p.logTail())
				}
			})
			return p
		}

		// 未就绪：输出诊断（含退出码）后清理，换端口重试
		code := "（仍在运行）"
		select {
		case <-p.doneCh:
			code = strconv.Itoa(p.cmd.ProcessState.ExitCode())
		default:
		}
		t.Logf("cloudcore 第 %d 次启动未就绪（退出码 %s），日志尾部:\n%s",
			attempt, code, p.logTail())
		p.stop()
	}
	t.Fatal("cloudcore 子进程 3 次尝试均未就绪")
	return nil
}

// ============================================================
// 测试一：路由覆盖
// ============================================================

// httpClient 带超时的客户端，防止服务端异常导致测试挂起。
var httpClient = &http.Client{Timeout: 5 * time.Second}

// TestContractRoutesRegisteredAtRuntime 对契约表每条端点做运行时探测，
// 验证「该 method+path 已注册」。
//
// 已注册路由与未注册路径的判别（net/http ServeMux 语义）：
//   - 405 = 路径存在但未以契约方法注册 → 契约违约；
//   - 404 + 非 JSON 响应体（text/plain "404 page not found"）= 路径未注册；
//   - 无路径参数端点（/healthz、/metrics、列表类）应精确返回 200；
//   - 有路径参数的 GET（节点不存在）由处理器返回 404 + JSON error 体
//     （Content-Type: application/json，body 以 { 开头）；
//   - POST 端点空 body 由处理器返回 400 + JSON error 体（解码失败即返回，
//     不触发可靠投递，无 5s×3 重试耗时）。
func TestContractRoutesRegisteredAtRuntime(t *testing.T) {
	proc := startCloudcore(t)

	for _, ep := range ContractEndpoints {
		t.Run(ep.Method+" "+ep.Path, func(t *testing.T) {
			path := strings.ReplaceAll(ep.Path, "{nodeID}", "contract-test-node")
			req, err := http.NewRequest(ep.Method, proc.base+path, nil)
			if err != nil {
				t.Fatalf("构造请求失败: %v", err)
			}
			if ep.Method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				t.Fatalf("请求 %s %s 失败: %v", ep.Method, path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			bodyStr := strings.TrimSpace(string(body))

			// 判别 1：405 = 路径存在但方法未按契约注册
			if resp.StatusCode == http.StatusMethodNotAllowed {
				t.Fatalf("路由未以 %s 注册（405）: %s", ep.Method, path)
			}
			// 判别 2：404 且响应体非 JSON = net/http 默认 404 页，路径未注册
			if resp.StatusCode == http.StatusNotFound && !strings.HasPrefix(bodyStr, "{") {
				t.Fatalf("路径未注册（404 且非 JSON 响应）: %s %s → %d body=%q",
					ep.Method, path, resp.StatusCode, bodyStr)
			}

			// 判别 3：各端点按契约期望的精确状态码
			switch {
			case ep.Path == "/ocsp":
				// OCSP 协议端点：空 body 为非法请求 → 400（协议要求 DER 请求体）
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("%s %s 状态码 = %d，期望 400（空 body 为非法 OCSP 请求，body=%q）",
						ep.Method, path, resp.StatusCode, bodyStr)
				}
			case ep.Path == "/healthz", ep.Path == "/metrics", !strings.Contains(ep.Path, "{nodeID}"):
				// 无路径参数端点：200（列表端点空数据返回 200 空列表）
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s %s 状态码 = %d，期望 200（body=%q）",
						ep.Method, path, resp.StatusCode, bodyStr)
				}
			case ep.Method == http.MethodGet:
				// 有路径参数的 GET：未知节点 → 处理器 404 + JSON
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("%s %s 状态码 = %d，期望 404（未知节点，body=%q）",
						ep.Method, path, resp.StatusCode, bodyStr)
				}
			default:
				// POST 端点：空 body → 处理器 400 + JSON
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("%s %s 状态码 = %d，期望 400（空 body 校验失败，body=%q）",
						ep.Method, path, resp.StatusCode, bodyStr)
				}
			}
		})
	}

	// 全部探测完成后验证优雅退出（SIGTERM → 退出码 0）
	if err := proc.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("发送 SIGTERM 失败: %v", err)
	}
	code, ok := proc.waitExit(10 * time.Second)
	if !ok {
		t.Fatal("cloudcore 收到 SIGTERM 后 10s 未退出")
	}
	if code != 0 {
		t.Fatalf("cloudcore 优雅退出码 = %d，期望 0\n日志尾部:\n%s", code, proc.logTail())
	}
}

// registeredRoute 是源码中解析出的一条路由注册。
type registeredRoute struct {
	method string // "GET"/"POST"/"ANY"（无方法限制）/"PREFIX"（前缀挂载）
	path   string
	line   int
}

// registeredRoutesFromSource 静态解析 cmd/cloudcore/main.go 的路由注册。
// 全仓 grep 确认 HandleFunc/Handle 调用点仅 main.go 一处（路由装配集中在 run()）。
func registeredRoutesFromSource(t *testing.T) []registeredRoute {
	t.Helper()
	f, err := os.Open(filepath.Join(repoRoot(t), "cmd", "cloudcore", "main.go"))
	if err != nil {
		t.Fatalf("打开 cmd/cloudcore/main.go 失败: %v", err)
	}
	defer f.Close()

	var routes []registeredRoute
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		// 形如：apiMux.HandleFunc("GET /api/v1/nodes", api.listNodes)
		//       mux.HandleFunc("/healthz", httpx.Healthz())
		//       mux.HandleFunc("GET /metrics", m.Handler())
		if i := strings.Index(line, `HandleFunc("`); i >= 0 {
			rest := line[i+len(`HandleFunc("`):]
			end := strings.Index(rest, `"`)
			if end < 0 {
				t.Fatalf("第 %d 行 HandleFunc 模式解析失败: %q", lineNo, line)
			}
			pattern := rest[:end]
			method, path, ok := strings.Cut(pattern, " ")
			if !ok {
				method, path = "ANY", pattern
			}
			routes = append(routes, registeredRoute{method: method, path: path, line: lineNo})
			continue
		}
		// 前缀挂载：mux.Handle("/api/v1/", apiHandler)
		// （注意 ".Handle(" 不会匹配 ".HandleFunc("，"HandleFunc" 后为 'F'）
		if i := strings.Index(line, `.Handle("`); i >= 0 {
			rest := line[i+len(`.Handle("`):]
			end := strings.Index(rest, `"`)
			if end < 0 {
				t.Fatalf("第 %d 行 Handle 模式解析失败: %q", lineNo, line)
			}
			routes = append(routes, registeredRoute{method: "PREFIX", path: rest[:end], line: lineNo})
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读取 cmd/cloudcore/main.go 失败: %v", err)
	}
	return routes
}

// TestContractRoutesNoExtraRoutesRegistered（反向断言）遍历 cmd/cloudcore/main.go
// 实际注册的路由表，断言不存在契约表之外的方法+路径组合。
//
// 已知内部路径白名单（已逐一确认）：
//   - /healthz：以无方法形式注册（ANY，契约按 GET 探测，实际允许任意方法）；
//   - /api/v1/：管理 API 子 mux 的前缀挂载（认证+审计中间件挂载点），非具体端点。
func TestContractRoutesNoExtraRoutesRegistered(t *testing.T) {
	routes := registeredRoutesFromSource(t)

	contractSet := make(map[string]bool, len(ContractEndpoints))
	for _, ep := range ContractEndpoints {
		contractSet[ep.Method+" "+ep.Path] = true
	}
	// 内部路径白名单：{方法: 路径}
	internal := map[string]string{
		"ANY":    "/healthz", // 健康检查无方法限制注册
		"PREFIX": "/api/v1/", // 管理 API 子 mux 前缀挂载
	}

	var unexpected []string
	for _, r := range routes {
		key := r.method + " " + r.path
		if contractSet[key] {
			continue
		}
		if allow, ok := internal[r.method]; ok && allow == r.path {
			continue
		}
		unexpected = append(unexpected, fmt.Sprintf("main.go 第 %d 行: %s", r.line, key))
	}
	if len(unexpected) > 0 {
		t.Errorf("发现契约表之外的已注册路由 %d 条:\n%s", len(unexpected), strings.Join(unexpected, "\n"))
	}

	// 反向补集：契约每条端点都必须在源码注册表中找到对应注册
	// （/healthz 以 ANY 注册，可满足 GET 契约）。
	registered := make(map[string]bool, len(routes))
	for _, r := range routes {
		registered[r.method+" "+r.path] = true
		registered["ANY "+r.path] = true
	}
	var missing []string
	for _, ep := range ContractEndpoints {
		if !registered[ep.Method+" "+ep.Path] && !registered["ANY "+ep.Path] {
			missing = append(missing, ep.Method+" "+ep.Path)
		}
	}
	if len(missing) > 0 {
		t.Errorf("契约端点未在源码中找到注册 %d 条:\n%s", len(missing), strings.Join(missing, "\n"))
	}
}

// ============================================================
// 测试一补充：运行时反向探测（无契约外路由的运行时断言）
// ============================================================

// contractProbe 是一条运行时反向探测用例。探测路径必须位于「保留前缀」命名空间
// （__probe__ 保留段），保证不可能与未来业务端点重名——若未来业务真的占用了
// 保留段，探测会以 404 期望失败，提示该前缀已不再「保留」。
//
// wantJSON 声明 404 响应体的期望形态（与 TestContractRoutesRegisteredAtRuntime
// 的 405/404 判别逻辑一致）：
//   - false —— 404 + 非 JSON（net/http 默认 "404 page not found" 纯文本页）
//     = 无任何模式命中，路径未注册；
//   - true  —— 404 + JSON error 体 = 仅契约通配参数路由（{nodeID}）命中，
//     处理器对未知实体返回 404；若该路径被字面/动态路由劫持（返回
//     200/405/非 JSON），即为契约外路由信号。
type contractProbe struct {
	method   string
	path     string
	wantJSON bool
	note     string
}

// contractProbePaths 是运行时反向探测的探测路径集合。
//
// 保留前缀约定：
//   - 根级：healthz_probe（/healthz 为精确匹配，不命中该路径；与 /metrics 亦无交集）；
//   - API 命名空间：__probe__ 保留段（双下划线前缀，与业务资源命名
//     nodes/edgenodes/pods/devices 无交集）。
var contractProbePaths = []contractProbe{
	{method: "GET", path: "/healthz_probe", wantJSON: false,
		note: "根级保留路径：/healthz 精确匹配不命中"},
	{method: "GET", path: "/api/v1/__probe__", wantJSON: false,
		note: "API 命名空间一级保留段"},
	{method: "POST", path: "/api/v1/__probe__", wantJSON: false,
		note: "方法变体：未注册路径的 POST 必须同为 404（405=路径存在但方法不符）"},
	{method: "GET", path: "/api/v1/__probe__/nested", wantJSON: false,
		note: "API 命名空间深层保留段"},
	{method: "GET", path: "/api/v1/__probe__/pods", wantJSON: false,
		note: "pods 子命名空间形态的保留首段（nodes 段被替换为保留段）"},
	{method: "GET", path: "/api/v1/nodes/__probe__", wantJSON: true,
		note: "仅契约通配 {nodeID} 命中 → 处理器 404+JSON（未知节点）"},
	{method: "GET", path: "/api/v1/edgenodes/__probe__", wantJSON: true,
		note: "仅契约通配 {nodeID} 命中 → 处理器 404+JSON"},
	{method: "GET", path: "/api/v1/nodes/__probe__/pods", wantJSON: true,
		note: "仅契约通配子路由命中 → 处理器 404+JSON"},
	{method: "GET", path: "/api/v1/nodes/__probe__/devices", wantJSON: true,
		note: "仅契约通配子路由命中 → 处理器 404+JSON"},
}

// TestContractProbePathsNotRegisteredAtRuntime（运行时反向探测）对一组明确的
// 未注册/保留前缀路径发起真实请求，断言一律返回 404，作为「无契约外路由」的
// 运行时补充断言。
//
// 背景：TestContractRoutesNoExtraRoutesRegistered 只解析 main.go 的字面
// HandleFunc/Handle 调用，路由改为循环/变量注册时会漏报（动态注册的契约外
// 路由不可见）。本测试探测真实 ServeMux 装配结果，动态注册的路由（尤其是
// 挂在保留段上的字面路由）会在此暴露；两者并存，静态解析仍是第一道防线
// （定位精确、零运行成本）。
//
// 404 语义与既有判别一致：
//   - 404 + 非 JSON = 无任何模式命中（路径未注册，符合期望）；
//   - 404 + JSON = 契约通配参数路由命中，处理器对未知实体返回 404（符合期望）；
//   - 405 = 路径存在但方法未注册 → 契约外路由信号，直接失败；
//   - 其他状态码（200/400/401/…）= 路径被契约外路由处理 → 直接失败。
func TestContractProbePathsNotRegisteredAtRuntime(t *testing.T) {
	proc := startCloudcore(t)

	for _, probe := range contractProbePaths {
		t.Run(probe.method+" "+probe.path, func(t *testing.T) {
			req, err := http.NewRequest(probe.method, proc.base+probe.path, nil)
			if err != nil {
				t.Fatalf("构造请求失败: %v", err)
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				t.Fatalf("请求 %s %s 失败: %v", probe.method, probe.path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			bodyStr := strings.TrimSpace(string(body))

			// 405 = 路径存在但方法未按契约注册（契约外路由信号）
			if resp.StatusCode == http.StatusMethodNotAllowed {
				t.Fatalf("探测路径以其他方法注册（契约外路由）: %s %s → 405",
					probe.method, probe.path)
			}
			// 契约外路由（字面/动态注册）会返回非 404 状态码
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("探测路径状态码 = %d，期望 404（%s）; body=%q",
					resp.StatusCode, probe.note, bodyStr)
			}
			// 404 形态判别（与运行时探测的判别一致）：
			// 非 JSON = 路径未注册；JSON = 通配参数路由命中（处理器正常返回）
			isJSON := strings.HasPrefix(bodyStr, "{")
			if isJSON && !probe.wantJSON {
				t.Fatalf("探测路径期望 404+非 JSON（路径未注册），实际返回 JSON 体: %s %s → body=%q",
					probe.method, probe.path, bodyStr)
			}
			if !isJSON && probe.wantJSON {
				t.Fatalf("探测路径期望 404+JSON（通配参数路由命中），实际返回非 JSON 体: %s %s → body=%q",
					probe.method, probe.path, bodyStr)
			}
		})
	}
}

// ============================================================
// 测试二：文档一致性
// ============================================================

// docSection 提取文档指定小节的行（从 startHeading 到下一个标题行）。
func docSection(t *testing.T, path, startHeading string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == startHeading {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("文档 %s 未找到小节 %q", path, startHeading)
	}
	var out []string
	for _, ln := range lines[start+1:] {
		if strings.HasPrefix(ln, "#") {
			break
		}
		out = append(out, ln)
	}
	return out
}

// httpMethods 是表格行解析接受的 HTTP 方法集合。
var httpMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"PATCH": true, "HEAD": true, "OPTIONS": true,
}

// splitTableRow 拆分 markdown 表格行（保留单元格语义）。
func splitTableRow(line string) []string {
	trimmed := strings.Trim(line, "|")
	parts := strings.Split(trimmed, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

// parseEndpointRows 从小节行中解析端点表格行（简单字符串解析，不引入依赖）。
// 兼容两种列布局：API-SPEC §1.1（| 方法 | 路径 | ...）与
// API-COMPATIBILITY §1（| # | 方法 | 路径 | ...）。
// 判据：某单元格为 HTTP 方法名，且其后某单元格以 "/" 开头 → (方法, 路径)。
func parseEndpointRows(t *testing.T, lines []string) []Endpoint {
	t.Helper()
	var out []Endpoint
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "|") {
			continue
		}
		method, path := "", ""
		for _, c := range splitTableRow(ln) {
			c = strings.Trim(strings.TrimSpace(c), "`")
			if method == "" && httpMethods[c] {
				method = c
				continue
			}
			if method != "" && strings.HasPrefix(c, "/") {
				path = c
				break
			}
		}
		if method != "" && path != "" {
			out = append(out, Endpoint{Method: method, Path: path})
		}
	}
	return out
}

// parseMessageTypeRows 从消息矩阵小节中解析类型名列表。
// 判据：行首单元格为裸 ASCII 单词（可带反引号）。
// 注意：方向列不统一含 "→"（如 Ack 行为 "双向"），故不以方向标记为判据；
// 表头行首单元格为中文（"类型"），不满足 isBareWord 自然跳过。
func parseMessageTypeRows(t *testing.T, lines []string) []string {
	t.Helper()
	var out []string
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "|") {
			continue
		}
		cells := splitTableRow(ln)
		if len(cells) == 0 {
			continue
		}
		name := strings.Trim(strings.TrimSpace(cells[0]), "`")
		if !isBareWord(name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// isBareWord 判断字符串是否为纯字母数字单词（消息类型名形态）。
func isBareWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// compareEndpointSets 比对文档解析结果与契约表（双向），输出差异明细。
func compareEndpointSets(t *testing.T, source string, doc []Endpoint) {
	t.Helper()
	docSet := make(map[string]Endpoint, len(doc))
	for _, ep := range doc {
		docSet[ep.Method+" "+ep.Path] = ep
	}
	contractSet := make(map[string]Endpoint, len(ContractEndpoints))
	for _, ep := range ContractEndpoints {
		contractSet[ep.Method+" "+ep.Path] = ep
	}
	var missing, extra []string
	for key := range contractSet {
		if _, ok := docSet[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range docSet {
		if _, ok := contractSet[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s 缺少契约端点 %d 条:\n%s", source, len(missing), strings.Join(missing, "\n"))
	}
	if len(extra) > 0 {
		t.Errorf("%s 存在契约表之外的端点 %d 条:\n%s", source, len(extra), strings.Join(extra, "\n"))
	}
	if len(missing) == 0 && len(extra) == 0 {
		t.Logf("%s 端点表与契约一致（%d 条）", source, len(contractSet))
	}
}

// TestDocAPISpecEndpointsMatchContract 解析 docs/API-SPEC.md §1.1 端点总览表格，
// 与契约表逐行比对（method+path 一致）。
func TestDocAPISpecEndpointsMatchContract(t *testing.T) {
	lines := docSection(t, filepath.Join(repoRoot(t), "docs", "API-SPEC.md"), "### 1.1 端点总览")
	compareEndpointSets(t, "docs/API-SPEC.md §1.1", parseEndpointRows(t, lines))
}

// TestDocCompatibilityEndpointsMatchContract 解析 docs/API-COMPATIBILITY.md §1
// REST API 端点矩阵，与契约表逐行比对。
func TestDocCompatibilityEndpointsMatchContract(t *testing.T) {
	lines := docSection(t,
		filepath.Join(repoRoot(t), "docs", "API-COMPATIBILITY.md"),
		"## 1. REST API 端点矩阵（cloudcore，HTTP :8080）")
	compareEndpointSets(t, "docs/API-COMPATIBILITY.md §1", parseEndpointRows(t, lines))
}

// TestDocCompatibilityMessageTypesMatchProtocol 解析 docs/API-COMPATIBILITY.md §2
// 云边通道消息类型矩阵，与契约表（protocol.Type 常量，编译期绑定）双向比对：
//   - 契约表中 InDoc=true 的类型必须出现在文档，InDoc=false 必须不出现；
//   - 文档中的类型必须全部存在于契约表。
func TestDocCompatibilityMessageTypesMatchProtocol(t *testing.T) {
	lines := docSection(t,
		filepath.Join(repoRoot(t), "docs", "API-COMPATIBILITY.md"),
		"## 2. 云边通道消息类型矩阵（WebSocket /v1/edge）")
	docTypes := parseMessageTypeRows(t, lines)

	docSet := make(map[string]bool, len(docTypes))
	for _, name := range docTypes {
		docSet[name] = true
	}
	contractSet := make(map[string]MessageType, len(ContractMessageTypes))
	for _, mt := range ContractMessageTypes {
		contractSet[mt.Name] = mt
	}

	var missing, extra []string
	for _, mt := range ContractMessageTypes {
		if mt.InDoc && !docSet[mt.Name] {
			missing = append(missing, mt.Name+"（契约 InDoc=true 但文档矩阵未收录）")
		}
		if !mt.InDoc && docSet[mt.Name] {
			extra = append(extra, mt.Name+"（契约 InDoc=false 但文档矩阵收录）")
		}
	}
	for name := range docSet {
		if _, ok := contractSet[name]; !ok {
			extra = append(extra, name+"（文档收录但契约表中不存在）")
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("docs/API-COMPATIBILITY.md §2 缺少契约消息类型 %d 条:\n%s",
			len(missing), strings.Join(missing, "\n"))
	}
	if len(extra) > 0 {
		t.Errorf("docs/API-COMPATIBILITY.md §2 存在契约之外的消息类型 %d 条:\n%s",
			len(extra), strings.Join(extra, "\n"))
	}
	if len(missing) == 0 && len(extra) == 0 {
		inDoc, closed := 0, 0
		for _, mt := range ContractMessageTypes {
			if mt.InDoc {
				inDoc++
			} else {
				closed++
			}
		}
		t.Logf("docs/API-COMPATIBILITY.md §2 消息类型与契约一致（活跃 %d 条；%d 条关闭占位按约定不收录）",
			inDoc, closed)
	}
}
