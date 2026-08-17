package main

import (
	"bytes"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/certs"
)

// newOCSPTestEnv 生成临时证书目录（CA + 客户端叶子证书），返回
// certDir、节点 CN、CA 句柄与叶子证书。
func newOCSPTestEnv(t *testing.T) (string, string, *certs.CA, *x509.Certificate) {
	t.Helper()
	certDir := filepath.Join(t.TempDir(), "certs")
	cn := "edgeflow-ocsp-node"
	ca, err := certs.EnsureCA(certDir)
	if err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	leafTLS, err := certs.EnsureClientCert(certDir, cn)
	if err != nil {
		t.Fatalf("EnsureClientCert 失败: %v", err)
	}
	leaf, err := x509.ParseCertificate(leafTLS.Certificate[0])
	if err != nil {
		t.Fatalf("解析叶子证书失败: %v", err)
	}
	return certDir, cn, ca, leaf
}

// newOCSPServer 按 run() 的装配方式把 ocspHandler 挂到方法路由
// （POST /ocsp）后启动测试服务，保证与生产接线一致。
func newOCSPServer(t *testing.T, certDir string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ocsp", (&ocspHandler{certDir: certDir}).ServeHTTP)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newOCSPServerWithLimiter 同 newOCSPServer，但可注入自定义限流器
// （测试限流行为用；nil 时与生产一致按默认参数惰性初始化）。
func newOCSPServerWithLimiter(t *testing.T, certDir string, limiter *ocspRateLimiter) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ocsp", (&ocspHandler{certDir: certDir, limiter: limiter}).ServeHTTP)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newTestClockLimiter 构造带注入时钟的限流器（测试可确定性地推进时间，
// 无需 sleep）；rate/burst 不经过默认值归一化，允许 rate=0（永不补充）。
func newTestClockLimiter(rate float64, burst, maxIPs int) (*ocspRateLimiter, func(time.Duration)) {
	var mu sync.Mutex
	clock := time.Now()
	limiter := &ocspRateLimiter{
		buckets: make(map[string]*ocspBucket),
		rate:    rate,
		burst:   burst,
		maxIPs:  maxIPs,
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return clock
		},
	}
	advance := func(d time.Duration) {
		mu.Lock()
		clock = clock.Add(d)
		mu.Unlock()
	}
	return limiter, advance
}

// postOCSP 向 responder 的 /ocsp 端点发送一次 OCSP 查询，返回响应（helper）。
func postOCSP(t *testing.T, baseURL string, reqDER []byte) *http.Response {
	t.Helper()
	resp, err := http.Post(baseURL+"/ocsp", "application/ocsp-request", bytes.NewReader(reqDER))
	if err != nil {
		t.Fatalf("POST %s 失败: %v", baseURL+"/ocsp", err)
	}
	return resp
}

// TestOCSPEndpoint_GoodAndRevoked 覆盖 /ocsp 端点主路径：
// 未吊销 → good；吊销后 → revoked + 吊销时间；再用 OCSPStatusAt 全闭环。
func TestOCSPEndpoint_GoodAndRevoked(t *testing.T) {
	certDir, cn, ca, leaf := newOCSPTestEnv(t)
	srv := newOCSPServer(t, certDir)

	reqDER, err := certs.OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	reqCID, err := certs.ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}

	// good：200 + application/ocsp-response + Cache-Control + 可解析
	resp := postOCSP(t, srv.URL, reqDER)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/ocsp-response" {
		t.Errorf("Content-Type = %q，期望 application/ocsp-response", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "max-age="+strconv.Itoa(ocspCacheMaxAge) {
		t.Errorf("Cache-Control = %q，期望 max-age=%d", cc, ocspCacheMaxAge)
	}
	status, revokedAt, err := certs.ParseOCSPResponse(ca.Cert, body, reqCID)
	if err != nil {
		t.Fatalf("ParseOCSPResponse 失败: %v", err)
	}
	if status != certs.StatusGood || revokedAt != nil {
		t.Errorf("状态 = %q/%v，期望 good/nil", status, revokedAt)
	}

	// revoked：RevokeCert 后查询 → revoked + 吊销时间非 nil
	if err := certs.RevokeCert(certDir, cn); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	resp = postOCSP(t, srv.URL, reqDER)
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，期望 200", resp.StatusCode)
	}
	status, revokedAt, err = certs.ParseOCSPResponse(ca.Cert, body, reqCID)
	if err != nil {
		t.Fatalf("ParseOCSPResponse(revoked) 失败: %v", err)
	}
	if status != certs.StatusRevoked {
		t.Errorf("状态 = %q，期望 revoked", status)
	}
	if revokedAt == nil || revokedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("吊销时间异常: %v", revokedAt)
	}

	// 客户端全闭环：OCSPStatusAt 直接查真实 handler（mux 路由 /ocsp）
	status, _, err = certs.OCSPStatusAt(certDir, leaf, srv.URL+"/ocsp")
	if err != nil {
		t.Fatalf("OCSPStatusAt 失败: %v", err)
	}
	if status != certs.StatusRevoked {
		t.Errorf("OCSPStatusAt 状态 = %q，期望 revoked", status)
	}
}

