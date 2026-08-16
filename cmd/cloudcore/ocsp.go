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
package main

import (
	"errors"
	"io"
	"net/http"

	"edgeflow/pkg/certs"
)

// maxOCSPRequestBytes 是 OCSP 请求体大小上限（16 KiB，标准 CertID 请求
// <200 字节；超限直接 400，防止超大请求体拖垮 ASN.1 解析）。
const maxOCSPRequestBytes = 16 << 10

// ocspHandler 是 POST /ocsp 的处理器：解析请求 → 判定吊销状态 →
// CA 签名响应。certDir 与 mTLS 证书目录同一配置路径。
type ocspHandler struct {
	certDir string
}

// ServeHTTP 处理一次 OCSP 查询。
// 响应语义：200 + DER OCSPResponse；400 = 请求非法（DER 损坏/超限/为空）；
// 500 = responder 内部错误（CA 不可用/吊销记录损坏）。
func (h *ocspHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Content-Type", "application/ocsp-response")
	w.WriteHeader(http.StatusOK)
	// 写失败（客户端断开）无需额外处理，忽略即可
	_, _ = w.Write(respDER)
}
