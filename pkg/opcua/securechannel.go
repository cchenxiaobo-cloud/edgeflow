package opcua

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SecureChannel 层（OPC UA Part 6 §6.7.3，SecurityPolicy None）：
// 在既有 Conn（裸 HEL/ACK 传输）之上实现 OPN/CLO 通道生命周期，
// 为 MSG 服务消息提供对称安全头 + 序列号 + 请求/响应关联。

// SecurityPolicyNoneURI 是 SecurityPolicy None 的策略 URI。
const SecurityPolicyNoneURI = "http://opcfoundation.org/UA/SecurityPolicy#None"

// OpenSecureChannel 请求类型（RequestType）。
const (
	SecurityTokenRequestTypeIssue uint32 = 0 // Issue
	SecurityTokenRequestTypeRenew uint32 = 1 // Renew（v1 仅 Issue）
)

// DefaultRequestedLifetime 是请求的通道生命周期（毫秒，10 分钟）。
const DefaultRequestedLifetime uint32 = 600000

// 安全头与序列头（OPC UA Part 6 §6.7.3.1/§6.7.3.2）。
type AsymmetricSecurityHeader struct {
	SecurityPolicyURI             string
	SenderCertificate             []byte
	ReceiverCertificateThumbprint []byte
}

func (h AsymmetricSecurityHeader) encodeUA(e *encoder) error {
	e.str(h.SecurityPolicyURI)
	e.bytes(h.SenderCertificate)
	e.bytes(h.ReceiverCertificateThumbprint)
	return nil
}

func decodeAsymmetricSecurityHeader(d *decoder) (AsymmetricSecurityHeader, error) {
	var h AsymmetricSecurityHeader
	var err error
	if h.SecurityPolicyURI, err = d.str(); err != nil {
		return h, err
	}
	if h.SenderCertificate, err = d.bytes(); err != nil {
		return h, err
	}
	if h.ReceiverCertificateThumbprint, err = d.bytes(); err != nil {
		return h, err
	}
	return h, nil
}

type SymmetricSecurityHeader struct {
	TokenID uint32
}

func (h SymmetricSecurityHeader) encodeUA(e *encoder) error {
	e.u32(h.TokenID)
	return nil
}

func decodeSymmetricSecurityHeader(d *decoder) (SymmetricSecurityHeader, error) {
	var h SymmetricSecurityHeader
	var err error
	if h.TokenID, err = d.u32(); err != nil {
		return h, err
	}
	return h, nil
}

type SequenceHeader struct {
	SequenceNumber uint32
	RequestID      uint32
}

func (h SequenceHeader) encodeUA(e *encoder) error {
	e.u32(h.SequenceNumber)
	e.u32(h.RequestID)
	return nil
}

func decodeSequenceHeader(d *decoder) (SequenceHeader, error) {
	var h SequenceHeader
	var err error
	if h.SequenceNumber, err = d.u32(); err != nil {
		return h, err
	}
	if h.RequestID, err = d.u32(); err != nil {
		return h, err
	}
	return h, nil
}

// OpenSecureChannelRequest 打开安全通道（OPN 消息体，Part 4 §5.5.2）。
type OpenSecureChannelRequest struct {
	ClientProtocolVersion uint32 // 0
	RequestType           uint32 // 0=Issue
	RequestedLifetime     uint32
}

func (r OpenSecureChannelRequest) encodeUA(e *encoder) error {
	e.u32(r.ClientProtocolVersion)
	e.u32(r.RequestType)
	e.u32(r.RequestedLifetime)
	return nil
}

func decodeOpenSecureChannelRequest(d *decoder) (OpenSecureChannelRequest, error) {
	var r OpenSecureChannelRequest
	var err error
	if r.ClientProtocolVersion, err = d.u32(); err != nil {
		return r, err
	}
	if r.RequestType, err = d.u32(); err != nil {
		return r, err
	}
	if r.RequestedLifetime, err = d.u32(); err != nil {
		return r, err
	}
	return r, nil
}

// ChannelSecurityToken 是通道安全令牌（Part 4 §7.33）。
type ChannelSecurityToken struct {
	ChannelID       uint32
	TokenID         uint32
	CreatedAt       DateTime
	RevisedLifetime float64
}

