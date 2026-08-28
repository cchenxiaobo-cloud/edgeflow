// cloudcore /ocsp 端点：标准 OCSP responder（RFC 6960，WBS 7.1 吊销闭环）。
//
// 协议：
//   - 方法 POST，请求体 = DER 编码的 OCSPRequest（Content-Type:
//     application/ocsp-request），响应体 = DER 编码的 OCSPResponse
//     （Content-Type: application/ocsp-response）；
//   - 请求体上限 maxOCSPRequestBytes（16 KiB），超限/非法 DER → 400；
//   - responder 数据源 = 证书目录（EDGEFLOW_CLOUDCORE_CERT_DIR 或默认
//     data/certs，与 mTLS 同一目录）：CA 用于签发响应（LoadCA），吊销
//     状态直接读 crl.json（certs.LoadRevokedList），与 CRL 保持同源一致；
//   - CA 缺失/吊销记录损坏 → 500（fail-closed，不静默放行）。
//
// 认证说明：/ocsp 不挂 API Token 认证中间件（与 /api/v1/* 不同）。
// 它是协议端点而非管理端点：响应自带 CA 签名（客户端验签防伪造），
// 边缘节点查询时不持有管理令牌；管理 API 的认证/审计边界不受影响。
//
// 防滥用说明（M1 加固）：/ocsp 免认证 + RSA 签名响应 = 可被放大的
// CPU 消耗面。两道控制：
//   - per-IP 令牌桶限流（默认 10 req/s、burst 20，可用环境变量
//     EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT 调整每秒速率），超限 → 429；
//     限流器 map 有界（maxOCSPRateTrackedIPs），超限淘汰最旧条目，
//     防海量伪造源 IP 撑爆内存；
//   - 成功响应带 Cache-Control: max-age=3600（响应 nextUpdate ≈ 7 天），
//     允许客户端/中间层缓存复用，降低重复查询压力。
package main

import (
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"edgeflow/pkg/certs"
	"edgeflow/pkg/log"
)

// maxOCSPRequestBytes 是 OCSP 请求体大小上限（16 KiB，标准 CertID 请求
// <200 字节；超限直接 400，防止超大请求体拖垮 ASN.1 解析）。
const maxOCSPRequestBytes = 16 << 10

// /ocsp 限流与缓存参数（M1：签名放大 DoS 面控制）。
const (
	// defaultOCSPRatePerSec 是默认限流速率（每 IP 每秒请求数）。
	defaultOCSPRatePerSec = 10
	// defaultOCSPBurst 是默认突发容量（令牌桶初始/上限令牌数）。
	defaultOCSPBurst = 20
	// maxOCSPRateTrackedIPs 是限流器跟踪的 IP 数量上限（map 有界，
	// 超限淘汰最旧条目，防海量伪造源 IP 耗尽内存）。
	maxOCSPRateTrackedIPs = 10000
	// ocspCacheMaxAge 是成功响应的 Cache-Control: max-age 秒数。
	// 响应 nextUpdate ≈ 7 天（certs.crlValidity），1 小时缓存对
	// good/revoked/unknown 均安全：吊销不可逆（缓存的 revoked 仍有效），
	// good/unknown 过期后由客户端按 nextUpdate 新鲜度校验拒绝（见
	// pkg/certs OCSPStatusAtWithPolicy）。
	ocspCacheMaxAge = 3600
)

// ocspHandler 是 POST /ocsp 的处理器：解析请求 → 判定吊销状态 →
// CA 签名响应。certDir 与 mTLS 证书目录同一配置路径。
type ocspHandler struct {
	certDir string

	// limiter 是 per-IP 限流器；nil 时按环境变量/默认参数惰性初始化
	// （保留 &ocspHandler{certDir: ...} 的零值构造兼容）。
	limiterMu sync.Mutex
	limiter   *ocspRateLimiter
}

// limiterForRequest 返回当前生效的限流器（惰性初始化，并发安全）。
func (h *ocspHandler) limiterForRequest() *ocspRateLimiter {
	h.limiterMu.Lock()
	defer h.limiterMu.Unlock()
	if h.limiter == nil {
		rate, burst := ocspRateLimitConfig()
		h.limiter = newOCSPRateLimiter(rate, burst, maxOCSPRateTrackedIPs)
	}
	return h.limiter
}

