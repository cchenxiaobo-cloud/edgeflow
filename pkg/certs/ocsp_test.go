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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mustOCSPEnv 生成临时证书目录：CA + 客户端叶子证书，返回 CA 句柄与叶子证书。
func mustOCSPEnv(t *testing.T, certDir, cn string) (*CA, *x509.Certificate) {
	t.Helper()
	ca, err := EnsureCA(certDir)
	if err != nil {
		t.Fatalf("EnsureCA 失败: %v", err)
	}
	leafTLS, err := EnsureClientCert(certDir, cn)
	if err != nil {
		t.Fatalf("EnsureClientCert 失败: %v", err)
	}
	leaf, err := x509.ParseCertificate(leafTLS.Certificate[0])
	if err != nil {
		t.Fatalf("解析叶子证书失败: %v", err)
	}
	return ca, leaf
}

// TestOCSPRequest_RoundTrip 验证 OCSPRequest 构造→解析往返：CertID 的
// 哈希算法/issuerNameHash/issuerKeyHash/序列号与 RFC 6960 定义一致。
func TestOCSPRequest_RoundTrip(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")

	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("OCSPRequestForCert 失败: %v", err)
	}
	cid, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	if !cid.HashAlgorithm.Algorithm.Equal(oidSHA256) {
		t.Errorf("hashAlgorithm = %v，期望 sha256", cid.HashAlgorithm.Algorithm)
	}
	nameHash := sha256.Sum256(ca.Cert.RawSubject)
	if !bytes.Equal(cid.IssuerNameHash, nameHash[:]) {
		t.Errorf("issuerNameHash 与 SHA-256(CA RawSubject) 不一致")
	}
	keyBits, err := caPublicKeyBits(ca.Cert)
	if err != nil {
		t.Fatalf("caPublicKeyBits 失败: %v", err)
	}
	keyHash := sha256.Sum256(keyBits)
	if !bytes.Equal(cid.IssuerKeyHash, keyHash[:]) {
		t.Errorf("issuerKeyHash 与 SHA-256(CA 公钥 BIT STRING 内容) 不一致")
	}
	if cid.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Errorf("序列号 = %v，期望 %v", cid.SerialNumber, leaf.SerialNumber)
	}
	// 请求必须能被标准解析器接受：单条 Request，无扩展
	var req ocspRequest
	if rest, err := asn1.Unmarshal(reqDER, &req); err != nil {
		t.Fatalf("asn1 解析请求失败: %v", err)
	} else if len(rest) != 0 {
		t.Fatalf("请求存在尾部数据")
	}
	if len(req.TBSRequest.RequestList) != 1 {
		t.Fatalf("RequestList 长度 = %d，期望 1", len(req.TBSRequest.RequestList))
	}
}

// TestBuildParseOCSP_Good 闭环：未吊销证书 → good。
func TestBuildParseOCSP_Good(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	respDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	status, revokedAt, err := ParseOCSPResponse(ca.Cert, respDER, reqCID)
	if err != nil {
		t.Fatalf("ParseOCSPResponse 失败: %v", err)
	}
	if status != StatusGood {
		t.Errorf("status = %q，期望 %q", status, StatusGood)
	}
	if revokedAt != nil {
		t.Errorf("revokedAt = %v，期望 nil", revokedAt)
	}
}

