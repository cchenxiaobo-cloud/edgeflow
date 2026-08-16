// OCSP 在线吊销（WBS 7.1 吊销闭环，RFC 6960）。
//
// 本文件是 OCSP 的标准实现：encoding/asn1 手写 RFC 6960 全部结构
// （CertID / OCSPRequest / OCSPResponse / BasicOCSPResponse /
// SingleResponse / RevokedInfo），签名用 crypto/rsa 的
// sha256WithRSAEncryption（PKCS#1 v1.5），零第三方运行时依赖。
//
// 闭环：
//   - OCSPRequestForCert / ParseOCSPRequest：构造/解析查询请求；
//   - BuildOCSPResponse：CA 私钥签名生成响应（good/revoked/unknown），
//     状态源 = crl.json（LoadRevokedList 复用 loadRevokedList 私有函数），
//     与 CRL 吊销状态天然一致；
//   - ParseOCSPResponse：客户端校验（responseStatus、CA 验签、
//     responderID、CertID 匹配），返回 good/revoked/unknown + 吊销时间；
//   - OCSPStatus / OCSPStatusAt：客户端一键查询（默认本机 cloudcore /ocsp）。
//
// ASN.1 要点（RFC 6960 Appendix B 模块为 IMPLICIT TAGS）：
//   - responderID 用 byName [1] IMPLICIT Name：内容 = issuer DN 的
//     RDNSequence content（不含 SEQUENCE 头）；解析端兼容 EXPLICIT 形式；
//   - certStatus 是 CHOICE：good [0] IMPLICIT NULL、revoked [1] IMPLICIT
//     RevokedInfo、unknown [2] IMPLICIT UnknownInfo；
//   - 时间字段用 GeneralizedTime（asn1:"generalized"），nextUpdate 为
//     [0] EXPLICIT GeneralizedTime；
//   - version 字段 [0] EXPLICIT INTEGER DEFAULT v1（值为 0 时省略）。
package certs

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"
)

// OCSP 状态常量（ParseOCSPResponse / OCSPStatus 的返回值）。
const (
	// StatusGood 表示证书未吊销。
	StatusGood = "good"
	// StatusRevoked 表示证书已吊销（revokedAt 携带吊销时间）。
	StatusRevoked = "revoked"
	// StatusUnknown 表示 responder 无法确认状态（请求的 issuer 不是本 CA）。
	StatusUnknown = "unknown"

	// DefaultOCSPResponderURL 是 OCSPStatus 默认查询的 responder 地址
	// （本机 cloudcore 的 /ocsp 端点；HTTP 默认端口 8080 与 pkg/config 一致）。
	DefaultOCSPResponderURL = "http://127.0.0.1:8080/ocsp"

	// maxOCSPResponseBytes 是客户端接受的 OCSP 响应体上限（64 KiB，
	// 标准响应 <1 KiB，超限视为异常防拖垮）。
	maxOCSPResponseBytes = 64 << 10
	// ocspHTTPTimeout 是客户端 HTTP 查询超时。
	ocspHTTPTimeout = 5 * time.Second
)

// ErrMalformedOCSPRequest 表示请求 DER 无法解析为合法 OCSPRequest
// （responder 端点据此映射 HTTP 400）。
var ErrMalformedOCSPRequest = errors.New("malformed OCSP request")

// RFC 6960 涉及的 OID。
var (
	// oidSHA256 是 CertID.hashAlgorithm（id-sha256）。
	oidSHA256 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	// oidSHA256WithRSA 是签名算法（sha256WithRSAEncryption）。
	oidSHA256WithRSA = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	// oidOCSPBasic 是 responseBytes.responseType（id-pkix-ocsp-basic）。
	oidOCSPBasic = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 48, 1, 1}
)

// ---- RFC 6960 ASN.1 结构（encoding/asn1，手写，零第三方依赖）----

// certID 对应 RFC 6960 §4.1.1 CertID：
//
//	CertID ::= SEQUENCE {
//	    hashAlgorithm  AlgorithmIdentifier,
//	    issuerNameHash OCTET STRING,
//	    issuerKeyHash  OCTET STRING,
//	    serialNumber   CertificateSerialNumber }
type certID struct {
	HashAlgorithm  pkix.AlgorithmIdentifier
	IssuerNameHash []byte
	IssuerKeyHash  []byte
	SerialNumber   *big.Int
}