func (t ChannelSecurityToken) encodeUA(e *encoder) error {
	e.u32(t.ChannelID)
	e.u32(t.TokenID)
	e.i64(int64(t.CreatedAt))
	e.f64(t.RevisedLifetime)
	return nil
}

func decodeChannelSecurityToken(d *decoder) (ChannelSecurityToken, error) {
	var t ChannelSecurityToken
	var err error
	if t.ChannelID, err = d.u32(); err != nil {
		return t, err
	}
	if t.TokenID, err = d.u32(); err != nil {
		return t, err
	}
	if v, err := d.i64(); err != nil {
		return t, err
	} else {
		t.CreatedAt = DateTime(v)
	}
	if t.RevisedLifetime, err = d.f64(); err != nil {
		return t, err
	}
	return t, nil
}

// OpenSecureChannelResponse 是 OPN 响应（Part 4 §5.5.2）。
type OpenSecureChannelResponse struct {
	// ResponseHeader（OPN 响应也带，需 DiagnosticInfo 解码）。
	Timestamp          DateTime
	RequestHandle      uint32
	ServiceResult      StatusCode
	ServiceDiagnostics DiagnosticInfo
	StringTable        []string
	AdditionalHeader   ExtensionObject

	ServerProtocolVersion uint32
	SecurityToken         ChannelSecurityToken
	ServerNonce           []byte
}

func (r OpenSecureChannelResponse) encodeUA(e *encoder) error {
	e.i64(int64(r.Timestamp))
	e.u32(r.RequestHandle)
	e.u32(uint32(r.ServiceResult))
	if err := r.ServiceDiagnostics.encodeUA(e); err != nil {
		return err
	}
	encodeStringList(e, r.StringTable)
	if err := r.AdditionalHeader.encodeUA(e); err != nil {
		return err
	}
	e.u32(r.ServerProtocolVersion)
	if err := r.SecurityToken.encodeUA(e); err != nil {
		return err
	}
	e.bytes(r.ServerNonce)
	return nil
}

func decodeOpenSecureChannelResponse(d *decoder) (OpenSecureChannelResponse, error) {
	var r OpenSecureChannelResponse
	var err error
	if v, err := d.i64(); err != nil {
		return r, err
	} else {
		r.Timestamp = DateTime(v)
	}
	if r.RequestHandle, err = d.u32(); err != nil {
		return r, err
	}
	if v, err := d.u32(); err != nil {
		return r, err
	} else {
		r.ServiceResult = StatusCode(v)
	}
	if r.ServiceDiagnostics, err = decodeDiagnosticInfo(d, 0); err != nil {
		return r, err
	}
	if r.StringTable, err = decodeStringList(d); err != nil {
		return r, err
	}
	if r.AdditionalHeader, err = decodeExtensionObject(d); err != nil {
		return r, err
	}
	if r.ServerProtocolVersion, err = d.u32(); err != nil {
		return r, err
	}
	if r.SecurityToken, err = decodeChannelSecurityToken(d); err != nil {
		return r, err
	}
	if r.ServerNonce, err = d.bytes(); err != nil {
		return r, err
	}
	return r, nil
}

// CloseSecureChannelRequest 关闭安全通道（CLO 消息体为空字段）。
type CloseSecureChannelRequest struct{}

// SecureChannel 是客户端侧的安全通道会话：持有一个已 OPN 的 Conn，
// 提供 MSG 服务消息的发送/响应关联。
type SecureChannel struct {
	conn      *Conn
	channelId uint32
	tokenId   uint32
	seq       uint32 // 出站 SequenceNumber（每帧 +1）
	reqId     uint32 // 出站 RequestId（每请求 +1）

	sendMu sync.Mutex // 串行化“分配 reqId+整帧写”，单请求发送原子性（v0.15.0 泵模式前提）

	// opts 是 v0.28.0 引入的 OPN 可选配置（默认零值 = v0.27.0 逐字行为）。
	opts OpenSecureChannelOptions
	// policy 是 sendOPN 校验后的协商策略 URI（""=未 OPN）。
	policy string
	// clientNonce/serverNonce 是 v0.28.1 Basic256Sha256 握手的随机数
	//（B256NonceLen=32B，crypto/rand）；None 路径为空。派生密钥 keys
	// 仅在双方握手成功后持有（v0.29.0 MSG 对称覆盖接线用）。
	clientNonce []byte
	serverNonce []byte
	keys        *DerivedKeys
}