// TestBuildParseOCSP_Revoked 闭环：吊销后 → revoked，且吊销时间与
// crl.json 记录一致（状态源同源验证）。
func TestBuildParseOCSP_Revoked(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	cn := "edgeflow-ocsp-a"
	// 窗口放宽 ±1s：crl.json 的吊销时间按 RFC3339 秒级精度截断
	before := time.Now().UTC().Add(-time.Second)
	if err := RevokeCert(certDir, cn); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	revoked, err := LoadRevokedList(certDir)
	if err != nil {
		t.Fatalf("LoadRevokedList 失败: %v", err)
	}
	if len(revoked) != 1 || revoked[0].Serial != leaf.SerialNumber.Text(16) {
		t.Fatalf("吊销集合 = %+v，期望含序列号 %s", revoked, leaf.SerialNumber.Text(16))
	}

	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	respDER, err := BuildOCSPResponse(ca, revoked, reqDER, OCSPResponseOptions{})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	status, revokedAt, err := ParseOCSPResponse(ca.Cert, respDER, reqCID)
	if err != nil {
		t.Fatalf("ParseOCSPResponse 失败: %v", err)
	}
	if status != StatusRevoked {
		t.Errorf("status = %q，期望 %q", status, StatusRevoked)
	}
	if revokedAt == nil {
		t.Fatal("revokedAt = nil，期望吊销时间")
	}
	if revokedAt.Before(before) || revokedAt.After(after) {
		t.Errorf("吊销时间 %v 不在吊销操作窗口 [%v, %v] 内", revokedAt, before, after)
	}
	// 吊销时间必须与 crl.json 记录一致（同源）
	list, err := loadRevokedList(certDir)
	if err != nil {
		t.Fatalf("loadRevokedList 失败: %v", err)
	}
	wantTime, err := time.Parse(time.RFC3339, list.Entries[0].Time)
	if err != nil {
		t.Fatalf("解析 crl.json 吊销时间失败: %v", err)
	}
	if !revokedAt.Equal(wantTime) {
		t.Errorf("OCSP 吊销时间 %v 与 crl.json %v 不一致", revokedAt, wantTime)
	}
}

// TestBuildParseOCSP_UnknownOtherCA 闭环：用另一个 CA 签发的证书的
// CertID 查询本 CA responder → unknown（responder 不为其他 CA 背书）。
func TestBuildParseOCSP_UnknownOtherCA(t *testing.T) {
	dirA := newTempCertDir(t)
	caA, _ := mustOCSPEnv(t, dirA, "edgeflow-ocsp-a")
	dirB := newTempCertDir(t)
	caB, leafB := mustOCSPEnv(t, dirB, "edgeflow-ocsp-b")

	// 请求按 CA-B 的哈希构造（模拟 CA-B 的客户端来查）
	reqDER, err := OCSPRequestForCert(caB.Cert, leafB)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	// CA-A 的 responder 应答
	respDER, err := BuildOCSPResponse(caA, nil, reqDER, OCSPResponseOptions{})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	// 客户端用本 CA（CA-A）验签：签名由 CA-A 私钥签发，验签通过，
	// 状态应为 unknown
	status, revokedAt, err := ParseOCSPResponse(caA.Cert, respDER, reqCID)
	if err != nil {
		t.Fatalf("ParseOCSPResponse 失败: %v", err)
	}
	if status != StatusUnknown {
		t.Errorf("status = %q，期望 %q", status, StatusUnknown)
	}
	if revokedAt != nil {
		t.Errorf("revokedAt = %v，期望 nil", revokedAt)
	}
}

// TestParseOCSP_TamperedResponse 篡改响应字节 → 签名验证失败。
func TestParseOCSP_TamperedResponse(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	respDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	// 翻转响应最后一个字节（签名区）
	tampered := append([]byte(nil), respDER...)
	tampered[len(tampered)-1] ^= 0xff
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	if _, _, err := ParseOCSPResponse(ca.Cert, tampered, reqCID); err == nil {
		t.Fatal("篡改后的响应应验签失败")
	} else if !strings.Contains(err.Error(), "签名验证失败") {
		t.Errorf("错误信息不明确: %v", err)
	}
}

// TestParseOCSP_WrongSigner 用另一个 CA 的证书验签 → 失败（防伪造）。
func TestParseOCSP_WrongSigner(t *testing.T) {
	dirA := newTempCertDir(t)
	caA, leafA := mustOCSPEnv(t, dirA, "edgeflow-ocsp-a")
	dirB := newTempCertDir(t)
	caB, _ := mustOCSPEnv(t, dirB, "edgeflow-ocsp-b")

	reqDER, err := OCSPRequestForCert(caA.Cert, leafA)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	respDER, err := BuildOCSPResponse(caA, nil, reqDER, OCSPResponseOptions{})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	// 用 CA-B 的证书验 CA-A 签发的响应 → 必须失败
	if _, _, err := ParseOCSPResponse(caB.Cert, respDER, reqCID); err == nil {
		t.Fatal("用错误 CA 验签应失败")
	}
}

