package opcua

import (
	"fmt"
)

// ---------------------------------------------------------------------
// 服务消息层（OPC UA Part 4 "Services"）：CreateSession / ActivateSession
// / Read / Write / CloseSession 的请求/响应结构与 UA Binary 编解码。
//
// 全部构建于既有类型（NodeId / Variant / DataValue / LocalizedText /
// ExtensionObject / String / DateTime）之上，无新增 UA 原语类型。
// 服务消息 body 直接是结构编码（UA Binary 中服务请求/响应不带
// TypeId 前缀——TypeId 是概念上的服务节点，不落线上）。
// ---------------------------------------------------------------------

// ServiceTypeId 是服务消息的概念节点标识（Part 4 标准 ObjectId，
// 仅文档/诊断用途，不参与线上编码）。
const (
	ServiceCreateSessionRequest    uint32 = 461
	ServiceCreateSessionResponse   uint32 = 462
	ServiceActivateSessionRequest  uint32 = 465
	ServiceActivateSessionResponse uint32 = 466
	ServiceCloseSessionRequest     uint32 = 471
	ServiceCloseSessionResponse    uint32 = 472
	ServiceReadRequest             uint32 = 631
	ServiceReadResponse            uint32 = 632
	ServiceWriteRequest            uint32 = 673
	ServiceWriteResponse           uint32 = 674
	ServiceAnonymousIdentity       uint32 = 319
)

// AttributeIdValue 是 AttributeId 的 Value 属性标识（Part 4 §8.15）。
const AttributeIdValue uint32 = 13

// 服务结果常用 StatusCode（Good / BadNodeIdUnknown 等由 StatusCode
// 常量表提供；此处补充服务层专用）。
const (
	// StatusBadNodeIdUnknown 表示引用的节点不存在（0x80340000）。
	StatusBadNodeIdUnknown StatusCode = 0x80340000
	// StatusBadAttributeIdInvalid 表示属性标识无效。
	StatusBadAttributeIdInvalid StatusCode = 0x80350000
	// StatusBadNothingToDo 表示无操作内容。
	StatusBadNothingToDo StatusCode = 0x800F0000
)

// ---------------------------------------------------------------------
// 通用头（Part 4 §7.27/§7.28）
// ---------------------------------------------------------------------

// RequestHeader 是每个服务请求的前缀头（Part 4 §7.27）。
type RequestHeader struct {
	AuthenticationToken NodeId
	Timestamp           DateTime
	RequestHandle       uint32
	ReturnDiagnostics   uint32
	AuditEntryId        string
	TimeoutHint         uint32
	AdditionalHeader    ExtensionObject // 空：TypeId=NodeId(0),Encoding=0
}

func (h RequestHeader) encodeUA(e *encoder) error {
	if err := h.AuthenticationToken.encodeUA(e); err != nil {
		return err
	}
	e.i64(int64(h.Timestamp))
	e.u32(h.RequestHandle)
	e.u32(h.ReturnDiagnostics)
	e.str(h.AuditEntryId)
	e.u32(h.TimeoutHint)
	return h.AdditionalHeader.encodeUA(e)
}

func decodeRequestHeader(d *decoder) (RequestHeader, error) {
	var h RequestHeader
	var err error
	if h.AuthenticationToken, err = decodeNodeID(d); err != nil {
		return h, err
	}
	if v, err := d.i64(); err != nil {
		return h, err
	} else {
		h.Timestamp = DateTime(v)
	}
	if h.RequestHandle, err = d.u32(); err != nil {
		return h, err
	}
	if h.ReturnDiagnostics, err = d.u32(); err != nil {
		return h, err
	}
	if h.AuditEntryId, err = d.str(); err != nil {
		return h, err
	}
	if h.TimeoutHint, err = d.u32(); err != nil {
		return h, err
	}
	if h.AdditionalHeader, err = decodeExtensionObject(d); err != nil {
		return h, err
	}
	return h, nil
}