// ServeHTTP 处理一次 OCSP 查询。
// 响应语义：200 + DER OCSPResponse；400 = 请求非法（DER 损坏/超限/为空）；
// 429 = 该 IP 限流（令牌耗尽）；500 = responder 内部错误（CA 不可用/
// 吊销记录损坏）。
func (h *ocspHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 限流最先执行：廉价拒绝超限请求，避免解析/签名成本被放大。
	if !h.limiterForRequest().allow(clientIP(r)) {
		http.Error(w, "ocsp rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	// 请求体大小限制（超限 http.MaxBytesReader 报 *http.MaxBytesError）。
	r.Body = http.MaxBytesReader(w, r.Body, maxOCSPRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "ocsp request too large", http.StatusBadRequest)
			return
		}
		http.Error(w, "read ocsp request failed", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty ocsp request", http.StatusBadRequest)
		return
	}
	// 先解析请求：非法 DER 无论 responder 状态如何都是 400。
	if _, err := certs.ParseOCSPRequest(body); err != nil {
		http.Error(w, "malformed ocsp request", http.StatusBadRequest)
		return
	}

	ca, err := certs.LoadCA(h.certDir)
	if err != nil {
		http.Error(w, "ocsp responder: CA unavailable", http.StatusInternalServerError)
		return
	}
	revoked, err := certs.LoadRevokedList(h.certDir)
	if err != nil {
		http.Error(w, "ocsp responder: revoked list unavailable", http.StatusInternalServerError)
		return
	}
	respDER, err := certs.BuildOCSPResponse(ca, revoked, body, certs.OCSPResponseOptions{})
	if err != nil {
		// 请求已在上面通过 ParseOCSPRequest，走到这里只剩内部错误
		// （签名失败等）：500，不向客户端泄露细节。
		http.Error(w, "ocsp responder: build response failed", http.StatusInternalServerError)
		return
	}
	// 成功响应：带缓存头（见 ocspCacheMaxAge 注释），降低重复查询压力。
	w.Header().Set("Cache-Control", "max-age="+strconv.Itoa(ocspCacheMaxAge))
	w.Header().Set("Content-Type", "application/ocsp-response")
	w.WriteHeader(http.StatusOK)
	// 写失败（客户端断开）无需额外处理，忽略即可
	_, _ = w.Write(respDER)
}

// ---- 限流器（纯标准库 per-IP 令牌桶，map 有界）----

// ocspRateLimiter 是 per-IP 令牌桶限流器（纯标准库实现，零第三方依赖）。
// map 有界：跟踪的 IP 条目超过 maxIPs 时淘汰最久未访问的条目，
// 防止攻击者用海量伪造源 IP 耗尽内存。
type ocspRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ocspBucket
	rate    float64          // 每秒补充令牌数
	burst   int              // 桶容量（初始令牌数 = 突发上限）
	maxIPs  int              // map 条目上限
	now     func() time.Time // 时钟注入点（nil = time.Now；测试用）
}

// ocspBucket 是单个 IP 的令牌桶（惰性补充：按经过时间补令牌）。
type ocspBucket struct {
	tokens  float64
	lastRef time.Time // 最近一次访问时间（令牌补充基准 + 淘汰依据）
}

// newOCSPRateLimiter 构造限流器；参数非法时回退默认值（防御性）。
func newOCSPRateLimiter(rate float64, burst, maxIPs int) *ocspRateLimiter {
	if rate <= 0 {
		rate = defaultOCSPRatePerSec
	}
	if burst < 1 {
		burst = defaultOCSPBurst
	}
	if maxIPs < 1 {
		maxIPs = maxOCSPRateTrackedIPs
	}
	return &ocspRateLimiter{
		buckets: make(map[string]*ocspBucket),
		rate:    rate,
		burst:   burst,
		maxIPs:  maxIPs,
	}
}

// nowTime 返回当前时间（支持测试注入）。
func (rl *ocspRateLimiter) nowTime() time.Time {
	if rl.now != nil {
		return rl.now()
	}
	return time.Now()
}