// TestParseOCSP_CertIDMismatch 响应针对证书 A，却用证书 B 的 CertID
// 解析 → 错误（防串响应）。
func TestParseOCSP_CertIDMismatch(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leafA := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	// 同 CA 再签发一张证书 B（直接内存签发，不落盘）
	keyB, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	serialB, err := randomSerial()
	if err != nil {
		t.Fatalf("生成序列号失败: %v", err)
	}
	derB, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serialB,
		Subject:      pkix.Name{CommonName: "edgeflow-ocsp-b"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca.Cert, &keyB.PublicKey, ca.Key)
	if err != nil {
		t.Fatalf("签发证书 B 失败: %v", err)
	}
	leafB, err := x509.ParseCertificate(derB)
	if err != nil {
		t.Fatalf("解析证书 B 失败: %v", err)
	}

	reqDER, err := OCSPRequestForCert(ca.Cert, leafA)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	respDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	// 用证书 B 的 CertID 解析证书 A 的响应 → 必须失败
	cidB, err := CertIDForCert(ca.Cert, leafB)
	if err != nil {
		t.Fatalf("CertIDForCert 失败: %v", err)
	}
	if _, _, err := ParseOCSPResponse(ca.Cert, respDER, cidB); err == nil {
		t.Fatal("CertID 不匹配时应报错")
	} else if !strings.Contains(err.Error(), "不存在与请求 CertID 匹配") {
		t.Errorf("错误信息不明确: %v", err)
	}
}

// TestBuildOCSP_BadRequestDER 非法 DER → ErrMalformedOCSPRequest
// （responder 据此返回 400 语义）。
func TestBuildOCSP_BadRequestDER(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, _ := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	if _, err := BuildOCSPResponse(ca, nil, []byte{0xff, 0x00, 0x01}, OCSPResponseOptions{}); err == nil {
		t.Fatal("非法 DER 应报错")
	} else if !errors.Is(err, ErrMalformedOCSPRequest) {
		t.Errorf("错误应包装 ErrMalformedOCSPRequest，实际: %v", err)
	}
	// 空请求
	if _, err := BuildOCSPResponse(ca, nil, nil, OCSPResponseOptions{}); !errors.Is(err, ErrMalformedOCSPRequest) {
		t.Errorf("空请求应报 ErrMalformedOCSPRequest，实际: %v", err)
	}
	// 无 Request 项的合法外壳
	emptyReq, err := asn1.Marshal(ocspRequest{TBSRequest: tbsRequest{}})
	if err != nil {
		t.Fatalf("构造空请求失败: %v", err)
	}
	if _, err := BuildOCSPResponse(ca, nil, emptyReq, OCSPResponseOptions{}); !errors.Is(err, ErrMalformedOCSPRequest) {
		t.Errorf("无 Request 项应报 ErrMalformedOCSPRequest，实际: %v", err)
	}
}

// TestBuildOCSP_RevokedTimeInvalid crl.json 吊销时间损坏 → 明确报错
// （fail-closed，与 CRL 路径同约定）。
func TestBuildOCSP_RevokedTimeInvalid(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	bad := []RevokedCert{{Serial: leaf.SerialNumber.Text(16), Time: "not-a-time"}}
	if _, err := BuildOCSPResponse(ca, bad, reqDER, OCSPResponseOptions{}); err == nil {
		t.Fatal("吊销时间损坏应报错")
	}
	// 序列号匹配用数值比较：大小写/前导零差异也能命中
	upper := []RevokedCert{{Serial: strings.ToUpper(leaf.SerialNumber.Text(16)), Time: time.Now().UTC().Format(time.RFC3339)}}
	if _, err := BuildOCSPResponse(ca, upper, reqDER, OCSPResponseOptions{}); err != nil {
		t.Errorf("大小写不同的序列号应命中吊销集合: %v", err)
	}
}

