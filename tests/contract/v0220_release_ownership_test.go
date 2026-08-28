package contract

// v0.22.0 契约测试：release 子资源跨模型归属校验统一（T-09，CLD-06/CLD-04）。
//
// 口径：/api/v1/models/{m}/releases/{id} 系列子资源端点中，release 不属于
// URL 中模型（跨模型引用）一律 404（ErrReleaseNotFound 语义，与 v0.19.0
// snapshot / v0.20.0 retry/DELETE 既有行为一致；v0.22.0 起经统一 helper
// ownedRelease 补齐缺口端点）。
//
// 覆盖端点（7 缺口 + 3 既有合规回归锚）：
//
//	GET    /api/v1/models/{m}/releases/{id}             发布详情
//	GET    /api/v1/models/{m}/releases/{id}/digest      digest 复核
//	GET    /api/v1/models/{m}/releases/{id}/snapshot    审计快照（回归锚）
//	PATCH  /api/v1/models/{m}/releases/{id}             运行中可调参数
//	POST   /api/v1/models/{m}/releases/{id}/cancel      取消
//	POST   /api/v1/models/{m}/releases/{id}/pause       暂停
//	POST   /api/v1/models/{m}/releases/{id}/resume      恢复
//	POST   /api/v1/models/{m}/releases/{id}/rollback    回滚
//	POST   /api/v1/models/{m}/releases/{id}/retry       失败节点重试（回归锚）
//	DELETE /api/v1/models/{m}/releases/{id}             终态归档删除（回归锚）
//
// 装配方式与 api_contract_test.go 同约定（真实 cloudcore 子进程）；因创建
// release 需注册节点（白名单校验 422 族），本文件自带最小 edgecore 子进程
// 装配（镜像 tests/e2e 的 edgeEnv/waitNodeRegistered 约定）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── edgecore 二进制（进程级一次编译；binDir/TestMain 清理复用本包基建）──

var (
	v0220EdgeOnce sync.Once
	v0220EdgeBin  string
	v0220EdgeErr  error
)

// v0220EdgecoreBinary 编译 cmd/edgecore（cloudcore 已由 cloudcoreBinary
// 建立 binDir，edgecore 产物放进同一目录，TestMain 统一清理）。
func v0220EdgecoreBinary(t *testing.T) string {
	t.Helper()
	cloudcoreBinary(t) // 确保 binDir 就绪（失败即 Fatal）
	v0220EdgeOnce.Do(func() {
		v0220EdgeBin = filepath.Join(binDir, "edgecore")
		cmd := exec.Command("go", "build", "-o", v0220EdgeBin, "./cmd/edgecore")
		cmd.Dir = repoRoot(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			v0220EdgeErr = fmt.Errorf("编译 cmd/edgecore 失败: %v\n%s", err, out)
		}
	})
	if v0220EdgeErr != nil {
		t.Fatalf("edgecore 二进制构建失败: %v", v0220EdgeErr)
	}
	return v0220EdgeBin
}

// ── 最小云边双进程装配（cloudcore 固定 hub 端口 + edgecore 接入）────────

// v0220Stack 是一套运行中的云边双进程。
type v0220Stack struct {
	base    string // cloudcore HTTP API 根地址
	cleanup func()
}

// v0220CloudcoreEnv 构造 cloudcore 子进程环境（与 cloudcoreEnv 同源，但
// HUB_PORT 固定为传入端口，供 edgecore 接入）。
func v0220CloudcoreEnv(t *testing.T, hubPort int) []string {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "EDGEFLOW_CLOUDCORE_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"EDGEFLOW_CLOUDCORE_PORT=", // 空：--port 优先
		"EDGEFLOW_CLOUDCORE_HUB_PORT="+strconv.Itoa(hubPort),
		"EDGEFLOW_CLOUDCORE_AUDIT_PATH="+filepath.Join(t.TempDir(), "audit-ledger.jsonl"),
		"EDGEFLOW_CLOUDCORE_TLS=",
		"EDGEFLOW_CLOUDCORE_AUTH=",
	)
}