// MaxEndpointUrlLength 是 Hello.EndpointUrl 的长度上限（PRT-15）。
// 服务端一般限 4096；超长 URL 视为构造异常，在编码前拒绝。
const MaxEndpointUrlLength = 4096

// OpenSecureChannel 在已握手的 Conn 上执行 OPN 并返回就绪通道。
// OPN 成功后 conn.channelId 被写入，后续 WriteMessage/ReadMessage
// 自动带上真实 ChannelId（既有导出 API 零变更）。
//
// opts 为 v0.28.0 引入的可选入参（v0.27.0 调用点为零值）；零值 = 完全
// 逐字的 v0.27.0 行为：SecurityPolicyURI=空 → SecurityPolicyNoneURI；
// ClientCert/ServerCert/ClientKey 缺失 → 不加密、不签名。
func (c *Conn) OpenSecureChannelWith(timeout time.Duration, opts OpenSecureChannelOptions) (*SecureChannel, error) {
	sc := &SecureChannel{conn: c, opts: opts}
	if err := sc.sendOPN(timeout); err != nil {
		return nil, err
	}
	if err := sc.recvOPN(timeout); err != nil {
		return nil, err
	}
	c.channelId = sc.channelId
	return sc, nil
}

// OpenSecureChannel 在已握手的 Conn 上执行 OPN 并返回就绪通道（v0.27.0
// 原型），调用 OpenSecureChannelWith 零值 opts，保持字节级一致。
func (c *Conn) OpenSecureChannel(timeout time.Duration) (*SecureChannel, error) {
	return c.OpenSecureChannelWith(timeout, OpenSecureChannelOptions{})
}

// OpenSecureChannelOptions 是 v0.28.0 引入的 OPN 可选配置：
// SecurityPolicyURI == "" 代表 SecurityPolicyNoneURI（v0.27.0 逐字一致）；
// 在 Basic256Sha256 下需要 ClientCert/ClientKey/ServerCert 三个字段同时
// 提供、字段不完整时拒绝（PROT-1）以避免出现「策略选了加密但证书为空」的
// 半加密状态。
type OpenSecureChannelOptions struct {
	SecurityPolicyURI string            // SecurityPolicy#None / #Basic256Sha256
	ClientCert        *x509.Certificate // 客户端证书（Basic256Sha256 必填）
	ClientKey         *rsa.PrivateKey   // 客户端私钥（Basic256Sha256 必填）
	ServerCert        *x509.Certificate // 服务端证书（Basic256Sha256 必填）
}

// validateSecurityOptions 验证 OpenSecureChannelOptions 与策略 URI 的一致性：
//   - 空 / NoneURI → 全部字段空 = v0.27.0 行为
//   - Basic256Sha256URI → 必须三个证书字段均提供，否则拒绝
//   - 其他 → 拒绝（v0.28.0 不实现 Basic128Rsa15 / Basic256 等）
func (o OpenSecureChannelOptions) validateSecurityOptions() (string, error) {
	if o.SecurityPolicyURI == "" {
		return SecurityPolicyNoneURI, nil
	}
	switch o.SecurityPolicyURI {
	case SecurityPolicyNoneURI:
		return SecurityPolicyNoneURI, nil
	case SecurityPolicyBasic256Sha256URI:
		if o.ClientCert == nil || o.ClientKey == nil || o.ServerCert == nil {
			return "", errors.New("opcua: Basic256Sha256 要求 ClientCert/ClientKey/ServerCert 同时提供")
		}
		return SecurityPolicyBasic256Sha256URI, nil
	default:
		return "", fmt.Errorf("opcua: 不支持的 SecurityPolicyURI %q (v0.28.0 仅实现 None 与 Basic256Sha256)", o.SecurityPolicyURI)
	}
}

// RequestID 返回下一个出站 RequestId（诊断/测试用）。
func (sc *SecureChannel) RequestID() uint32 { return sc.reqId }

// ChannelID 返回协商出的通道 id。
func (sc *SecureChannel) ChannelID() uint32 { return sc.channelId }

// TokenID 返回协商出的安全令牌 id。
func (sc *SecureChannel) TokenID() uint32 { return sc.tokenId }