// TestBuildOCSP_NilCA CA 句柄为 nil → 报错。
func TestBuildOCSP_NilCA(t *testing.T) {
	if _, err := BuildOCSPResponse(nil, nil, []byte{0x30, 0x00}, OCSPResponseOptions{}); err == nil {
		t.Fatal("nil CA 应报错")
	}
}

// TestParseOCSP_NonSuccessfulStatus responseStatus 非 successful → 报错。
func TestParseOCSP_NonSuccessfulStatus(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, _ := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	// 构造 malformedRequest(1) 响应（无 responseBytes）
	der, err := asn1.Marshal(ocspResponse{ResponseStatus: asn1.Enumerated(1)})
	if err != nil {
		t.Fatalf("构造错误状态响应失败: %v", err)
	}
	cid, err := CertIDForCert(ca.Cert, ca.Cert)
	if err != nil {
		t.Fatalf("CertIDForCert 失败: %v", err)
	}
	if _, _, err := ParseOCSPResponse(ca.Cert, der, cid); err == nil {
		t.Fatal("非 successful 状态应报错")
	} else if !strings.Contains(err.Error(), "malformedRequest") {
		t.Errorf("错误信息应含状态名: %v", err)
	}
}

// TestParseOCSP_WrongResponseType responseBytes 类型非 basic → 报错。
func TestParseOCSP_WrongResponseType(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, _ := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	der, err := asn1.Marshal(ocspResponse{
		ResponseStatus: asn1.Enumerated(0),
		ResponseBytes: responseBytes{
			ResponseType: asn1.ObjectIdentifier{1, 2, 3, 4},
			Response:     []byte{0x30, 0x00},
		},
	})
	if err != nil {
		t.Fatalf("构造响应失败: %v", err)
	}
	cid, err := CertIDForCert(ca.Cert, ca.Cert)
	if err != nil {
		t.Fatalf("CertIDForCert 失败: %v", err)
	}
	if _, _, err := ParseOCSPResponse(ca.Cert, der, cid); err == nil {
		t.Fatal("错误响应类型应报错")
	}
}

// TestParseOCSP_TrailingGarbage 响应带尾部多余数据 → 报错。
func TestParseOCSP_TrailingGarbage(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	respDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	garbage := append(append([]byte(nil), respDER...), 0x00)
	if _, _, err := ParseOCSPResponse(ca.Cert, garbage, reqCID); err == nil {
		t.Fatal("尾部多余数据应报错")
	}
}

// TestParseOCSP_NilArgs nil 参数 → 报错。
func TestParseOCSP_NilArgs(t *testing.T) {
	if _, _, err := ParseOCSPResponse(nil, []byte{0x30, 0x00}, &CertID{}); err == nil {
		t.Fatal("nil CA 应报错")
	}
	certDir := newTempCertDir(t)
	ca, _ := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	if _, _, err := ParseOCSPResponse(ca.Cert, []byte{0x30, 0x00}, nil); err == nil {
		t.Fatal("nil CertID 应报错")
	}
}