// request 对应 RFC 6960 §4.1.1 Request（singleRequestExtensions 可选，
// openssl 客户端的 nonce 扩展即落在这里）：
//
//	Request ::= SEQUENCE {
//	    reqCert  CertID,
//	    singleRequestExtensions [0] EXPLICIT Extensions OPTIONAL }
type request struct {
	CertID                 certID
	SingleRequestExtension asn1.RawValue `asn1:"explicit,tag:0,optional"`
}

// tbsRequest 对应 TBSRequest（version 默认 v1 省略；requestorName /
// requestExtensions 仅解析保留，本实现不发送）：
//
//	TBSRequest ::= SEQUENCE {
//	    version           [0] EXPLICIT Version DEFAULT v1,
//	    requestorName     [1] EXPLICIT GeneralName OPTIONAL,
//	    requestList       SEQUENCE OF Request,
//	    requestExtensions [2] EXPLICIT Extensions OPTIONAL }
type tbsRequest struct {
	Version       int           `asn1:"explicit,tag:0,default:0,optional"`
	RequestorName asn1.RawValue `asn1:"explicit,tag:1,optional"`
	RequestList   []request
	RequestExtras asn1.RawValue `asn1:"explicit,tag:2,optional"`
}

// ocspRequest 对应 OCSPRequest（optionalSignature 不发送，解析时若出现
// 会被判为尾部多余数据）：
//
//	OCSPRequest ::= SEQUENCE {
//	    tbsRequest        TBSRequest,
//	    optionalSignature [0] EXPLICIT Signature OPTIONAL }
type ocspRequest struct {
	TBSRequest tbsRequest
}

// revokedInfo 对应 RevokedInfo（revocationReason 不发送，仅解析保留）：
//
//	RevokedInfo ::= SEQUENCE {
//	    revocationTime   GeneralizedTime,
//	    revocationReason [0] EXPLICIT CRLReason OPTIONAL }
type revokedInfo struct {
	RevocationTime   time.Time `asn1:"generalized"`
	RevocationReason int       `asn1:"explicit,tag:0,optional"`
}

// singleResponse 对应 SingleResponse：
//
//	SingleResponse ::= SEQUENCE {
//	    certID            CertID,
//	    certStatus        CertStatus,        -- CHOICE，见 certStatusRaw*
//	    thisUpdate        GeneralizedTime,
//	    nextUpdate        [0] EXPLICIT GeneralizedTime OPTIONAL,
//	    singleExtensions  [1] EXPLICIT Extensions OPTIONAL }
type singleResponse struct {
	CertID           certID
	CertStatus       asn1.RawValue
	ThisUpdate       time.Time     `asn1:"generalized"`
	NextUpdate       time.Time     `asn1:"generalized,explicit,tag:0,optional"`
	SingleExtensions asn1.RawValue `asn1:"explicit,tag:1,optional"`
}

// responseData 对应 ResponseData（version 默认 v1 省略；
// responseExtensions 仅解析保留，本实现不发送 nonce）：
//
//	ResponseData ::= SEQUENCE {
//	    version            [0] EXPLICIT Version DEFAULT v1,
//	    responderID        ResponderID,     -- CHOICE：byName [1] / byKey [2]
//	    producedAt         GeneralizedTime,
//	    responses          SEQUENCE OF SingleResponse,
//	    responseExtensions [1] EXPLICIT Extensions OPTIONAL }
type responseData struct {
	Version            int              `asn1:"explicit,tag:0,default:0,optional"`
	ResponderID        asn1.RawValue
	ProducedAt         time.Time        `asn1:"generalized"`
	Responses          []singleResponse
	ResponseExtensions asn1.RawValue    `asn1:"explicit,tag:1,optional"`
}

// basicOCSPResponse 对应 BasicOCSPResponse。TBSResponseData 用 RawValue
// 保留原始 TLV：签名/验签直接作用于其 FullBytes，语义字段再从中解析：
//
//	BasicOCSPResponse ::= SEQUENCE {
//	    tbsResponseData      ResponseData,
//	    signatureAlgorithm   AlgorithmIdentifier,
//	    signature            BIT STRING,
//	    certs                [0] EXPLICIT SEQUENCE OF Certificate OPTIONAL }
type basicOCSPResponse struct {
	TBSResponseData    asn1.RawValue
	SignatureAlgorithm pkix.AlgorithmIdentifier
	Signature          asn1.BitString
	Certs              []asn1.RawValue `asn1:"explicit,tag:0,optional"`
}