// nextReqID 分配并返回下一个出站 RequestId。
func (sc *SecureChannel) nextReqID() uint32 {
	sc.reqId++
	return sc.reqId
}

// sendOPN 发送 OpenSecureChannel 请求（AsymmetricSecurityHeader +
// SequenceHeader + OpenSecureChannelRequest）。
//
// v0.28.0：策略 URI 与证书字段由 opts 驱动；零值 opts 时为 NoneURI+空字段，
// 与 v0.27.0 字节一致。
// v0.28.1：Basic256Sha256 路径发送加密体 + 签名（线格式与签名/派生规则见
// specs/0001-opn-body-encryption/spec.md；服务端镜像实现见
// opcuasim.handleB256OpenSecureChannel；None 请求体编码逐字不变）。
func (sc *SecureChannel) sendOPN(timeout time.Duration) error {
	if timeout > 0 {
		if err := sc.conn.netConn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		defer func() { _ = sc.conn.netConn.SetDeadline(time.Time{}) }()
	}
	policyURI, err := sc.opts.validateSecurityOptions()
	if err != nil {
		return err
	}
	sc.policy = policyURI
	// 公用部分：SequenceHeader + OpenSecureChannelRequest body。
	var bodyEnc encoder
	reqID := sc.nextReqID()
	if err := (SequenceHeader{SequenceNumber: nextSeq(&sc.seq), RequestID: reqID}).encodeUA(&bodyEnc); err != nil {
		return err
	}
	if err := (OpenSecureChannelRequest{
		ClientProtocolVersion: 0,
		RequestType:           SecurityTokenRequestTypeIssue,
		RequestedLifetime:     DefaultRequestedLifetime,
	}).encodeUA(&bodyEnc); err != nil {
		return err
	}
	if policyURI == SecurityPolicyNoneURI {
		// v0.27.0 原路径：AsymHeader 全空字段。
		var e encoder
		if err := (AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI}).encodeUA(&e); err != nil {
			return err
		}
		e.raw(bodyEnc.buf)
		return sc.conn.WriteMessage(MsgOpenSecureChannel, e.buf)
	}
	// Basic256Sha256 路径（v0.28.1）：OPN 请求体加密 + 签名（Part 6
	// §6.7.2/§6.7.5.3）。
	// 线格式：AsymHeader(策略+SenderCert+ReceiverThumbprint) +
	// SignatureData{算法URI, 签名} + EncryptedData。
	//   legacyBody  = SequenceHeader + 旧三字段请求（None 路径同一编码）
	//   EncryptedData = RSA-OAEP-SHA1(serverPub, ClientNonce(32B) || legacyBody)
	//   签名         = RSA-PKCS1v15-SHA1(AsymHeader线编码 || legacyBody)
	// None 路径的旧三字段线上格式逐字不变（冻结带）；加密体内不复用
	// RequestHeader/MessageSecurityMode 扩展（规范偏差登记 KNOWN-ISSUES §29：
	// ClientNonce 直接作为加密体前缀，避免双重请求格式）。
	if sc.opts.ClientCert == nil || sc.opts.ServerCert == nil || sc.opts.ClientKey == nil {
		return errors.New("opcua: sendOPN Basic256Sha256 缺失证书/私钥")
	}
	nonce := make([]byte, B256NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("opcua: 生成 ClientNonce 失败: %w", err)
	}
	sc.clientNonce = nonce
	var hdrEnc encoder
	asymHdr := AsymmetricSecurityHeader{
		SecurityPolicyURI:             SecurityPolicyBasic256Sha256URI,
		SenderCertificate:             sc.opts.ClientCert.Raw,
		ReceiverCertificateThumbprint: SHA1ThumbprintOfCert(sc.opts.ServerCert),
	}
	if err := asymHdr.encodeUA(&hdrEnc); err != nil {
		return err
	}
	asymWire := append([]byte{}, hdrEnc.buf...)
	sig, err := SignOPNBody(sc.opts.ClientKey, asymWire, bodyEnc.buf)
	if err != nil {
		return err
	}
	var sdEnc encoder
	if err := (SignatureData{Algorithm: AsymmetricSignatureAlgorithmRsaSha1, Signature: sig}).encodeUA(&sdEnc); err != nil {
		return err
	}
	inner := append(append([]byte{}, nonce...), bodyEnc.buf...)
	serverPub, ok := sc.opts.ServerCert.PublicKey.(*rsa.PublicKey)
	if !ok || serverPub == nil {
		return errors.New("opcua: 服务端证书公钥不是 RSA（Basic256Sha256 要求 RSA）")
	}
	ct, err := WrapOPNBody(serverPub, inner)
	if err != nil {
		return err
	}
	var e encoder
	e.raw(asymWire)
	e.raw(sdEnc.buf)
	e.raw(ct)
	return sc.conn.WriteMessage(MsgOpenSecureChannel, e.buf)
}