// TestParseOCSP_ResponderIDByKey 响应使用 byKey responderID 时，
// 校验 SHA-1(CA 公钥) 匹配（兼容解析）。
func TestParseOCSP_ResponderIDByKey(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	keyBits, err := caPublicKeyBits(ca.Cert)
	if err != nil {
		t.Fatalf("caPublicKeyBits 失败: %v", err)
	}
	keyHash := sha1.Sum(keyBits)
	// byKey 按 EXPLICIT 编码（内容 = OCTET STRING TLV，与 OpenSSL 输出一致）
	byKeyTLV, err := asn1.Marshal(keyHash[:])
	if err != nil {
		t.Fatalf("编码 byKey 失败: %v", err)
	}
	reqCID, err := CertIDForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("CertIDForCert 失败: %v", err)
	}
	rd := responseData{
		ResponderID: asn1.RawValue{
			Class:      asn1.ClassContextSpecific,
			Tag:        2,
			IsCompound: true,
			Bytes:      byKeyTLV,
		},
		ProducedAt: time.Now().UTC(),
		Responses: []singleResponse{{
			CertID:     reqCID.toInternal(),
			CertStatus: certStatusGood(),
			ThisUpdate: time.Now().UTC(),
		}},
	}
	respDER := buildSignedRespForTest(t, ca, rd)
	status, _, err := ParseOCSPResponse(ca.Cert, respDER, reqCID)
	if err != nil {
		t.Fatalf("byKey responder 解析失败: %v", err)
	}
	if status != StatusGood {
		t.Errorf("status = %q，期望 good", status)
	}
}

// TestParseOCSP_ResponderIDWrong 响应 responderID 指向其他 CA → 报错
// （防 CA 混淆）。
func TestParseOCSP_ResponderIDWrong(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	// 用伪造的 byName（另一个名字）构造响应（仍由本 CA 签名）
	otherName, err := asn1.Marshal(pkix.Name{CommonName: "evil-ca"}.ToRDNSequence())
	if err != nil {
		t.Fatalf("构造其他 CA 名字失败: %v", err)
	}
	var otherSeq asn1.RawValue
	if _, err := asn1.Unmarshal(otherName, &otherSeq); err != nil {
		t.Fatalf("解析名字失败: %v", err)
	}
	reqCID, err := CertIDForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("CertIDForCert 失败: %v", err)
	}
	rd := responseData{
		ResponderID: asn1.RawValue{
			Class:      asn1.ClassContextSpecific,
			Tag:        1,
			IsCompound: true,
			Bytes:      otherSeq.Bytes,
		},
		ProducedAt: time.Now().UTC(),
		Responses: []singleResponse{{
			CertID:     reqCID.toInternal(),
			CertStatus: certStatusGood(),
			ThisUpdate: time.Now().UTC(),
		}},
	}
	respDER := buildSignedRespForTest(t, ca, rd)
	if _, _, err := ParseOCSPResponse(ca.Cert, respDER, reqCID); err == nil {
		t.Fatal("responderID 不一致应报错")
	}
}