// allow 判断来自 ip 的一次请求是否放行（放行消耗 1 个令牌）。
// 新 IP 的桶以满令牌（burst）开始；令牌不足 → false（调用方回 429）。
// 内部维护 map 有界性：条目超限时淘汰 lastRef 最旧的条目。
func (rl *ocspRateLimiter) allow(ip string) bool {
	now := rl.nowTime()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[ip]
	if !ok {
		if len(rl.buckets) >= rl.maxIPs {
			rl.evictOldestLocked()
		}
		b = &ocspBucket{tokens: float64(rl.burst), lastRef: now}
		rl.buckets[ip] = b
	}
	if elapsed := now.Sub(b.lastRef).Seconds(); elapsed > 0 {
		b.tokens += elapsed * rl.rate
		if b.tokens > float64(rl.burst) {
			b.tokens = float64(rl.burst)
		}
	}
	b.lastRef = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictOldestLocked 淘汰 lastRef 最早的 IP 条目（锁内调用，map 已满时腾位）。
func (rl *ocspRateLimiter) evictOldestLocked() {
	var oldestIP string
	var oldestRef time.Time
	for ip, b := range rl.buckets {
		if oldestIP == "" || b.lastRef.Before(oldestRef) {
			oldestIP, oldestRef = ip, b.lastRef
		}
	}
	if oldestIP != "" {
		delete(rl.buckets, oldestIP)
	}
}

// clientIP 提取请求来源 IP（剥离端口；无端口或解析失败时原样返回）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ocspRateLimitConfig 从环境变量读取限流参数：
// EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT = 每 IP 每秒请求数（默认 10）；
// burst 取 2×速率（下限 1）；非法值回退默认。零第三方依赖，
// 便于运维按部署规模调整。
// EnvCRLStrict / EnvOCSPFresh 是 v0.22.0 吊销策略收紧 env（SEC-04）：
// EDGEFLOW_CLOUDCORE_CRL_STRICT=on → CRL 产物缺失（含证书目录缺失）时
// mTLS 握手拒绝（fail-closed；默认 off 放行，向后兼容）；
// EDGEFLOW_CLOUDCORE_OCSP_FRESH=on → OCSP 客户端查询启用新鲜度校验
// （nextUpdate 过期拒绝；默认 off，见 pkg/certs FreshnessPolicyFromEnv）。
const (
	EnvCRLStrict = "EDGEFLOW_CLOUDCORE_CRL_STRICT"
	// EnvOCSPFresh 复用 pkg/certs.EnvOCSPFreshCheck（同名常量再声明，
	// 避免 main 包直接 import 常量名过长）。
	EnvOCSPFresh = "EDGEFLOW_CLOUDCORE_OCSP_FRESH"
)

// revocationOptionsFromEnv 读取吊销收紧开关（v0.22.0 SEC-04 装配辅助，
// 默认全关 = v0.21.0 缺省行为逐字节一致）。主线装配点（main.go 禁触，
// 由主线落地，一行接线）：
//
//	tlsOpts := revocationOptionsFromEnv()
//	hubTLS, err = certs.LoadTLSConfigWithOptions(certDir, true, tlsOpts)
//	edgeTLS, err = certs.LoadTLSConfigWithOptions(certDir, false, tlsOpts)
//
// OCSP 客户端侧（keadm/运维查询）经 certs.FreshnessPolicyFromEnv 独立生效。
// 返回值同时供启动日志登记当前收紧面（可见性，SEC-04 部署建议见 SECURITY.md）。
func revocationOptionsFromEnv() certs.RevocationOptions {
	opts := certs.RevocationOptions{
		CRLStrict:      os.Getenv(EnvCRLStrict) == "on",
		OCSPFreshCheck: os.Getenv(EnvOCSPFresh) == "on",
	}
	if opts.CRLStrict {
		log.Warnf("[security] %s=on：CRL 产物缺失时 mTLS 握手将拒绝（fail-closed）；请确保吊销链已启用（keadm cert revoke 会生成 crl.pem）", EnvCRLStrict)
	}
	if opts.OCSPFreshCheck {
		log.Infof("[security] %s=on：OCSP 客户端查询启用新鲜度校验（过期响应拒绝）", EnvOCSPFresh)
	}
	return opts
}

func ocspRateLimitConfig() (rate float64, burst int) {
	rate = defaultOCSPRatePerSec
	if v := os.Getenv("EDGEFLOW_CLOUDCORE_OCSP_RATE_LIMIT"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			rate = n
		}
	}
	burst = int(math.Ceil(rate * 2))
	if burst < 1 {
		burst = 1
	}
	return rate, burst
}