// recvOPN 读取并校验 OPN 响应。
func (sc *SecureChannel) recvOPN(timeout time.Duration) error {
	if timeout > 0 {
		if err := sc.conn.netConn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		defer func() { _ = sc.conn.netConn.SetDeadline(time.Time{}) }()
	}
	msgType, body, err := sc.conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("opcua: read OPN reply: %w", err)
	}
	if msgType == MsgError {
		em, err := DecodeError(body)
		if err != nil {
			return fmt.Errorf("opcua: decode OPN Error: %w", err)
		}
		return fmt.Errorf("opcua: server rejected OpenSecureChannel: %s (%s)", em.ErrorReason, em.ErrorCode)
	}
	if msgType != MsgOpenSecureChannel {
		return fmt.Errorf("opcua: unexpected OPN reply %q", msgType)
	}
	var d decoder
	d.b = body
	asym, err := decodeAsymmetricSecurityHeader(&d)
	if err != nil {
		return fmt.Errorf("opcua: decode OPN asymmetric header: %w", err)
	}
	// v0.28.0：策略/证书字段校验按协商策略分支（None 语义与错误文案逐字保留）。
	if err := validateOPNResponseHeader(asym, sc.policy, sc.opts); err != nil {
		return err
	}
	// v0.28.1：Basic256Sha256 响应体解封 + 验签 + 密钥派生。
	// 线格式：AsymHeader + SignatureData + EncryptedData(RSA-OAEP(client pub,
	// ServerNonce(32B) || inner))；inner = SequenceHeader + OpenSecureChannelResponse；
	// 签名 = RSA-PKCS1v15-SHA1(AsymHeader线编码 || inner)（服务端私钥）。
	if sc.policy == SecurityPolicyBasic256Sha256URI {
		var sd SignatureData
		if sd, err = decodeSignatureData(&d); err != nil {
			return fmt.Errorf("opcua: decode OPN SignatureData: %w", err)
		}
		ct := d.b[d.off:]
		plain, decErr := UnwrapOPNBody(sc.opts.ClientKey, ct)
		if decErr != nil {
			return fmt.Errorf("opcua: OPN 响应解封失败: %w", decErr)
		}
		if len(plain) < B256NonceLen {
			return fmt.Errorf("opcua: OPN 响应明文过短（%dB）", len(plain))
		}
		sc.serverNonce = plain[:B256NonceLen]
		inner := plain[B256NonceLen:]
		serverPub, ok := sc.opts.ServerCert.PublicKey.(*rsa.PublicKey)
		if !ok || serverPub == nil {
			return errors.New("opcua: pin 的服务端证书公钥不是 RSA（Basic256Sha256 要求 RSA）")
		}
		var asymWireEnc encoder
		if err := asym.encodeUA(&asymWireEnc); err != nil {
			return err
		}
		if !VerifyOPNBody(serverPub, asymWireEnc.buf, inner, sd.Signature) {
			return errors.New("opcua: OPN 响应签名验证失败")
		}
		var id decoder
		id.b = inner
		if _, err := decodeSequenceHeader(&id); err != nil {
			return fmt.Errorf("opcua: decode OPN sequence header: %w", err)
		}
		resp, err := decodeOpenSecureChannelResponse(&id)
		if err != nil {
			return fmt.Errorf("opcua: decode OPN response: %w", err)
		}
		if !resp.ServiceResult.IsGood() {
			return fmt.Errorf("opcua: OpenSecureChannel 服务失败: %s", resp.ServiceResult)
		}
		if resp.SecurityToken.RevisedLifetime <= 0 {
			return fmt.Errorf("opcua: OPN 响应 RevisedLifetime=%v 非法（须 > 0）", resp.SecurityToken.RevisedLifetime)
		}
		if len(resp.ServerNonce) != B256NonceLen {
			return fmt.Errorf("opcua: OPN 响应 ServerNonce 长度 %d 非 %d", len(resp.ServerNonce), B256NonceLen)
		}
		if !bytes.Equal(resp.ServerNonce, sc.serverNonce) {
			return errors.New("opcua: OPN 响应 ServerNonce 与加密体前缀不一致")
		}
		sc.channelId = resp.SecurityToken.ChannelID
		sc.tokenId = resp.SecurityToken.TokenID
		sc.keys = DeriveKeys(sc.clientNonce, sc.serverNonce, sc.opts.ClientCert.Raw, sc.opts.ServerCert.Raw)
		return nil
	}
	if _, err := decodeSequenceHeader(&d); err != nil {
		return fmt.Errorf("opcua: decode OPN sequence header: %w", err)
	}
	resp, err := decodeOpenSecureChannelResponse(&d)
	if err != nil {
		return fmt.Errorf("opcua: decode OPN response: %w", err)
	}
	if !resp.ServiceResult.IsGood() {
		return fmt.Errorf("opcua: OpenSecureChannel 服务失败: %s", resp.ServiceResult)
	}
	// PRT-08：协商出的安全令牌必须可用，RevisedLifetime>0（0 会使
	// 通道生命周期立即失效，属异常/恶意响应）。
	if resp.SecurityToken.RevisedLifetime <= 0 {
		return fmt.Errorf("opcua: OPN 响应 RevisedLifetime=%v 非法（须 > 0）", resp.SecurityToken.RevisedLifetime)
	}
	sc.channelId = resp.SecurityToken.ChannelID
	sc.tokenId = resp.SecurityToken.TokenID
	return nil
}