// TestOCSPStatusAt_HTTP 客户端全闭环（httptest responder）：
// good → revoked 状态流转 + 吊销时间；并覆盖 responder 错误路径。
func TestOCSPStatusAt_HTTP(t *testing.T) {
	certDir := newTempCertDir(t)
	_, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	cn := "edgeflow-ocsp-a"

	// responder 端：复用真实装配路径（LoadCA + LoadRevokedList +
	// BuildOCSPResponse），与 cmd/cloudcore/ocspHandler 同逻辑。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		if _, err := ParseOCSPRequest(body[:n]); err != nil {
			http.Error(w, "malformed ocsp request", http.StatusBadRequest)
			return
		}
		caSrv, err := LoadCA(certDir)
		if err != nil {
			http.Error(w, "CA unavailable", http.StatusInternalServerError)
			return
		}
		revoked, err := LoadRevokedList(certDir)
		if err != nil {
			http.Error(w, "revoked list unavailable", http.StatusInternalServerError)
			return
		}
		respDER, err := BuildOCSPResponse(caSrv, revoked, body[:n], OCSPResponseOptions{})
		if err != nil {
			http.Error(w, "build failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/ocsp-response")
		_, _ = w.Write(respDER)
	}))
	defer srv.Close()

	// good
	status, revokedAt, err := OCSPStatusAt(certDir, leaf, srv.URL)
	if err != nil {
		t.Fatalf("OCSPStatusAt(good) 失败: %v", err)
	}
	if status != StatusGood || revokedAt != nil {
		t.Errorf("good 状态 = %q/%v，期望 good/nil", status, revokedAt)
	}

	// revoked
	if err := RevokeCert(certDir, cn); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	status, revokedAt, err = OCSPStatusAt(certDir, leaf, srv.URL)
	if err != nil {
		t.Fatalf("OCSPStatusAt(revoked) 失败: %v", err)
	}
	if status != StatusRevoked || revokedAt == nil {
		t.Errorf("revoked 状态 = %q/%v，期望 revoked/非 nil", status, revokedAt)
	}

	// 默认 URL 不可达 → 报错
	if _, _, err := OCSPStatus(certDir, leaf); err == nil {
		t.Fatal("默认 responder 不可达应报错")
	}

	// responder 返回 400 → 报错
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "malformed", http.StatusBadRequest)
	}))
	defer badSrv.Close()
	if _, _, err := OCSPStatusAt(certDir, leaf, badSrv.URL); err == nil {
		t.Fatal("HTTP 400 应报错")
	} else if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("错误信息应含 HTTP 400: %v", err)
	}

	// responder 返回垃圾 200 → 解析失败
	garbageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not a real ocsp response"))
	}))
	defer garbageSrv.Close()
	if _, _, err := OCSPStatusAt(certDir, leaf, garbageSrv.URL); err == nil {
		t.Fatal("垃圾响应应报错")
	}

	// nil 参数
	if _, _, err := OCSPStatusAt(certDir, nil, srv.URL); err == nil {
		t.Fatal("nil leaf 应报错")
	}
	if _, _, err := OCSPStatusAt(certDir, leaf, ""); err == nil {
		t.Fatal("空 URL 应报错")
	}
	// 证书目录无 CA → 报错
	if _, _, err := OCSPStatusAt(newTempCertDir(t), leaf, srv.URL); err == nil {
		t.Fatal("无 CA 的目录应报错")
	}
}

// TestOCSPStatusAt_ResponderVerifiesClientRequest 验证 responder 端
// 对客户端请求的完整校验：非法 DER 走 400（BuildOCSPResponse 返回
// ErrMalformedOCSPRequest）。
func TestOCSPStatusAt_ResponderVerifiesClientRequest(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, _ := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	if _, err := BuildOCSPResponse(ca, nil, []byte("garbage"), OCSPResponseOptions{}); !errors.Is(err, ErrMalformedOCSPRequest) {
		t.Fatalf("非法请求应报 ErrMalformedOCSPRequest，实际: %v", err)
	}
}