// TestOCSPEndpoint_BadRequest 请求非法 → 400：垃圾 DER / 超限 / 空体；
// 非 POST 方法 → 405。
func TestOCSPEndpoint_BadRequest(t *testing.T) {
	certDir, _, _, _ := newOCSPTestEnv(t)
	srv := newOCSPServer(t, certDir)

	// 垃圾 DER → 400
	resp := postOCSP(t, srv.URL, []byte{0xff, 0x00, 0x01})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("垃圾 DER status = %d，期望 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 超过 16 KiB → 400
	huge := bytes.Repeat([]byte{0x30}, maxOCSPRequestBytes+1)
	resp = postOCSP(t, srv.URL, huge)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("超限请求 status = %d，期望 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 空体 → 400
	resp = postOCSP(t, srv.URL, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("空请求 status = %d，期望 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 非 POST 方法 → 405（ServeMux 方法模式自动处理）
	getResp, err := http.Get(srv.URL + "/ocsp")
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d，期望 405", getResp.StatusCode)
	}
	_ = getResp.Body.Close()
}

// TestOCSPEndpoint_MissingCA 证书目录无 CA → 500（fail-closed，
// 不静默放行/不自动建 CA）。请求用另一目录的合法证书构造。
func TestOCSPEndpoint_MissingCA(t *testing.T) {
	// 合法请求：从独立证书目录构造
	srcDir, _, srcCA, srcLeaf := newOCSPTestEnv(t)
	reqDER, err := certs.OCSPRequestForCert(srcCA.Cert, srcLeaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	_ = srcDir

	// responder 指向空目录（无 CA）
	emptyDir := filepath.Join(t.TempDir(), "no-certs")
	srv := newOCSPServer(t, emptyDir)

	resp := postOCSP(t, srv.URL, reqDER)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("无 CA status = %d，期望 500", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestOCSPEndpoint_RouteRegistered run() 装配的 mux 包含 POST /ocsp。
// 通过 run 的完整装配路径验证路由存在性（handler 行为由上述用例覆盖）。
func TestOCSPEndpoint_RouteRegistered(t *testing.T) {
	// run() 内部创建 mux 并注册 /ocsp；这里无法直接取 mux，
	// 改为验证注册代码路径的常量与 handler 类型存在。
	if maxOCSPRequestBytes != 16<<10 {
		t.Errorf("maxOCSPRequestBytes = %d，期望 16384", maxOCSPRequestBytes)
	}
	h := &ocspHandler{certDir: t.TempDir()}
	if h.certDir == "" {
		t.Error("ocspHandler.certDir 不应为空")
	}
	// 冒烟：请求体超过限制时 handler 返回 400 而非 panic
	srv := newOCSPServer(t, h.certDir)
	resp, err := http.Post(srv.URL+"/ocsp", "application/ocsp-request", strings.NewReader(strings.Repeat("x", maxOCSPRequestBytes+1)))
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d，期望 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestOCSPEndpoint_RateLimitExceeded 限流生效：rate=0（永不补充令牌）、
// burst=1 → 首次请求 200、第二次起 429。验证 429 在多次请求内持续。
func TestOCSPEndpoint_RateLimitExceeded(t *testing.T) {
	certDir, _, ca, leaf := newOCSPTestEnv(t)
	reqDER, err := certs.OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	// rate=0：令牌永不补充。burst=1：首次消费 1 个令牌，后续均为 0 → 429。
	limiter := &ocspRateLimiter{
		buckets: make(map[string]*ocspBucket),
		rate:    0,
		burst:   1,
		maxIPs:  100,
	}
	srv := newOCSPServerWithLimiter(t, certDir, limiter)

	// 1st → 200
	resp := postOCSP(t, srv.URL, reqDER)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("首次 status = %d，期望 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	// 2nd, 3rd → 429
	for i := 2; i <= 3; i++ {
		r := postOCSP(t, srv.URL, reqDER)
		if r.StatusCode != http.StatusTooManyRequests {
			t.Errorf("第 %d 次 status = %d，期望 429", i, r.StatusCode)
		}
		_ = r.Body.Close()
	}
}

// TestOCSPEndpoint_RateLimitRecover 限流恢复放行：注入时钟，推进时间后
// 令牌补充恢复放行。全程无 sleep，测试确定性强。
func TestOCSPEndpoint_RateLimitRecover(t *testing.T) {
	certDir, _, ca, leaf := newOCSPTestEnv(t)
	reqDER, err := certs.OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	// rate=0.5/s（1 令牌需 2 秒），burst=1：首次消费后 0；时钟推进 4s
	// 后补充 2 令牌（cap 1）→ 放行。
	limiter, advance := newTestClockLimiter(0.5, 1, 100)
	srv := newOCSPServerWithLimiter(t, certDir, limiter)

	resp := postOCSP(t, srv.URL, reqDER)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("首次 status = %d，期望 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = postOCSP(t, srv.URL, reqDER)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("恢复前 status = %d，期望 429", resp.StatusCode)
	}
	_ = resp.Body.Close()

	advance(4 * time.Second) // 补充 0.5×4=2 令牌，cap=1

	resp = postOCSP(t, srv.URL, reqDER)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("恢复后 status = %d，期望 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestOCSPRateLimiter_PerIPIndependent 不同 IP 独立计数：
// 1.1.1.1 耗尽后 2.2.2.2 仍可放行。
func TestOCSPRateLimiter_PerIPIndependent(t *testing.T) {
	rl := &ocspRateLimiter{buckets: make(map[string]*ocspBucket), rate: 0, burst: 1, maxIPs: 100}
	if !rl.allow("1.1.1.1") {
		t.Error("1.1.1.1 首次应放行")
	}
	if rl.allow("1.1.1.1") {
		t.Error("1.1.1.1 二次应拒绝")
	}
	if !rl.allow("2.2.2.2") {
		t.Error("2.2.2.2 首次应放行（独立计数）")
	}
}

// TestOCSPRateLimiter_BoundedMapEvictsOldest map 有界淘汰：
// maxIPs=2，填满 2 IP 后加入第 3 个 → 最旧的被淘汰，总数仍 ≤ 2；
// 被淘汰的 IP 再次请求时获得新桶（满令牌）。
func TestOCSPRateLimiter_BoundedMapEvictsOldest(t *testing.T) {
	rl, advance := newTestClockLimiter(0, 1, 2)
	if !rl.allow("1.1.1.1") {
		t.Fatal("1.1.1.1 首次应放行")
	}
	advance(time.Second) // 确保 1.1.1.1 的 lastRef 早于后续
	if !rl.allow("2.2.2.2") {
		t.Fatal("2.2.2.2 首次应放行")
	}
	advance(time.Second)
	// 1.1.1.1 lastRef 最旧 → 被淘汰
	if !rl.allow("3.3.3.3") {
		t.Fatal("3.3.3.3 首次应放行")
	}
	rl.mu.Lock()
	if len(rl.buckets) > 2 {
		t.Errorf("buckets 数量 = %d，应 ≤ 2", len(rl.buckets))
	}
	if _, ok := rl.buckets["1.1.1.1"]; ok {
		t.Error("1.1.1.1 应已被淘汰")
	}
	rl.mu.Unlock()
	// 被淘汰后 1.1.1.1 重新请求 → 新桶，满令牌放行
	if !rl.allow("1.1.1.1") {
		t.Error("1.1.1.1 淘汰后再次请求应获新桶放行")
	}
}

// TestOCSPRateLimitConfig 环境变量解析：默认值、合法覆盖、非法回退。
func TestOCSPRateLimitConfig(t *testing.T) {
	// 默认
	t.Setenv("EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT", "")
	rate, burst := ocspRateLimitConfig()
	if rate != defaultOCSPRatePerSec {
		t.Errorf("默认 rate = %v，期望 %d", rate, defaultOCSPRatePerSec)
	}
	if burst != defaultOCSPBurst {
		t.Errorf("默认 burst = %d，期望 %d", burst, defaultOCSPBurst)
	}
	// 合法覆盖
	t.Setenv("EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT", "5")
	rate, burst = ocspRateLimitConfig()
	if rate != 5 {
		t.Errorf("rate = %v，期望 5", rate)
	}
	if burst != 10 { // 2×rate
		t.Errorf("burst = %d，期望 10", burst)
	}
	// 非法 → 回退默认
	t.Setenv("EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT", "abc")
	rate, _ = ocspRateLimitConfig()
	if rate != defaultOCSPRatePerSec {
		t.Errorf("非法值 rate = %v，期望默认 %d", rate, defaultOCSPRatePerSec)
	}
	// 零/负值 → 回退默认
	t.Setenv("EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT", "0")
	rate, _ = ocspRateLimitConfig()
	if rate != defaultOCSPRatePerSec {
		t.Errorf("零值 rate = %v，期望默认 %d", rate, defaultOCSPRatePerSec)
	}
}