// responseBytes 对应 ResponseBytes（BasicOCSPResponse 的 OCTET STRING 负载）。
type responseBytes struct {
	ResponseType asn1.ObjectIdentifier
	Response     []byte
}

// ocspResponse 对应 OCSPResponse：
//
//	OCSPResponse ::= SEQUENCE {
//	    responseStatus  OCSPResponseStatus,   -- ENUMERATED
//	    responseBytes   [0] EXPLICIT ResponseBytes OPTIONAL }
type ocspResponse struct {
	ResponseStatus asn1.Enumerated
	ResponseBytes  responseBytes `asn1:"explicit,tag:0,optional"`
}

// CertID 是 RFC 6960 CertID 的导出形态（构造请求 / 校验响应时传递）。
type CertID struct {
	// HashAlgorithm 是请求使用的哈希算法（本实现固定 sha256 + NULL 参数）。
	HashAlgorithm pkix.AlgorithmIdentifier
	// IssuerNameHash 是 issuer DN 的 SHA-256（SHA-256(issuer 的 DER Name)）。
	IssuerNameHash []byte
	// IssuerKeyHash 是 issuer 公钥 BIT STRING 内容（不含 tag/len/未用位数）的 SHA-256。
	IssuerKeyHash []byte
	// SerialNumber 是待查询证书的序列号。
	SerialNumber *big.Int
}

// RevokedCert 是吊销集合中的一条记录（与 crl.json 的条目同构；
// BuildOCSPResponse 据此判定 revoked 并回填吊销时间）。
type RevokedCert struct {
	// Serial 是证书序列号（小写十六进制，无 0x 前缀）。
	Serial string
	// Time 是吊销时间（RFC3339）。
	Time string
}

// OCSPResponseOptions 控制 BuildOCSPResponse 生成响应的时间字段。
type OCSPResponseOptions struct {
	// ThisUpdate 是状态已知为正确的最新时间；零值 → 取当前时间。
	ThisUpdate time.Time
	// NextUpdate 是状态刷新截止时间；零值 → ThisUpdate + 7 天（与 CRL 一致）。
	NextUpdate time.Time
}

// toInternal 把导出的 CertID 转为内部 ASN.1 结构。
func (c *CertID) toInternal() certID {
	return certID{
		HashAlgorithm:  c.HashAlgorithm,
		IssuerNameHash: c.IssuerNameHash,
		IssuerKeyHash:  c.IssuerKeyHash,
		SerialNumber:   c.SerialNumber,
	}
}

// toExported 把内部 ASN.1 结构转为导出的 CertID。
func (c *certID) toExported() *CertID {
	return &CertID{
		HashAlgorithm:  c.HashAlgorithm,
		IssuerNameHash: c.IssuerNameHash,
		IssuerKeyHash:  c.IssuerKeyHash,
		SerialNumber:   c.SerialNumber,
	}
}

// CertIDForCert 按 RFC 6960 §4.1.1 计算 CA 与叶子证书的 CertID：
// issuerNameHash = SHA-256(issuer 的 DER Name)，issuerKeyHash =
// SHA-256(issuer 公钥 BIT STRING 内容，不含 tag/长度/未用位数)。
func CertIDForCert(ca, leaf *x509.Certificate) (*CertID, error) {
	if ca == nil {
		return nil, errors.New("CertIDForCert: ca 为 nil")
	}
	if leaf == nil {
		return nil, errors.New("CertIDForCert: leaf 为 nil")
	}
	nameHash, keyHash, err := issuerHashes(ca)
	if err != nil {
		return nil, err
	}
	return &CertID{
		HashAlgorithm:  pkix.AlgorithmIdentifier{Algorithm: oidSHA256, Parameters: asn1.NullRawValue},
		IssuerNameHash: nameHash,
		IssuerKeyHash:  keyHash,
		SerialNumber:   new(big.Int).Set(leaf.SerialNumber),
	}, nil
}

// OCSPRequestForCert 构造 DER 编码的 OCSPRequest（RFC 6960 §4.1.1）：
// 单条 Request，CertID 用 sha256 哈希，不含 nonce 等扩展。
func OCSPRequestForCert(ca, leaf *x509.Certificate) ([]byte, error) {
	cid, err := CertIDForCert(ca, leaf)
	if err != nil {
		return nil, err
	}
	req := ocspRequest{
		TBSRequest: tbsRequest{
			RequestList: []request{{CertID: cid.toInternal()}},
		},
	}
	der, err := asn1.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("编码 OCSPRequest 失败: %w", err)
	}
	return der, nil
}