// v0220EdgeEnv 构造 edgecore 子进程环境（镜像 tests/e2e edgeEnv：快超时、
// MQTT 指向不可达端口避免阻塞，仅注册/心跳链路生效）。
func v0220EdgeEnv(nodeID, cloudAddr, dbPath string) []string {
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

// v0220ReservePort 预约空闲端口（探测后释放，与 e2e reservePort 同约定）。
func v0220ReservePort(t *testing.T) int {
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
	t.Fatal("预约端口失败")
	return 0
}

// v0220StartStack 启动 cloudcore（HTTP+hub 固定端口）+ edgecore（注册节点）
// 并等待节点就绪；失败路径全部有界清理。
func v0220StartStack(t *testing.T, nodeID string) *v0220Stack {
	t.Helper()
	root := repoRoot(t)
	bin := cloudcoreBinary(t)
	httpPort := v0220ReservePort(t)
	hubPort := v0220ReservePort(t)

	// cloudcore：固定 HTTP/hub 端口（等待 /healthz 就绪，最多 3 次换端口）。
	var ccCmd *exec.Cmd
	base := ""
	for attempt := 1; attempt <= 3; attempt++ {
		base = fmt.Sprintf("http://127.0.0.1:%d", httpPort)
		ccCmd = exec.Command(bin, "--port", strconv.Itoa(httpPort),
			"--config", filepath.Join(t.TempDir(), "not-exist.json"))
		ccCmd.Dir = root
		ccCmd.Env = v0220CloudcoreEnv(t, hubPort)
		if err := ccCmd.Start(); err != nil {
			t.Fatalf("启动 cloudcore 失败: %v", err)
		}
		if waitHealthy(base, 10*time.Second) {
			break
		}
		_ = ccCmd.Process.Kill()
		_, _ = ccCmd.Process.Wait()
		if attempt == 3 {
			t.Fatal("cloudcore 子进程 3 次尝试均未就绪")
		}
		httpPort = v0220ReservePort(t)
		hubPort = v0220ReservePort(t)
	}

	// edgecore：接入 hub 完成节点注册。
	edgeBin := v0220EdgecoreBinary(t)
	ecCmd := exec.Command(edgeBin)
	ecCmd.Dir = root
	ecCmd.Env = v0220EdgeEnv(nodeID, "ws://127.0.0.1:"+strconv.Itoa(hubPort), filepath.Join(t.TempDir(), "edge.db"))
	var ecBuf bytes.Buffer
	ecCmd.Stdout = &ecBuf
	ecCmd.Stderr = &ecBuf
	if err := ecCmd.Start(); err != nil {
		_ = ccCmd.Process.Kill()
		t.Fatalf("启动 edgecore 失败: %v", err)
	}

	stack := &v0220Stack{base: base}
	stack.cleanup = func() {
		_ = ecCmd.Process.Signal(os.Interrupt)
		select {
		case <-time.After(3 * time.Second):
			_ = ecCmd.Process.Kill()
		}
		_ = ccCmd.Process.Kill()
		_, _ = ccCmd.Process.Wait()
	}
	t.Cleanup(stack.cleanup)

	// 等待节点注册（/api/v1/nodes 出现 nodeID，60s 上限）。
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/v1/nodes")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if strings.Contains(string(body), `"nodeID":"`+nodeID+`"`) {
				return stack
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("等待节点 %s 注册超时（60s）；edgecore 日志尾部:\n%s", nodeID, ecBuf.String())
	return nil
}

// ── 请求辅助 ─────────────────────────────────────────────────────────

// v0220Do 发送请求并解析 JSON 响应（非 JSON 响应返回空 map）。
func v0220Do(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// v0220MustModelVersion 一次到位：建模型 + 版本 + 激活。
func v0220MustModelVersion(t *testing.T, base, model, version string) {
	t.Helper()
	code, out := v0220Do(t, http.MethodPost, base+"/api/v1/models", mapJSON(t, map[string]any{
		"name": model, "description": "v0220 归属校验测试", "type": "generic",
	}))
	if code != http.StatusOK && code != http.StatusConflict {
		t.Fatalf("创建模型 %s = %d（%v）", model, code, out)
	}
	code, out = v0220Do(t, http.MethodPost, base+"/api/v1/models/"+model+"/versions", mapJSON(t, map[string]any{
		"version": version, "mirror": "reg.example.com/x:v1", "sha256": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	}))
	if code != http.StatusOK && code != http.StatusConflict {
		t.Fatalf("创建版本 %s/%s = %d（%v）", model, version, code, out)
	}
	code, out = v0220Do(t, http.MethodPost, base+"/api/v1/models/"+model+"/versions/"+version+"/activate", "")
	if code != http.StatusOK {
		t.Fatalf("激活版本 %s/%s = %d（%v）", model, version, code, out)
	}
}

// v0220MustCreateRelease 创建发布并返回 releaseID（202）。
func v0220MustCreateRelease(t *testing.T, base, model, version, nodeID string) string {
	t.Helper()
	code, out := v0220Do(t, http.MethodPost, base+"/api/v1/models/"+model+"/releases", mapJSON(t, map[string]any{
		"version": version,
		"target":  map[string]any{"type": "nodeIDs", "nodeIDs": []string{nodeID}},
	}))
	if code != http.StatusAccepted {
		t.Fatalf("创建发布 %s/%s = %d（%v）", model, version, code, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("创建发布响应缺 id: %v", out)
	}
	return id
}

// mapJSON 序列化为 JSON 字符串（请求体构造）。
func mapJSON(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	return string(b)
}

// ── 用例 ─────────────────────────────────────────────────────────────

// v0220OwnershipEndpoint 是一条子资源访问用例。
type v0220OwnershipEndpoint struct {
	name   string
	method string
	suffix string // 拼接在 /api/v1/models/%s/releases/%s 之后（"" = 详情本身）
	body   string
}

// v0220OwnershipEndpoints 是全部 10 条子资源端点（7 缺口 + 3 回归锚）。
var v0220OwnershipEndpoints = []v0220OwnershipEndpoint{
	{name: "详情", method: http.MethodGet},
	{name: "digest复核", method: http.MethodGet, suffix: "/digest"},
	{name: "审计快照", method: http.MethodGet, suffix: "/snapshot"},
	{name: "可调参数PATCH", method: http.MethodPatch, body: `{"batchSize":2}`},
	{name: "取消", method: http.MethodPost, suffix: "/cancel"},
	{name: "暂停", method: http.MethodPost, suffix: "/pause"},
	{name: "恢复", method: http.MethodPost, suffix: "/resume"},
	{name: "回滚", method: http.MethodPost, suffix: "/rollback"},
	{name: "重试", method: http.MethodPost, suffix: "/retry"},
	{name: "归档删除", method: http.MethodDelete},
}

// TestV0220ReleaseOwnership 跨模型引用一律 404：构造 m1/m2 两模型，release
// 挂在 m2（own-model）下，以 m1（other-model）路径访问 10 条子资源 → 全部
// 404 且响应为 JSON error 体；正确归属路径访问 → 一律非 404（按各自状态机
// 语义响应：200/202/409/422 均合法，证明 404 来自归属校验而非路由缺失）。
// 另含模型不存在（ghost）→ 404 先行的链序回归锚。
func TestV0220ReleaseOwnership(t *testing.T) {
	// 唯一后缀：嵌入式 etcd（dataDir=data/etcd，cwd=仓库根）跨运行持久化，
	// 模型/版本名必须每次运行唯一，否则 activate 409（status=active 残留）。
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	mCorrect := "v0220-own-" + suffix // release 实际归属
	mOther := "v0220-other-" + suffix // 干扰模型（存在、无 release）
	mGhost := "v0220-ghost-" + suffix // 不存在的模型
	const (
		version = "v1"
		nodeID  = "v0220-node-a"
	)
	stack := v0220StartStack(t, nodeID)
	base := stack.base

	v0220MustModelVersion(t, base, mCorrect, version)
	v0220MustModelVersion(t, base, mOther, version)
	relID := v0220MustCreateRelease(t, base, mCorrect, version, nodeID)

	detail := "/api/v1/models/%s/releases/%s"

	// 1) 跨模型（m1 路径 + m2 的 release）→ 404。
	for _, ep := range v0220OwnershipEndpoints {
		ep := ep
		t.Run("跨模型_"+ep.name, func(t *testing.T) {
			path := fmt.Sprintf(detail+ep.suffix, mOther, relID)
			code, out := v0220Do(t, ep.method, base+path, ep.body)
			if code != http.StatusNotFound {
				t.Fatalf("跨模型 %s %s 应 404，实际 %d（body=%v）", ep.method, path, code, out)
			}
			if msg, _ := out["error"].(string); msg == "" {
				t.Fatalf("404 应为 JSON error 体，实际 %v", out)
			}
		})
	}

	// 2) 模型不存在 → 404 先行（C-4 链序纪律回归锚）。
	for _, ep := range v0220OwnershipEndpoints[:3] {
		ep := ep
		t.Run("模型不存在_"+ep.name, func(t *testing.T) {
			path := fmt.Sprintf(detail+ep.suffix, mGhost, relID)
			code, out := v0220Do(t, ep.method, base+path, ep.body)
			if code != http.StatusNotFound {
				t.Fatalf("模型不存在 %s %s 应 404，实际 %d（body=%v）", ep.method, path, code, out)
			}
		})
	}

	// 3) 正确归属路径 → 一律非 404（无论 release 处于什么状态机位置：
	//    200/202/409/422 均为端点自身语义；404 只允许出现在归属/存在性缺失）。
	for _, ep := range v0220OwnershipEndpoints {
		ep := ep
		t.Run("正确归属_"+ep.name, func(t *testing.T) {
			path := fmt.Sprintf(detail+ep.suffix, mCorrect, relID)
			code, out := v0220Do(t, ep.method, base+path, ep.body)
			if code == http.StatusNotFound {
				t.Fatalf("正确归属 %s %s 不应 404（body=%v）", ep.method, path, out)
			}
		})
	}
}

// TestV0220ReleaseOwnershipAbsentRelease 回归锚：release 本身不存在 → 404
// （仅 cloudcore，无需 edgecore；v0.22.0 后统一走 ownedRelease 链）。
func TestV0220ReleaseOwnershipAbsentRelease(t *testing.T) {
	proc := startCloudcore(t)
	model := "v0220-absent-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	v0220MustModelVersion(t, proc.base, model, "v1")
	code, out := v0220Do(t, http.MethodGet,
		proc.base+"/api/v1/models/"+model+"/releases/no-such-release", "")
	if code != http.StatusNotFound {
		t.Fatalf("不存在的 release 应 404，实际 %d（body=%v）", code, out)
	}
}