// ResponseHeader 是每个服务响应的前缀头（Part 4 §7.28）。
// ServiceDiagnostics 依赖 DiagnosticInfo 位域解码（diagnostic.go）。
type ResponseHeader struct {
	Timestamp          DateTime
	RequestHandle      uint32
	ServiceResult      StatusCode
	ServiceDiagnostics DiagnosticInfo
	StringTable        []string
	AdditionalHeader   ExtensionObject
}

func (h ResponseHeader) encodeUA(e *encoder) error {
	e.i64(int64(h.Timestamp))
	e.u32(h.RequestHandle)
	e.u32(uint32(h.ServiceResult))
	if err := h.ServiceDiagnostics.encodeUA(e); err != nil {
		return err
	}
	encodeStringList(e, h.StringTable)
	return h.AdditionalHeader.encodeUA(e)
}

func decodeResponseHeader(d *decoder) (ResponseHeader, error) {
	var h ResponseHeader
	var err error
	if v, err := d.i64(); err != nil {
		return h, err
	} else {
		h.Timestamp = DateTime(v)
	}
	if h.RequestHandle, err = d.u32(); err != nil {
		return h, err
	}
	if v, err := d.u32(); err != nil {
		return h, err
	} else {
		h.ServiceResult = StatusCode(v)
	}
	if h.ServiceDiagnostics, err = decodeDiagnosticInfo(d, 0); err != nil {
		return h, err
	}
	if h.StringTable, err = decodeStringList(d); err != nil {
		return h, err
	}
	if h.AdditionalHeader, err = decodeExtensionObject(d); err != nil {
		return h, err
	}
	return h, nil
}

func encodeStringList(e *encoder, list []string) {
	if list == nil {
		e.i32(-1)
		return
	}
	e.i32(int32(len(list)))
	for _, s := range list {
		e.str(s)
	}
}