// ParseOCSPRequest 解析 DER 编码的 OCSPRequest，返回第一个 Request 的
// CertID（responder 用其判定状态）。请求含 requestorName / 扩展时宽松
// 接受；含 optionalSignature 或尾部多余数据时报错。
func ParseOCSPRequest(reqDER []byte) (*CertID, error) {
	var req ocspRequest
	if rest, err := asn1.Unmarshal(reqDER, &req); err != nil {
		return nil, fmt.Errorf("%w: 解析 OCSPRequest 失败: %v", ErrMalformedOCSPRequest, err)
	} else if len(rest) != 0 {
		return nil, fmt.Errorf("%w: OCSPRequest 存在尾部多余数据", ErrMalformedOCSPRequest)
	}
	if len(req.TBSRequest.RequestList) == 0 {
		return nil, fmt.Errorf("%w: OCSPRequest 不含任何 Request 项", ErrMalformedOCSPRequest)
	}
	return req.TBSRequest.RequestList[0].CertID.toExported(), nil
}

// BuildOCSPResponse 是 OCSP responder 的核心：解析请求 CertID →
// 判定状态（revoked 集合来自 crl.json，与 CRL 同源）→ CA 私钥对
// BasicOCSPResponse 做 sha256WithRSAEncryption 签名 → 返回 DER 编码的
// OCSPResponse（responseStatus = successful）。
//
// 状态语义（RFC 6960 §4.2.1 CertStatus）：
//   - 请求 CertID 的 issuer 哈希匹配本 CA：序列号在 revoked 集合 →
//     revoked（含吊销时间）；否则 → good；
//   - issuer 哈希不匹配本 CA → unknown（responder 不为其他 CA 背书）。
//
// 错误语义：请求 DER 非法 → 返回 *ErrMalformedOCSPRequest（responder
// 映射 HTTP 400）；吊销记录时间损坏 → 明确报错不静默（与 CRL 同约定）；
// 其余（编码/签名失败）为内部错误。
func BuildOCSPResponse(ca *CA, revoked []RevokedCert, reqDER []byte, opts OCSPResponseOptions) ([]byte, error) {
	if ca == nil || ca.Cert == nil || ca.Key == nil {
		return nil, errors.New("BuildOCSPResponse: CA 未初始化（Cert/Key 为 nil）")
	}
	reqCertID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		return nil, err
	}

	// 时间字段：零值回退（ThisUpdate→now，NextUpdate→+7 天与 CRL 一致）。
	now := time.Now().UTC()
	thisUpdate := opts.ThisUpdate
	if thisUpdate.IsZero() {
		thisUpdate = now
	}
	nextUpdate := opts.NextUpdate
	if nextUpdate.IsZero() {
		nextUpdate = thisUpdate.Add(crlValidity)
	}

	// 状态判定：issuer 匹配本 CA 才查吊销集合，否则 unknown。
	status := StatusUnknown
	var revokedAt *time.Time
	if issuerHashesMatch(ca.Cert, reqCertID) {
		status = StatusGood
		t, err := findRevokedTime(revoked, reqCertID.SerialNumber)
		if err != nil {
			return nil, err
		}
		if t != nil {
			status = StatusRevoked
			revokedAt = t
		}
	}

	// SingleResponse：certStatus 按状态构造 CHOICE 编码。
	sr := singleResponse{
		CertID:     reqCertID.toInternal(),
		ThisUpdate: thisUpdate,
		NextUpdate: nextUpdate,
	}
	switch status {
	case StatusGood:
		sr.CertStatus = certStatusGood()
	case StatusRevoked:
		raw, err := certStatusRevoked(*revokedAt)
		if err != nil {
			return nil, err
		}
		sr.CertStatus = raw
	default: // StatusUnknown
		sr.CertStatus = certStatusUnknown()
	}

	// responderID：byName [1] IMPLICIT Name，内容 = CA Subject 的
	// RDNSequence content（RFC 6960 模块为 IMPLICIT TAGS）。
	var name asn1.RawValue
	if _, err := asn1.Unmarshal(ca.Cert.RawSubject, &name); err != nil {
		return nil, fmt.Errorf("解析 CA Subject 失败: %w", err)
	}
	rd := responseData{
		ResponderID: asn1.RawValue{
			Class:      asn1.ClassContextSpecific,
			Tag:        1,
			IsCompound: true,
			Bytes:      name.Bytes,
		},
		ProducedAt: thisUpdate,
		Responses:  []singleResponse{sr},
	}
	tbsDER, err := asn1.Marshal(rd)
	if err != nil {
		return nil, fmt.Errorf("编码 ResponseData 失败: %w", err)
	}

	// 签名：sha256WithRSAEncryption（PKCS#1 v1.5），覆盖 ResponseData TLV。
	digest := sha256.Sum256(tbsDER)
	sig, err := rsa.SignPKCS1v15(rand.Reader, ca.Key, crypto.SHA256, digest[:])
	if err != nil {
		return nil, fmt.Errorf("OCSP 响应签名失败: %w", err)
	}
	basic := basicOCSPResponse{
		TBSResponseData:    asn1.RawValue{FullBytes: tbsDER},
		SignatureAlgorithm: pkix.AlgorithmIdentifier{Algorithm: oidSHA256WithRSA, Parameters: asn1.NullRawValue},
		Signature:          asn1.BitString{Bytes: sig, BitLength: len(sig) * 8},
	}
	basicDER, err := asn1.Marshal(basic)
	if err != nil {
		return nil, fmt.Errorf("编码 BasicOCSPResponse 失败: %w", err)
	}
	der, err := asn1.Marshal(ocspResponse{
		ResponseStatus: asn1.Enumerated(0), // successful
		ResponseBytes: responseBytes{
			ResponseType: oidOCSPBasic,
			Response:     basicDER,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("编码 OCSPResponse 失败: %w", err)
	}
	return der, nil
}

// ParseOCSPResponse 是客户端校验入口：解析并验证 OCSP 响应，返回证书
// 状态（StatusGood / StatusRevoked / StatusUnknown）与吊销时间（仅
// revoked 时非 nil）。
//
// 验证链（防伪造，任一环节失败即报错）：
//  1. responseStatus 必须为 successful(0)；
//  2. responseBytes 类型必须为 id-pkix-ocsp-basic；
//  3. BasicOCSPResponse 签名算法必须为 sha256WithRSAEncryption，
//     并用 ca 的公钥验签（响应必须由可信 CA 签发）；
//  4. responderID（byName/byKey）必须匹配 ca（防 CA 混淆）；
//  5. 响应中的 SingleResponse.CertID 必须与 reqCertID 一致（防串响应）。
//
// producedAt / nextUpdate 的新鲜度由调用方按需校验（本函数不强制）。
func ParseOCSPResponse(ca *x509.Certificate, respDER []byte, reqCertID *CertID) (string, *time.Time, error) {
	if ca == nil {
		return "", nil, errors.New("ParseOCSPResponse: ca 为 nil")
	}
	if reqCertID == nil {
		return "", nil, errors.New("ParseOCSPResponse: reqCertID 为 nil")
	}
	var resp ocspResponse
	if rest, err := asn1.Unmarshal(respDER, &resp); err != nil {
		return "", nil, fmt.Errorf("解析 OCSPResponse 失败: %w", err)
	} else if len(rest) != 0 {
		return "", nil, errors.New("OCSPResponse 存在尾部多余数据")
	}
	if resp.ResponseStatus != 0 {
		return "", nil, fmt.Errorf("OCSP 响应状态非 successful: %s",
			ocspStatusName(int(resp.ResponseStatus)))
	}
	if !resp.ResponseBytes.ResponseType.Equal(oidOCSPBasic) {
		return "", nil, fmt.Errorf("不支持的 OCSP 响应类型 %v（仅支持 id-pkix-ocsp-basic）",
			resp.ResponseBytes.ResponseType)
	}
	var basic basicOCSPResponse
	if rest, err := asn1.Unmarshal(resp.ResponseBytes.Response, &basic); err != nil {
		return "", nil, fmt.Errorf("解析 BasicOCSPResponse 失败: %w", err)
	} else if len(rest) != 0 {
		return "", nil, errors.New("BasicOCSPResponse 存在尾部多余数据")
	}
	if !basic.SignatureAlgorithm.Algorithm.Equal(oidSHA256WithRSA) {
		return "", nil, fmt.Errorf("OCSP 响应签名算法 %v 不受支持（仅支持 sha256WithRSAEncryption）",
			basic.SignatureAlgorithm.Algorithm)
	}
	// 验签（防伪造/篡改）：签名覆盖 ResponseData 的完整 TLV。
	digest := sha256.Sum256(basic.TBSResponseData.FullBytes)
	pub, ok := ca.PublicKey.(*rsa.PublicKey)
	if !ok {
		return "", nil, errors.New("CA 公钥不是 RSA 类型，无法验签 OCSP 响应")
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], basic.Signature.Bytes); err != nil {
		return "", nil, fmt.Errorf("OCSP 响应签名验证失败（响应被篡改或非本 CA 签发）: %w", err)
	}

	// 解析 ResponseData 语义字段 + responderID 一致性校验。
	var rd responseData
	if _, err := asn1.Unmarshal(basic.TBSResponseData.FullBytes, &rd); err != nil {
		return "", nil, fmt.Errorf("解析 ResponseData 失败: %w", err)
	}
	if err := verifyResponderID(rd.ResponderID, ca); err != nil {
		return "", nil, err
	}

	// 找与请求 CertID 匹配的 SingleResponse（RFC 允许单响应含多条）。
	for i := range rd.Responses {
		sr := &rd.Responses[i]
		if !certIDEqual(&sr.CertID, reqCertID) {
			continue
		}
		switch sr.CertStatus.Tag {
		case 0: // good [0] IMPLICIT NULL
			return StatusGood, nil, nil
		case 1: // revoked [1] IMPLICIT RevokedInfo
			content, err := unwrapSequenceContent(sr.CertStatus.Bytes)
			if err != nil {
				return "", nil, fmt.Errorf("解析 RevokedInfo 失败: %w", err)
			}
			seq, err := asn1.Marshal(asn1.RawValue{
				Class:      asn1.ClassUniversal,
				Tag:        asn1.TagSequence,
				IsCompound: true,
				Bytes:      content,
			})
			if err != nil {
				return "", nil, fmt.Errorf("解析 RevokedInfo 失败: %w", err)
			}
			var ri revokedInfo
			if _, err := asn1.Unmarshal(seq, &ri); err != nil {
				return "", nil, fmt.Errorf("解析 RevokedInfo 失败: %w", err)
			}
			t := ri.RevocationTime
			return StatusRevoked, &t, nil
		case 2: // unknown [2] IMPLICIT UnknownInfo
			return StatusUnknown, nil, nil
		default:
			return "", nil, fmt.Errorf("未知 CertStatus 标签 %d", sr.CertStatus.Tag)
		}
	}
	return "", nil, errors.New("OCSP 响应中不存在与请求 CertID 匹配的 SingleResponse")
}