// sendSecure 发送一条 MSG 服务消息（SymmetricSecurityHeader +
// SequenceHeader + 服务 body），返回本次 RequestId。
func (sc *SecureChannel) sendSecure(body []byte) (uint32, error) {
	sc.sendMu.Lock()
	defer sc.sendMu.Unlock()
	var e encoder
	if err := (SymmetricSecurityHeader{TokenID: sc.tokenId}).encodeUA(&e); err != nil {
		return 0, err
	}
	reqID := sc.nextReqID()
	if err := (SequenceHeader{SequenceNumber: nextSeq(&sc.seq), RequestID: reqID}).encodeUA(&e); err != nil {
		return 0, err
	}
	e.raw(body)
	if err := sc.conn.WriteMessage(MsgSecureMessage, e.buf); err != nil {
		return 0, err
	}
	return reqID, nil
}

// recvSecure 读取下一条 MSG 响应并关联到期望 RequestId。
// 非 MSG 帧（如 CLO 后无帧）报错；读取设上限防死循环。
func (sc *SecureChannel) recvSecure(wantReqID uint32, timeout time.Duration) ([]byte, error) {
	if timeout > 0 {
		if err := sc.conn.netConn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		defer func() { _ = sc.conn.netConn.SetDeadline(time.Time{}) }()
	}
	for i := 0; i < 32; i++ {
		msgType, body, err := sc.conn.ReadMessage()
		if err != nil {
			// PRT-16：Abort 帧是规范允许的发送方弃报行为，连接可继续；
			// 读到 Abort 跳过继续等期望响应。
			if errors.Is(err, ErrPeerAbort) {
				continue
			}
			return nil, fmt.Errorf("opcua: read service reply: %w", err)
		}
		if msgType == MsgError {
			em, err := DecodeError(body)
			if err != nil {
				return nil, fmt.Errorf("opcua: decode service Error: %w", err)
			}
			return nil, fmt.Errorf("opcua: server service error: %s (%s)", em.ErrorReason, em.ErrorCode)
		}
		if msgType != MsgSecureMessage {
			return nil, fmt.Errorf("opcua: unexpected service reply %q", msgType)
		}
		var d decoder
		d.b = body
		if _, err := decodeSymmetricSecurityHeader(&d); err != nil {
			return nil, fmt.Errorf("opcua: decode symmetric header: %w", err)
		}
		sh, err := decodeSequenceHeader(&d)
		if err != nil {
			return nil, fmt.Errorf("opcua: decode sequence header: %w", err)
		}
		if sh.RequestID != wantReqID {
			// 不匹配的响应（重发/乱序）：跳过继续读
			continue
		}
		return d.b[d.off:], nil
	}
	return nil, errors.New("opcua: 服务响应 RequestId 关联超限")
}