func decodeStringList(d *decoder) ([]string, error) {
	n, err := d.i32()
	if err != nil {
		return nil, err
	}
	if n == -1 {
		return nil, nil
	}
	if n < 0 {
		return nil, fmt.Errorf("%w: negative String array length %d", ErrInvalidEncoding, n)
	}
	if n > MaxArrayLength {
		return nil, fmt.Errorf("%w: String array length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
	}
	// PRT-14：分配前校验——声明长度 × 每元素最小字节数（长度定界符
	// 4B）超过剩余缓冲且元素数超过盲分配豁免阈值（同 PRT-01 的
	// maxUncheckedArrayElems）时，判定恶意/损坏报文，拒绝按声明长度
	// 预分配；小规模声明不拦截，截断语义与既有行为一致。
	if n > maxUncheckedArrayElems {
		if rem := len(d.b) - d.off; int64(n)*4 > int64(rem) {
			return nil, fmt.Errorf("%w: String array length %d needs ≥%d bytes, only %d remain", ErrTooLong, n, n*4, rem)
		}
	}
	out := make([]string, 0, n)
	for i := int32(0); i < n; i++ {
		s, err := d.str()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ---------------------------------------------------------------------
// 复合类型（Part 4 §7）
// ---------------------------------------------------------------------

// ApplicationDescription 描述一个应用（Part 4 §7.1）。
type ApplicationDescription struct {
	ApplicationUri      string
	ProductUri          string
	ApplicationName     LocalizedText
	ApplicationType     uint32 // 0=Server 1=Client 2=ClientAndServer 3=DiscoveryServer
	GatewayServerUri    string
	DiscoveryProfileUri string
	DiscoveryUrls       []string
}

func (a ApplicationDescription) encodeUA(e *encoder) error {
	e.str(a.ApplicationUri)
	e.str(a.ProductUri)
	a.ApplicationName.encodeUA(e) // LocalizedText.encodeUA 无返回值
	e.u32(a.ApplicationType)
	e.str(a.GatewayServerUri)
	e.str(a.DiscoveryProfileUri)
	encodeStringList(e, a.DiscoveryUrls)
	return nil
}

func decodeApplicationDescription(d *decoder) (ApplicationDescription, error) {
	var a ApplicationDescription
	var err error
	if a.ApplicationUri, err = d.str(); err != nil {
		return a, err
	}
	if a.ProductUri, err = d.str(); err != nil {
		return a, err
	}
	if a.ApplicationName, err = decodeLocalizedText(d); err != nil {
		return a, err
	}
	if a.ApplicationType, err = d.u32(); err != nil {
		return a, err
	}
	if a.GatewayServerUri, err = d.str(); err != nil {
		return a, err
	}
	if a.DiscoveryProfileUri, err = d.str(); err != nil {
		return a, err
	}
	if a.DiscoveryUrls, err = decodeStringList(d); err != nil {
		return a, err
	}
	return a, nil
}

// UserTokenPolicy 描述服务器支持的用户身份令牌策略（Part 4 §7.36）。
type UserTokenPolicy struct {
	PolicyId          string
	TokenType         uint32 // 0=Anonymous 1=UserName 2=Certificate 3=IssuedToken
	IssuedTokenType   string
	IssuerEndpointUrl string
	SecurityPolicyUri string
}

func (p UserTokenPolicy) encodeUA(e *encoder) error {
	e.str(p.PolicyId)
	e.u32(p.TokenType)
	e.str(p.IssuedTokenType)
	e.str(p.IssuerEndpointUrl)
	e.str(p.SecurityPolicyUri)
	return nil
}

func decodeUserTokenPolicy(d *decoder) (UserTokenPolicy, error) {
	var p UserTokenPolicy
	var err error
	if p.PolicyId, err = d.str(); err != nil {
		return p, err
	}
	if p.TokenType, err = d.u32(); err != nil {
		return p, err
	}
	if p.IssuedTokenType, err = d.str(); err != nil {
		return p, err
	}
	if p.IssuerEndpointUrl, err = d.str(); err != nil {
		return p, err
	}
	if p.SecurityPolicyUri, err = d.str(); err != nil {
		return p, err
	}
	return p, nil
}

// EndpointDescription 描述一个服务端点（Part 4 §7.10）。
type EndpointDescription struct {
	EndpointUrl         string
	Server              ApplicationDescription
	ServerCertificate   []byte
	SecurityMode        uint32 // 1=None 2=Sign 3=SignAndEncrypt
	SecurityPolicyUri   string
	UserIdentityTokens  []UserTokenPolicy
	TransportProfileUri string
	SecurityLevel       byte
}

func (e EndpointDescription) encodeUA(enc *encoder) error {
	enc.str(e.EndpointUrl)
	if err := e.Server.encodeUA(enc); err != nil {
		return err
	}
	enc.bytes(e.ServerCertificate)
	enc.u32(e.SecurityMode)
	enc.str(e.SecurityPolicyUri)
	if e.UserIdentityTokens == nil {
		enc.i32(-1)
	} else {
		enc.i32(int32(len(e.UserIdentityTokens)))
		for i := range e.UserIdentityTokens {
			if err := e.UserIdentityTokens[i].encodeUA(enc); err != nil {
				return err
			}
		}
	}
	enc.str(e.TransportProfileUri)
	enc.u8(e.SecurityLevel)
	return nil
}

func decodeEndpointDescription(d *decoder) (EndpointDescription, error) {
	var e EndpointDescription
	var err error
	if e.EndpointUrl, err = d.str(); err != nil {
		return e, err
	}
	if e.Server, err = decodeApplicationDescription(d); err != nil {
		return e, err
	}
	if e.ServerCertificate, err = d.bytes(); err != nil {
		return e, err
	}
	if e.SecurityMode, err = d.u32(); err != nil {
		return e, err
	}
	if e.SecurityPolicyUri, err = d.str(); err != nil {
		return e, err
	}
	n, err := d.i32()
	if err != nil {
		return e, err
	}
	if n >= 0 {
		if n > MaxArrayLength {
			return e, fmt.Errorf("%w: UserTokenPolicy array length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
		}
		for i := int32(0); i < n; i++ {
			p, err := decodeUserTokenPolicy(d)
			if err != nil {
				return e, err
			}
			e.UserIdentityTokens = append(e.UserIdentityTokens, p)
		}
	}
	if e.TransportProfileUri, err = d.str(); err != nil {
		return e, err
	}
	if e.SecurityLevel, err = d.u8(); err != nil {
		return e, err
	}
	return e, nil
}

// SignedSoftwareCertificate 是带签名的软件证书（Part 4 §7.26）。
type SignedSoftwareCertificate struct {
	CertificateData []byte
	Signature       []byte
}

func (c SignedSoftwareCertificate) encodeUA(e *encoder) error {
	e.bytes(c.CertificateData)
	e.bytes(c.Signature)
	return nil
}

func decodeSignedSoftwareCertificate(d *decoder) (SignedSoftwareCertificate, error) {
	var c SignedSoftwareCertificate
	var err error
	if c.CertificateData, err = d.bytes(); err != nil {
		return c, err
	}
	if c.Signature, err = d.bytes(); err != nil {
		return c, err
	}
	return c, nil
}

// SignatureData 是签名数据（Part 4 §7.31）。
type SignatureData struct {
	Algorithm string
	Signature []byte
}

func (s SignatureData) encodeUA(e *encoder) error {
	e.str(s.Algorithm)
	e.bytes(s.Signature)
	return nil
}

func decodeSignatureData(d *decoder) (SignatureData, error) {
	var s SignatureData
	var err error
	if s.Algorithm, err = d.str(); err != nil {
		return s, err
	}
	if s.Signature, err = d.bytes(); err != nil {
		return s, err
	}
	return s, nil
}

// AnonymousIdentityToken 构造匿名身份令牌（Part 4 §7.36.2）：ExtensionObject
// TypeId=NodeId(0,319)，Encoding=ByteString，body = UA String("Anonymous")
// 的字节。
func AnonymousIdentityToken() ExtensionObject {
	var e encoder
	e.str("Anonymous")
	return ExtensionObject{
		TypeId:   NewNodeID(0, ServiceAnonymousIdentity),
		Encoding: ExtensionObjectEncodingByteString,
		Body:     e.buf,
	}
}

// ---------------------------------------------------------------------
// CreateSession（Part 4 §5.4.2）
// ---------------------------------------------------------------------

// CreateSessionRequest 建立会话（TypeId 461）。
type CreateSessionRequest struct {
	RequestHeader
	ClientDescription       ApplicationDescription
	ServerUri               string
	EndpointUrl             string
	SessionName             string
	ClientNonce             []byte
	ClientCertificate       []byte
	RequestedSessionTimeout float64
	MaxResponseMessageSize  uint32
}

func (r CreateSessionRequest) encodeUA(e *encoder) error {
	if err := r.RequestHeader.encodeUA(e); err != nil {
		return err
	}
	if err := r.ClientDescription.encodeUA(e); err != nil {
		return err
	}
	e.str(r.ServerUri)
	e.str(r.EndpointUrl)
	e.str(r.SessionName)
	e.bytes(r.ClientNonce)
	e.bytes(r.ClientCertificate)
	e.f64(r.RequestedSessionTimeout)
	e.u32(r.MaxResponseMessageSize)
	return nil
}

func decodeCreateSessionRequest(d *decoder) (CreateSessionRequest, error) {
	var r CreateSessionRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	if r.ClientDescription, err = decodeApplicationDescription(d); err != nil {
		return r, err
	}
	if r.ServerUri, err = d.str(); err != nil {
		return r, err
	}
	if r.EndpointUrl, err = d.str(); err != nil {
		return r, err
	}
	if r.SessionName, err = d.str(); err != nil {
		return r, err
	}
	if r.ClientNonce, err = d.bytes(); err != nil {
		return r, err
	}
	if r.ClientCertificate, err = d.bytes(); err != nil {
		return r, err
	}
	if r.RequestedSessionTimeout, err = d.f64(); err != nil {
		return r, err
	}
	if r.MaxResponseMessageSize, err = d.u32(); err != nil {
		return r, err
	}
	return r, nil
}

// CreateSessionResponse 返回会话与端点信息（TypeId 462）。
type CreateSessionResponse struct {
	ResponseHeader
	SessionId                  NodeId
	AuthenticationToken        NodeId
	RevisedSessionTimeout      float64
	ServerNonce                []byte
	ServerCertificate          []byte
	ServerEndpoints            []EndpointDescription
	ServerSoftwareCertificates []SignedSoftwareCertificate
	ServerSignature            SignatureData
	MaxRequestMessageSize      uint32
}

func (r CreateSessionResponse) encodeUA(e *encoder) error {
	if err := r.ResponseHeader.encodeUA(e); err != nil {
		return err
	}
	if err := r.SessionId.encodeUA(e); err != nil {
		return err
	}
	if err := r.AuthenticationToken.encodeUA(e); err != nil {
		return err
	}
	e.f64(r.RevisedSessionTimeout)
	e.bytes(r.ServerNonce)
	e.bytes(r.ServerCertificate)
	if r.ServerEndpoints == nil {
		e.i32(-1)
	} else {
		e.i32(int32(len(r.ServerEndpoints)))
		for i := range r.ServerEndpoints {
			if err := r.ServerEndpoints[i].encodeUA(e); err != nil {
				return err
			}
		}
	}
	if r.ServerSoftwareCertificates == nil {
		e.i32(-1)
	} else {
		e.i32(int32(len(r.ServerSoftwareCertificates)))
		for i := range r.ServerSoftwareCertificates {
			if err := r.ServerSoftwareCertificates[i].encodeUA(e); err != nil {
				return err
			}
		}
	}
	if err := r.ServerSignature.encodeUA(e); err != nil {
		return err
	}
	e.u32(r.MaxRequestMessageSize)
	return nil
}

func decodeCreateSessionResponse(d *decoder) (CreateSessionResponse, error) {
	var r CreateSessionResponse
	var err error
	if r.ResponseHeader, err = decodeResponseHeader(d); err != nil {
		return r, err
	}
	if r.SessionId, err = decodeNodeID(d); err != nil {
		return r, err
	}
	if r.AuthenticationToken, err = decodeNodeID(d); err != nil {
		return r, err
	}
	if r.RevisedSessionTimeout, err = d.f64(); err != nil {
		return r, err
	}
	if r.ServerNonce, err = d.bytes(); err != nil {
		return r, err
	}
	if r.ServerCertificate, err = d.bytes(); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 {
		if n > MaxArrayLength {
			return r, fmt.Errorf("%w: EndpointDescription array length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
		}
		for i := int32(0); i < n; i++ {
			ep, err := decodeEndpointDescription(d)
			if err != nil {
				return r, err
			}
			r.ServerEndpoints = append(r.ServerEndpoints, ep)
		}
	}
	n2, err := d.i32()
	if err != nil {
		return r, err
	}
	if n2 >= 0 {
		if n2 > MaxArrayLength {
			return r, fmt.Errorf("%w: SignedSoftwareCertificate array length %d exceeds limit %d", ErrTooLong, n2, MaxArrayLength)
		}
		for i := int32(0); i < n2; i++ {
			c, err := decodeSignedSoftwareCertificate(d)
			if err != nil {
				return r, err
			}
			r.ServerSoftwareCertificates = append(r.ServerSoftwareCertificates, c)
		}
	}
	if r.ServerSignature, err = decodeSignatureData(d); err != nil {
		return r, err
	}
	if r.MaxRequestMessageSize, err = d.u32(); err != nil {
		return r, err
	}
	return r, nil
}

// ---------------------------------------------------------------------
// ActivateSession（Part 4 §5.4.3）
// ---------------------------------------------------------------------

// ActivateSessionRequest 激活会话（TypeId 465，匿名身份）。
type ActivateSessionRequest struct {
	RequestHeader
	ClientSignature            SignatureData
	ClientSoftwareCertificates []SignedSoftwareCertificate
	LocaleIds                  []string
	UserIdentityToken          ExtensionObject // AnonymousIdentityToken
	UserTokenSignature         SignatureData
}

func (r ActivateSessionRequest) encodeUA(e *encoder) error {
	if err := r.RequestHeader.encodeUA(e); err != nil {
		return err
	}
	if err := r.ClientSignature.encodeUA(e); err != nil {
		return err
	}
	if r.ClientSoftwareCertificates == nil {
		e.i32(-1)
	} else {
		e.i32(int32(len(r.ClientSoftwareCertificates)))
		for i := range r.ClientSoftwareCertificates {
			if err := r.ClientSoftwareCertificates[i].encodeUA(e); err != nil {
				return err
			}
		}
	}
	encodeStringList(e, r.LocaleIds)
	if err := r.UserIdentityToken.encodeUA(e); err != nil {
		return err
	}
	return r.UserTokenSignature.encodeUA(e)
}

func decodeActivateSessionRequest(d *decoder) (ActivateSessionRequest, error) {
	var r ActivateSessionRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	if r.ClientSignature, err = decodeSignatureData(d); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 {
		if n > MaxArrayLength {
			return r, fmt.Errorf("%w: SignedSoftwareCertificate array length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
		}
		for i := int32(0); i < n; i++ {
			c, err := decodeSignedSoftwareCertificate(d)
			if err != nil {
				return r, err
			}
			r.ClientSoftwareCertificates = append(r.ClientSoftwareCertificates, c)
		}
	}
	if r.LocaleIds, err = decodeStringList(d); err != nil {
		return r, err
	}
	if r.UserIdentityToken, err = decodeExtensionObject(d); err != nil {
		return r, err
	}
	if r.UserTokenSignature, err = decodeSignatureData(d); err != nil {
		return r, err
	}
	return r, nil
}

// ActivateSessionResponse 返回会话激活结果（TypeId 466）。
type ActivateSessionResponse struct {
	ResponseHeader
	ServerNonce     []byte
	Results         []StatusCode
	DiagnosticInfos []DiagnosticInfo
}

func (r ActivateSessionResponse) encodeUA(e *encoder) error {
	if err := r.ResponseHeader.encodeUA(e); err != nil {
		return err
	}
	e.bytes(r.ServerNonce)
	if r.Results == nil {
		e.i32(-1)
	} else {
		e.i32(int32(len(r.Results)))
		for _, s := range r.Results {
			e.u32(uint32(s))
		}
	}
	encodeDiagnosticInfoList(e, r.DiagnosticInfos)
	return nil
}

func decodeActivateSessionResponse(d *decoder) (ActivateSessionResponse, error) {
	var r ActivateSessionResponse
	var err error
	if r.ResponseHeader, err = decodeResponseHeader(d); err != nil {
		return r, err
	}
	if r.ServerNonce, err = d.bytes(); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 {
		if n > MaxArrayLength {
			return r, fmt.Errorf("%w: StatusCode array length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
		}
		for i := int32(0); i < n; i++ {
			v, err := d.u32()
			if err != nil {
				return r, err
			}
			r.Results = append(r.Results, StatusCode(v))
		}
	}
	if r.DiagnosticInfos, err = decodeDiagnosticInfoList(d); err != nil {
		return r, err
	}
	return r, nil
}

// ---------------------------------------------------------------------
// CloseSession（Part 4 §5.4.4）
// ---------------------------------------------------------------------

// CloseSessionRequest 关闭会话（TypeId 471）。
type CloseSessionRequest struct {
	RequestHeader
	DeleteSubscriptions bool
}

func (r CloseSessionRequest) encodeUA(e *encoder) error {
	if err := r.RequestHeader.encodeUA(e); err != nil {
		return err
	}
	e.bool(r.DeleteSubscriptions)
	return nil
}

func decodeCloseSessionRequest(d *decoder) (CloseSessionRequest, error) {
	var r CloseSessionRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	if r.DeleteSubscriptions, err = d.bool(); err != nil {
		return r, err
	}
	return r, nil
}

// CloseSessionResponse 返回会话关闭结果（TypeId 472）。
type CloseSessionResponse struct {
	ResponseHeader
}

func (r CloseSessionResponse) encodeUA(e *encoder) error { return r.ResponseHeader.encodeUA(e) }

func decodeCloseSessionResponse(d *decoder) (CloseSessionResponse, error) {
	var r CloseSessionResponse
	var err error
	if r.ResponseHeader, err = decodeResponseHeader(d); err != nil {
		return r, err
	}
	return r, nil
}

// ---------------------------------------------------------------------
// Read（Part 4 §5.10.2）
// ---------------------------------------------------------------------

// ReadValueId 标识要读取的属性（Part 4 §7.24）。
type ReadValueId struct {
	NodeId       NodeId
	AttributeId  uint32 // 13 = Value
	IndexRange   string
	DataEncoding QualifiedName // 空（Namespace=0,Name=""）
}

func (v ReadValueId) encodeUA(e *encoder) error {
	if err := v.NodeId.encodeUA(e); err != nil {
		return err
	}
	e.u32(v.AttributeId)
	e.str(v.IndexRange)
	v.DataEncoding.encodeUA(e)
	return nil
}

func decodeReadValueId(d *decoder) (ReadValueId, error) {
	var v ReadValueId
	var err error
	if v.NodeId, err = decodeNodeID(d); err != nil {
		return v, err
	}
	if v.AttributeId, err = d.u32(); err != nil {
		return v, err
	}
	if v.IndexRange, err = d.str(); err != nil {
		return v, err
	}
	if v.DataEncoding, err = decodeQualifiedName(d); err != nil {
		return v, err
	}
	return v, nil
}

// ReadRequest 批量读取属性（TypeId 631）。
type ReadRequest struct {
	RequestHeader
	MaxAge             float64
	TimestampsToReturn uint32 // 0=Source 1=Server 2=Both 3=Neither
	NodesToRead        []ReadValueId
}

func (r ReadRequest) encodeUA(e *encoder) error {
	if err := r.RequestHeader.encodeUA(e); err != nil {
		return err
	}
	e.f64(r.MaxAge)
	e.u32(r.TimestampsToReturn)
	if r.NodesToRead == nil {
		e.i32(-1)
		return nil
	}
	e.i32(int32(len(r.NodesToRead)))
	for i := range r.NodesToRead {
		if err := r.NodesToRead[i].encodeUA(e); err != nil {
			return err
		}
	}
	return nil
}

func decodeReadRequest(d *decoder) (ReadRequest, error) {
	var r ReadRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	if r.MaxAge, err = d.f64(); err != nil {
		return r, err
	}
	if r.TimestampsToReturn, err = d.u32(); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 {
		if n > MaxArrayLength {
			return r, fmt.Errorf("%w: ReadValueId array length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
		}
		for i := int32(0); i < n; i++ {
			v, err := decodeReadValueId(d)
			if err != nil {
				return r, err
			}
			r.NodesToRead = append(r.NodesToRead, v)
		}
	}
	return r, nil
}

// ReadResponse 返回读取结果（TypeId 632）。
type ReadResponse struct {
	ResponseHeader
	Results         []DataValue
	DiagnosticInfos []DiagnosticInfo
}

func (r ReadResponse) encodeUA(e *encoder) error {
	if err := r.ResponseHeader.encodeUA(e); err != nil {
		return err
	}
	if r.Results == nil {
		e.i32(-1)
	} else {
		e.i32(int32(len(r.Results)))
		for i := range r.Results {
			if err := r.Results[i].encodeUA(e); err != nil {
				return err
			}
		}
	}
	encodeDiagnosticInfoList(e, r.DiagnosticInfos)
	return nil
}

func decodeReadResponse(d *decoder) (ReadResponse, error) {
	var r ReadResponse
	var err error
	if r.ResponseHeader, err = decodeResponseHeader(d); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 {
		if n > MaxArrayLength {
			return r, fmt.Errorf("%w: DataValue array length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
		}
		for i := int32(0); i < n; i++ {
			dv, err := decodeDataValue(d)
			if err != nil {
				return r, err
			}
			r.Results = append(r.Results, dv)
		}
	}
	if r.DiagnosticInfos, err = decodeDiagnosticInfoList(d); err != nil {
		return r, err
	}
	return r, nil
}

// ---------------------------------------------------------------------
// Write（Part 4 §5.10.4）
// ---------------------------------------------------------------------

// WriteValue 描述一次写操作（Part 4 §7.25）。
type WriteValue struct {
	NodeId      NodeId
	AttributeId uint32 // 13 = Value
	IndexRange  string
	Value       DataValue // Status=Good, 带 Variant
}

func (v WriteValue) encodeUA(e *encoder) error {
	if err := v.NodeId.encodeUA(e); err != nil {
		return err
	}
	e.u32(v.AttributeId)
	e.str(v.IndexRange)
	return v.Value.encodeUA(e)
}

func decodeWriteValue(d *decoder) (WriteValue, error) {
	var v WriteValue
	var err error
	if v.NodeId, err = decodeNodeID(d); err != nil {
		return v, err
	}
	if v.AttributeId, err = d.u32(); err != nil {
		return v, err
	}
	if v.IndexRange, err = d.str(); err != nil {
		return v, err
	}
	if v.Value, err = decodeDataValue(d); err != nil {
		return v, err
	}
	return v, nil
}

// WriteRequest 批量写入属性（TypeId 673）。
type WriteRequest struct {
	RequestHeader
	NodesToWrite []WriteValue
}

func (r WriteRequest) encodeUA(e *encoder) error {
	if err := r.RequestHeader.encodeUA(e); err != nil {
		return err
	}
	if r.NodesToWrite == nil {
		e.i32(-1)
		return nil
	}
	e.i32(int32(len(r.NodesToWrite)))
	for i := range r.NodesToWrite {
		if err := r.NodesToWrite[i].encodeUA(e); err != nil {
			return err
		}
	}
	return nil
}

func decodeWriteRequest(d *decoder) (WriteRequest, error) {
	var r WriteRequest
	var err error
	if r.RequestHeader, err = decodeRequestHeader(d); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 {
		if n > MaxArrayLength {
			return r, fmt.Errorf("%w: WriteValue array length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
		}
		for i := int32(0); i < n; i++ {
			v, err := decodeWriteValue(d)
			if err != nil {
				return r, err
			}
			r.NodesToWrite = append(r.NodesToWrite, v)
		}
	}
	return r, nil
}

// WriteResponse 返回写入结果（TypeId 674）。
type WriteResponse struct {
	ResponseHeader
	Results         []StatusCode
	DiagnosticInfos []DiagnosticInfo
}

func (r WriteResponse) encodeUA(e *encoder) error {
	if err := r.ResponseHeader.encodeUA(e); err != nil {
		return err
	}
	if r.Results == nil {
		e.i32(-1)
	} else {
		e.i32(int32(len(r.Results)))
		for _, s := range r.Results {
			e.u32(uint32(s))
		}
	}
	encodeDiagnosticInfoList(e, r.DiagnosticInfos)
	return nil
}

func decodeWriteResponse(d *decoder) (WriteResponse, error) {
	var r WriteResponse
	var err error
	if r.ResponseHeader, err = decodeResponseHeader(d); err != nil {
		return r, err
	}
	n, err := d.i32()
	if err != nil {
		return r, err
	}
	if n >= 0 {
		if n > MaxArrayLength {
			return r, fmt.Errorf("%w: StatusCode array length %d exceeds limit %d", ErrTooLong, n, MaxArrayLength)
		}
		for i := int32(0); i < n; i++ {
			v, err := d.u32()
			if err != nil {
				return r, err
			}
			r.Results = append(r.Results, StatusCode(v))
		}
	}
	if r.DiagnosticInfos, err = decodeDiagnosticInfoList(d); err != nil {
		return r, err
	}
	return r, nil
}