// LoadCA 严格加载 CA（证书 + 私钥均须存在且配对；不自动创建）。
// 供 cloudcore /ocsp 端点等外部调用方使用（内部 loadCA 的导出包装）。
func LoadCA(certDir string) (*CA, error) {
	return loadCA(certDir)
}

// LoadRevokedList 加载吊销集合（crl.json 来源记录；缺失时从 crl.pem
// 恢复，与 RevokeCert 同路径）。供 OCSP responder 使用，保证 OCSP 与
// CRL 吊销状态同源一致。
func LoadRevokedList(certDir string) ([]RevokedCert, error) {
	list, err := loadRevokedList(certDir)
	if err != nil {
		return nil, err
	}
	out := make([]RevokedCert, 0, len(list.Entries))
	for _, e := range list.Entries {
		out = append(out, RevokedCert{Serial: e.Serial, Time: e.Time})
	}
	return out, nil
}

// OCSPStatus 查询 leaf 证书的吊销状态（OCSP，WBS 7.1）：
// 构造 OCSPRequest → POST 本机 cloudcore /ocsp → 校验响应 → 返回状态。
// responder 地址用默认值（DefaultOCSPResponderURL），需要自定义时用
// OCSPStatusAt。
func OCSPStatus(certDir string, leaf *x509.Certificate) (string, *time.Time, error) {
	return OCSPStatusAt(certDir, leaf, DefaultOCSPResponderURL)
}