// validateOPNResponseHeader 校验 OPN 响应 AsymmetricSecurityHeader 与本端
// 协商策略的一致性（v0.28.0）。
//   - None：策略 URI 精确匹配 + 证书字段必须为空（PRT-08 原语义与文案）。
//   - Basic256Sha256：策略 URI 匹配；服务端证书与本端 pin 的
//     opts.ServerCert 逐字节一致；ReceiverCertificateThumbprint 等于本端
//     客户端证书 SHA-1 指纹。
func validateOPNResponseHeader(asym AsymmetricSecurityHeader, policy string, opts OpenSecureChannelOptions) error {
	if policy == SecurityPolicyBasic256Sha256URI {
		if asym.SecurityPolicyURI != SecurityPolicyBasic256Sha256URI {
			return fmt.Errorf("opcua: OPN 响应策略 URI 不符: %q (want %q)", asym.SecurityPolicyURI, SecurityPolicyBasic256Sha256URI)
		}
		if opts.ServerCert == nil || opts.ClientCert == nil {
			return errors.New("opcua: Basic256Sha256 响应校验需要 pin 的服务端/客户端证书")
		}
		if !bytes.Equal(asym.SenderCertificate, opts.ServerCert.Raw) {
			return errors.New("opcua: OPN 响应服务端证书与 pin 不符")
		}
		if want := SHA1ThumbprintOfCert(opts.ClientCert); !bytes.Equal(asym.ReceiverCertificateThumbprint, want) {
			return errors.New("opcua: OPN 响应 ReceiverCertificateThumbprint 与本端客户端证书指纹不符")
		}
		return nil
	}
	// None（v0.27.0 原路径）。
	if asym.SecurityPolicyURI != SecurityPolicyNoneURI {
		return fmt.Errorf("opcua: OPN 响应策略 URI 不符: %q (want %q)", asym.SecurityPolicyURI, SecurityPolicyNoneURI)
	}
	if len(asym.SenderCertificate) != 0 || len(asym.ReceiverCertificateThumbprint) != 0 {
		return fmt.Errorf("opcua: None 策略下 OPN 响应携带非空证书字段（sender=%d bytes, thumbprint=%d bytes）",
			len(asym.SenderCertificate), len(asym.ReceiverCertificateThumbprint))
	}
	return nil
}

// sendCLO 发送 CloseSecureChannel 请求（AsymmetricSecurityHeader +
// SequenceHeader，无其他 body 字段）。v0.28.0：沿用协商策略与证书字段。
// v0.28.1：Basic256Sha256 下 CLO 同样按本端签名能力发送（体不加密，
// 与 v0.28.0 行为一致；MSG/CLO 对称覆盖留 v0.29.0）。
func (sc *SecureChannel) sendCLO() error {
	policy := sc.policy
	if policy == "" {
		policy = SecurityPolicyNoneURI
	}
	hdr := AsymmetricSecurityHeader{SecurityPolicyURI: policy}
	if policy == SecurityPolicyBasic256Sha256URI && sc.opts.ClientCert != nil && sc.opts.ServerCert != nil {
		hdr.SenderCertificate = sc.opts.ClientCert.Raw
		hdr.ReceiverCertificateThumbprint = SHA1ThumbprintOfCert(sc.opts.ServerCert)
	}
	var e encoder
	if err := hdr.encodeUA(&e); err != nil {
		return err
	}
	_ = sc.nextReqID()
	if err := (SequenceHeader{SequenceNumber: nextSeq(&sc.seq), RequestID: sc.reqId}).encodeUA(&e); err != nil {
		return err
	}
	return sc.conn.WriteMessage(MsgCloseSecureChannel, e.buf)
}

// Close 发送 CLO 并关闭底层 TCP 连接。
func (sc *SecureChannel) Close() error {
	_ = sc.sendCLO()
	return sc.conn.Close()
}

// nextSeq 返回递增后的出站 SequenceNumber。
func nextSeq(seq *uint32) uint32 {
	*seq++
	return *seq
}