// buildSignedRespForTest 绕过 BuildOCSPResponse 的固定 responderID，
// 构造由 ca 签名的 OCSPResponse（用于 byKey/错误 responderID 等分支测试）。
func buildSignedRespForTest(t *testing.T, ca *CA, rd responseData) []byte {
	t.Helper()
	tbsDER, err := asn1.Marshal(rd)
	if err != nil {
		t.Fatalf("编码 ResponseData 失败: %v", err)
	}
	digest := sha256.Sum256(tbsDER)
	sig, err := rsa.SignPKCS1v15(rand.Reader, ca.Key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	basic, err := asn1.Marshal(basicOCSPResponse{
		TBSResponseData:    asn1.RawValue{FullBytes: tbsDER},
		SignatureAlgorithm: pkix.AlgorithmIdentifier{Algorithm: oidSHA256WithRSA, Parameters: asn1.NullRawValue},
		Signature:          asn1.BitString{Bytes: sig, BitLength: len(sig) * 8},
	})
	if err != nil {
		t.Fatalf("编码 BasicOCSPResponse 失败: %v", err)
	}
	der, err := asn1.Marshal(ocspResponse{
		ResponseStatus: asn1.Enumerated(0),
		ResponseBytes:  responseBytes{ResponseType: oidOCSPBasic, Response: basic},
	})
	if err != nil {
		t.Fatalf("编码 OCSPResponse 失败: %v", err)
	}
	return der
}

// ---- 新鲜度校验（M2：防重放绕过吊销）----

// TestParseOCSPResponseWithFreshness_Good 新鲜响应（默认 7 天有效期）
// 通过 fail-closed 默认策略。
func TestParseOCSPResponseWithFreshness_Good(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	respDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	status, revokedAt, err := ParseOCSPResponseWithFreshness(ca.Cert, respDER, reqCID, DefaultFreshnessPolicy())
	if err != nil {
		t.Fatalf("新鲜响应应通过校验，实际: %v", err)
	}
	if status != StatusGood || revokedAt != nil {
		t.Errorf("状态 = %q/%v，期望 good/nil", status, revokedAt)
	}
}

// TestParseOCSPResponseWithFreshness_Expired nextUpdate 已过 →
// ErrOCSPResponseStale（fail-closed 拒绝，防重放绕过吊销）。
func TestParseOCSPResponseWithFreshness_Expired(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	// 构造已过期响应：thisUpdate=-48h，nextUpdate=-24h
	past := time.Now().Add(-48 * time.Hour)
	respDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{
		ThisUpdate: past,
		NextUpdate: past.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	status, revokedAt, err := ParseOCSPResponseWithFreshness(ca.Cert, respDER, reqCID, DefaultFreshnessPolicy())
	if err == nil {
		t.Fatal("过期响应应被拒绝，实际无错误")
	}
	if !errors.Is(err, ErrOCSPResponseStale) {
		t.Errorf("错误应包装 ErrOCSPResponseStale，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "nextUpdate") {
		t.Errorf("错误信息应含 nextUpdate: %v", err)
	}
	// 新鲜度失败仍返回已解析的状态（good），便于调用方决策
	if status != StatusGood || revokedAt != nil {
		t.Errorf("状态 = %q/%v，期望 good/nil", status, revokedAt)
	}
}

// TestParseOCSPResponseWithFreshness_FutureProducedAt producedAt 在未来
// （超过时钟偏差容忍）→ ErrOCSPResponseFutureTime（伪造/重放）。
func TestParseOCSPResponseWithFreshness_FutureProducedAt(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	// producedAt = thisUpdate = now+1h（超过默认 5 分钟时钟偏差容忍）
	future := time.Now().Add(time.Hour)
	respDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{ThisUpdate: future})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	if _, _, err := ParseOCSPResponseWithFreshness(ca.Cert, respDER, reqCID, DefaultFreshnessPolicy()); err == nil {
		t.Fatal("未来 producedAt 应被拒绝，实际无错误")
	} else if !errors.Is(err, ErrOCSPResponseFutureTime) {
		t.Errorf("错误应包装 ErrOCSPResponseFutureTime，实际: %v", err)
	}
}

// TestParseOCSPResponseWithFreshness_ZeroPolicyBackwardCompat 零值策略
// （CheckFreshness=false）= 旧行为：过期响应不报错（ParseOCSPResponse /
// ParseOCSPResponseWithFreshness 零值策略均放行），保持旧签名兼容。
func TestParseOCSPResponseWithFreshness_ZeroPolicyBackwardCompat(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	past := time.Now().Add(-48 * time.Hour)
	respDER, err := BuildOCSPResponse(ca, nil, reqDER, OCSPResponseOptions{
		ThisUpdate: past,
		NextUpdate: past.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	// 旧入口：不校验新鲜度 → 放行
	if status, _, err := ParseOCSPResponse(ca.Cert, respDER, reqCID); err != nil || status != StatusGood {
		t.Errorf("旧入口应放行过期响应（旧行为），实际 status=%q err=%v", status, err)
	}
	// 新入口 + 零值策略：同样不校验（兼容）
	if status, _, err := ParseOCSPResponseWithFreshness(ca.Cert, respDER, reqCID, FreshnessPolicy{}); err != nil || status != StatusGood {
		t.Errorf("零值策略应放行过期响应，实际 status=%q err=%v", status, err)
	}
}

// TestOCSPStatusAtWithPolicy_StaleRejected 客户端全闭环：responder 返回
// 过期响应 → OCSPStatusAtWithPolicy（默认策略）报 ErrOCSPResponseStale；
// 旧入口 OCSPStatusAt 不校验新鲜度仍返回 good（旧签名兼容）。
func TestOCSPStatusAtWithPolicy_StaleRejected(t *testing.T) {
	certDir := newTempCertDir(t)
	_, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")

	// responder：固定返回已过期响应
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		if _, err := ParseOCSPRequest(body[:n]); err != nil {
			http.Error(w, "malformed ocsp request", http.StatusBadRequest)
			return
		}
		caSrv, err := LoadCA(certDir)
		if err != nil {
			http.Error(w, "CA unavailable", http.StatusInternalServerError)
			return
		}
		past := time.Now().Add(-48 * time.Hour)
		respDER, err := BuildOCSPResponse(caSrv, nil, body[:n], OCSPResponseOptions{
			ThisUpdate: past,
			NextUpdate: past.Add(24 * time.Hour),
		})
		if err != nil {
			http.Error(w, "build failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/ocsp-response")
		_, _ = w.Write(respDER)
	}))
	defer srv.Close()

	// 新入口 + fail-closed 默认策略 → 过期拒绝
	if _, _, err := OCSPStatusAtWithPolicy(certDir, leaf, srv.URL, DefaultFreshnessPolicy()); err == nil {
		t.Fatal("OCSPStatusAtWithPolicy 应拒绝过期响应，实际无错误")
	} else if !errors.Is(err, ErrOCSPResponseStale) {
		t.Errorf("错误应包装 ErrOCSPResponseStale，实际: %v", err)
	}
	// 旧入口 → 不校验新鲜度，仍返回 good（旧签名兼容）
	if status, _, err := OCSPStatusAt(certDir, leaf, srv.URL); err != nil || status != StatusGood {
		t.Errorf("旧入口应放行过期响应，实际 status=%q err=%v", status, err)
	}
	// 新入口 + 零值策略 → 同样放行（策略兼容）
	if status, _, err := OCSPStatusAtWithPolicy(certDir, leaf, srv.URL, FreshnessPolicy{}); err != nil || status != StatusGood {
		t.Errorf("零值策略应放行过期响应，实际 status=%q err=%v", status, err)
	}
}

// TestParseOCSPResponseWithFreshness_StaleRevoked 过期但状态=revoked：
// 新鲜度失败仍返回 revoked + 吊销时间（吊销不可逆，调用方可自行决策），
// 但错误仍非 nil（默认 fail-closed）。
func TestParseOCSPResponseWithFreshness_StaleRevoked(t *testing.T) {
	certDir := newTempCertDir(t)
	ca, leaf := mustOCSPEnv(t, certDir, "edgeflow-ocsp-a")
	cn := "edgeflow-ocsp-a"
	if err := RevokeCert(certDir, cn); err != nil {
		t.Fatalf("RevokeCert 失败: %v", err)
	}
	revoked, err := LoadRevokedList(certDir)
	if err != nil {
		t.Fatalf("LoadRevokedList 失败: %v", err)
	}
	reqDER, err := OCSPRequestForCert(ca.Cert, leaf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	past := time.Now().Add(-48 * time.Hour)
	respDER, err := BuildOCSPResponse(ca, revoked, reqDER, OCSPResponseOptions{
		ThisUpdate: past,
		NextUpdate: past.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("BuildOCSPResponse 失败: %v", err)
	}
	reqCID, err := ParseOCSPRequest(reqDER)
	if err != nil {
		t.Fatalf("ParseOCSPRequest 失败: %v", err)
	}
	status, revokedAt, err := ParseOCSPResponseWithFreshness(ca.Cert, respDER, reqCID, DefaultFreshnessPolicy())
	if !errors.Is(err, ErrOCSPResponseStale) {
		t.Fatalf("过期响应应报 ErrOCSPResponseStale，实际: %v", err)
	}
	if status != StatusRevoked || revokedAt == nil {
		t.Errorf("过期 revoked 响应应返回 revoked + 吊销时间，实际 %q/%v", status, revokedAt)
	}
}