// OCSPStatusAt 同 OCSPStatus，但 responder 地址可配置（测试/跨主机部署）。
// 校验语义与 ParseOCSPResponse 一致：响应必须由 certDir 中的 CA 签名、
// CertID 必须匹配，否则返回错误（防伪造/串响应）。
func OCSPStatusAt(certDir string, leaf *x509.Certificate, responderURL string) (string, *time.Time, error) {
	if leaf == nil {
		return "", nil, errors.New("OCSPStatusAt: leaf 为 nil")
	}
	if responderURL == "" {
		return "", nil, errors.New("OCSPStatusAt: responderURL 为空")
	}
	ca, err := loadCA(certDir)
	if err != nil {
		return "", nil, err
	}
	cid, err := CertIDForCert(ca.Cert, leaf)
	if err != nil {
		return "", nil, err
	}
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		return "", nil, err
	}
	client := &http.Client{Timeout: ocspHTTPTimeout}
	resp, err := client.Post(responderURL, "application/ocsp-request", bytes.NewReader(reqDER))
	if err != nil {
		return "", nil, fmt.Errorf("OCSP 查询 %s 失败: %w", responderURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOCSPResponseBytes+1))
	if err != nil {
		return "", nil, fmt.Errorf("读取 OCSP 响应失败: %w", err)
	}
	if len(body) > maxOCSPResponseBytes {
		return "", nil, fmt.Errorf("OCSP 响应过大（超过 %d 字节）", maxOCSPResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("OCSP responder 返回 HTTP %d", resp.StatusCode)
	}
	return ParseOCSPResponse(ca.Cert, body, cid)
}

