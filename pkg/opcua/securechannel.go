package opcua

import (
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
}

// MaxEndpointUrlLength 是 Hello.EndpointUrl 的长度上限（PRT-15）。
// 服务端一般限 4096；超长 URL 视为构造异常，在编码前拒绝。
const MaxEndpointUrlLength = 4096

// OpenSecureChannel 在已握手的 Conn 上执行 OPN 并返回就绪通道。
// OPN 成功后 conn.channelId 被写入，后续 WriteMessage/ReadMessage
// 自动带上真实 ChannelId（既有导出 API 零变更）。
func (c *Conn) OpenSecureChannel(timeout time.Duration) (*SecureChannel, error) {
	sc := &SecureChannel{conn: c}
	if err := sc.sendOPN(timeout); err != nil {
		return nil, err
	}
	if err := sc.recvOPN(timeout); err != nil {
		return nil, err
	}
	c.channelId = sc.channelId
	return sc, nil
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
func (sc *SecureChannel) sendOPN(timeout time.Duration) error {
	if timeout > 0 {
		if err := sc.conn.netConn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		defer func() { _ = sc.conn.netConn.SetDeadline(time.Time{}) }()
	}
	var e encoder
	if err := (AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI}).encodeUA(&e); err != nil {
		return err
	}
	reqID := sc.nextReqID()
	if err := (SequenceHeader{SequenceNumber: nextSeq(&sc.seq), RequestID: reqID}).encodeUA(&e); err != nil {
		return err
	}
	if err := (OpenSecureChannelRequest{
		ClientProtocolVersion: 0,
		RequestType:           SecurityTokenRequestTypeIssue,
		RequestedLifetime:     DefaultRequestedLifetime,
	}).encodeUA(&e); err != nil {
		return err
	}
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
	// PRT-08：策略 URI 必须精确匹配 None（客户端仅声明支持 None）。
	if asym.SecurityPolicyURI != SecurityPolicyNoneURI {
		return fmt.Errorf("opcua: OPN 响应策略 URI 不符: %q (want %q)", asym.SecurityPolicyURI, SecurityPolicyNoneURI)
	}
	// PRT-08：None 策略下证书字段必须为空，非空即中间人/异常服务器。
	if len(asym.SenderCertificate) != 0 || len(asym.ReceiverCertificateThumbprint) != 0 {
		return fmt.Errorf("opcua: None 策略下 OPN 响应携带非空证书字段（sender=%d bytes, thumbprint=%d bytes）",
			len(asym.SenderCertificate), len(asym.ReceiverCertificateThumbprint))
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

// sendCLO 发送 CloseSecureChannel 请求（AsymmetricSecurityHeader +
// SequenceHeader，无其他 body 字段）。
func (sc *SecureChannel) sendCLO() error {
	var e encoder
	if err := (AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI}).encodeUA(&e); err != nil {
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