// ---- 内部辅助 ----

// issuerHashes 计算 RFC 6960 §4.1.1 的 issuerNameHash / issuerKeyHash：
//
//	issuerNameHash = SHA-256(issuer 的 DER Name)
//	issuerKeyHash  = SHA-256(issuer 公钥 BIT STRING 内容)
//
// 公钥 BIT STRING 内容不含 tag/长度/未用位数 octet（即 Go
// asn1.BitString.Bytes 的语义，与 RFC 2560 §2.1.1 一致）。
func issuerHashes(ca *x509.Certificate) (nameHash, keyHash []byte, err error) {
	nameHashSum := sha256.Sum256(ca.RawSubject)
	keyBits, err := caPublicKeyBits(ca)
	if err != nil {
		return nil, nil, err
	}
	keyHashSum := sha256.Sum256(keyBits)
	return nameHashSum[:], keyHashSum[:], nil
}

// caPublicKeyBits 提取 CA 公钥 BIT STRING 的内容字节（不含 tag/长度/
// 未用位数 octet）。
func caPublicKeyBits(ca *x509.Certificate) ([]byte, error) {
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if rest, err := asn1.Unmarshal(ca.RawSubjectPublicKeyInfo, &spki); err != nil {
		return nil, fmt.Errorf("解析 CA SubjectPublicKeyInfo 失败: %w", err)
	} else if len(rest) != 0 {
		return nil, errors.New("解析 CA SubjectPublicKeyInfo 失败: 存在尾部多余数据")
	}
	return spki.PublicKey.Bytes, nil
}

// issuerHashesMatch 判断请求 CertID 的 issuer 哈希是否匹配本 CA
// （hashAlgorithm 必须为 sha256）。
func issuerHashesMatch(ca *x509.Certificate, cid *CertID) bool {
	if !cid.HashAlgorithm.Algorithm.Equal(oidSHA256) {
		return false
	}
	nameHash, keyHash, err := issuerHashes(ca)
	if err != nil {
		return false
	}
	return bytes.Equal(cid.IssuerNameHash, nameHash) && bytes.Equal(cid.IssuerKeyHash, keyHash)
}

// findRevokedTime 在吊销集合中查找序列号，返回吊销时间。
// 返回 (nil, nil) 表示未吊销；时间字段损坏返回错误（fail-closed，
// 与 CRL 生成路径对吊销时间的校验一致）。序列号按数值比较，
// 兼容大小写/前导零差异。
func findRevokedTime(revoked []RevokedCert, serial *big.Int) (*time.Time, error) {
	if serial == nil {
		return nil, errors.New("findRevokedTime: 序列号为 nil")
	}
	for _, e := range revoked {
		n, ok := new(big.Int).SetString(e.Serial, 16)
		if !ok || n.Sign() <= 0 {
			continue // 非法条目：LoadRevokedList 已拦截，防御性跳过
		}
		if n.Cmp(serial) != 0 {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.Time)
		if err != nil {
			return nil, fmt.Errorf("吊销记录序列号 %s 的吊销时间 %q 非法: %w", e.Serial, e.Time, err)
		}
		return &t, nil
	}
	return nil, nil
}

// certIDEqual 比较响应中的 CertID 与请求 CertID 是否一致。
func certIDEqual(a *certID, b *CertID) bool {
	return a.HashAlgorithm.Algorithm.Equal(b.HashAlgorithm.Algorithm) &&
		bytes.Equal(a.IssuerNameHash, b.IssuerNameHash) &&
		bytes.Equal(a.IssuerKeyHash, b.IssuerKeyHash) &&
		a.SerialNumber != nil && b.SerialNumber != nil &&
		a.SerialNumber.Cmp(b.SerialNumber) == 0
}

// certStatusGood 构造 CertStatus 的 good 编码（[0] IMPLICIT NULL）。
func certStatusGood() asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0}
}

// certStatusUnknown 构造 CertStatus 的 unknown 编码（[2] IMPLICIT NULL）。
func certStatusUnknown() asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 2}
}

// certStatusRevoked 构造 CertStatus 的 revoked 编码（[1] IMPLICIT
// RevokedInfo；内容 = RevokedInfo SEQUENCE 的 content）。
func certStatusRevoked(t time.Time) (asn1.RawValue, error) {
	der, err := asn1.Marshal(revokedInfo{RevocationTime: t})
	if err != nil {
		return asn1.RawValue{}, fmt.Errorf("编码 RevokedInfo 失败: %w", err)
	}
	var seq asn1.RawValue
	if _, err := asn1.Unmarshal(der, &seq); err != nil {
		return asn1.RawValue{}, fmt.Errorf("解析 RevokedInfo 失败: %w", err)
	}
	return asn1.RawValue{
		Class:      asn1.ClassContextSpecific,
		Tag:        1,
		IsCompound: true,
		Bytes:      seq.Bytes,
	}, nil
}

// unwrapSequenceContent 把 context-specific 包装里的 SEQUENCE 内容
// 归一化为 content octets：显式形式（内容 = SEQUENCE TLV）剥掉 SEQUENCE
// 头；隐式形式（内容 = SEQUENCE content）原样返回。解析端兼容两种形式。
func unwrapSequenceContent(b []byte) ([]byte, error) {
	var rv asn1.RawValue
	if _, err := asn1.Unmarshal(b, &rv); err == nil && rv.Tag == asn1.TagSequence && rv.IsCompound {
		return rv.Bytes, nil
	}
	return b, nil
}

// verifyResponderID 校验响应的 responderID 与 CA 一致（防 CA 混淆）：
//   - byName [1]：Name 与 CA Subject 的 DER 一致（兼容 IMPLICIT/EXPLICIT）；
//   - byKey [2]：KeyHash 与 SHA-1(CA 公钥 BIT STRING 内容) 一致
//     （RFC 6960 §4.2.1：responder 公钥的哈希）。
func verifyResponderID(rid asn1.RawValue, ca *x509.Certificate) error {
	if rid.Class != asn1.ClassContextSpecific {
		return errors.New("responderID 不是 context-specific 标签")
	}
	switch rid.Tag {
	case 1: // byName
		content, err := unwrapSequenceContent(rid.Bytes)
		if err != nil {
			return fmt.Errorf("解析 responderID byName 失败: %w", err)
		}
		subjectContent, err := unwrapSequenceContent(ca.RawSubject)
		if err != nil {
			return fmt.Errorf("解析 CA Subject 失败: %w", err)
		}
		if !bytes.Equal(content, subjectContent) {
			return errors.New("responderID byName 与本 CA 的 Subject 不一致（响应由其他 CA 签发？）")
		}
		return nil
	case 2: // byKey
		keyHash := rid.Bytes
		var rv asn1.RawValue
		if _, err := asn1.Unmarshal(keyHash, &rv); err == nil && rv.Tag == asn1.TagOctetString {
			keyHash = rv.Bytes // 兼容 EXPLICIT OCTET STRING 形式
		}
		keyBits, err := caPublicKeyBits(ca)
		if err != nil {
			return err
		}
		sum := sha1.Sum(keyBits)
		if !bytes.Equal(keyHash, sum[:]) {
			return errors.New("responderID byKey 与本 CA 公钥不一致（响应由其他 CA 签发？）")
		}
		return nil
	default:
		return fmt.Errorf("未知 responderID 标签 %d", rid.Tag)
	}
}

// ocspStatusName 把 OCSPResponseStatus 枚举映射为 RFC 6960 名称（错误信息用）。
func ocspStatusName(s int) string {
	switch s {
	case 0:
		return "successful"
	case 1:
		return "malformedRequest"
	case 2:
		return "internalError"
	case 3:
		return "tryLater"
	case 5:
		return "sigRequired"
	case 6:
		return "unauthorized"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}
