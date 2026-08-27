package opcua

import (
	"crypto/rand"
	"fmt"
	"time"
)

// DefaultClientTimeout 是 Client 单次操作超时（连接 + 握手 + 服务调用）。
const DefaultClientTimeout = 5 * time.Second

// 会话常量（CreateSession 请求参数）。
const (
	defaultSessionTimeout = 3600000.0 // 请求的会话超时（毫秒）
	clientApplicationURI  = "urn:edgeflow:client"
	clientProductURI      = "urn:edgeflow"
)

// Client 是 OPC-UA 客户端（SecurityPolicy None，匿名会话）。
//
// Open 完成完整握手：Dial（HEL/ACK）→ OpenSecureChannel（OPN）→
// CreateSession → ActivateSession（匿名）。之后 Read/Write 走
// 已建立的 MSG 服务通道，Close 依次关闭会话与通道。
type Client struct {
	sc       *SecureChannel
	endpoint string
	timeout  time.Duration
	authTok  NodeId // ActivateSession 后的会话令牌
}

// Open 连接指定端点并完成全链路握手，返回就绪客户端。
func Open(endpoint string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = DefaultClientTimeout
	}
	conn, err := Dial(endpoint)
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = conn.Close() }
	sc, err := conn.OpenSecureChannel(timeout)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("opcua: OpenSecureChannel: %w", err)
	}
	c := &Client{sc: sc, endpoint: endpoint, timeout: timeout}
	if err := c.createSession(); err != nil {
		_ = sc.Close()
		return nil, err
	}
	if err := c.activateSession(); err != nil {
		_ = sc.Close()
		return nil, err
	}
	return c, nil
}

// createSession 发送 CreateSession 请求并解析响应。
func (c *Client) createSession() error {
	var e encoder
	req := CreateSessionRequest{
		RequestHeader: RequestHeader{
			Timestamp:     DateTimeFromTime(time.Now()),
			RequestHandle: c.sc.nextReqID(),
		},
		ClientDescription: ApplicationDescription{
			ApplicationUri:  clientApplicationURI,
			ProductUri:      clientProductURI,
			ApplicationName: NewLocalizedText("EdgeFlow"),
			ApplicationType: 1, // Client
		},
		ServerUri:               c.endpoint,
		EndpointUrl:             c.endpoint,
		SessionName:             fmt.Sprintf("edgeflow-%d", time.Now().UnixNano()),
		ClientNonce:             randomNonce(32),
		RequestedSessionTimeout: defaultSessionTimeout,
	}
	if err := req.encodeUA(&e); err != nil {
		return err
	}
	reqID, err := c.sc.sendSecure(e.buf)
	if err != nil {
		return fmt.Errorf("opcua: send CreateSession: %w", err)
	}
	body, err := c.sc.recvSecure(reqID, c.timeout)
	if err != nil {
		return err
	}
	var d decoder
	d.b = body
	resp, err := decodeCreateSessionResponse(&d)
	if err != nil {
		return fmt.Errorf("opcua: decode CreateSession response: %w", err)
	}
	if !resp.ServiceResult.IsGood() {
		return fmt.Errorf("opcua: CreateSession 服务失败: %s", resp.ServiceResult)
	}
	c.authTok = resp.AuthenticationToken
	return nil
}

// activateSession 使用匿名身份激活会话。
func (c *Client) activateSession() error {
	var e encoder
	req := ActivateSessionRequest{
		RequestHeader: RequestHeader{
			AuthenticationToken: c.authTok,
			Timestamp:           DateTimeFromTime(time.Now()),
			RequestHandle:       c.sc.nextReqID(),
		},
		LocaleIds:         []string{"en"},
		UserIdentityToken: AnonymousIdentityToken(),
	}
	if err := req.encodeUA(&e); err != nil {
		return err
	}
	reqID, err := c.sc.sendSecure(e.buf)
	if err != nil {
		return fmt.Errorf("opcua: send ActivateSession: %w", err)
	}
	body, err := c.sc.recvSecure(reqID, c.timeout)
	if err != nil {
		return err
	}
	var d decoder
	d.b = body
	resp, err := decodeActivateSessionResponse(&d)
	if err != nil {
		return fmt.Errorf("opcua: decode ActivateSession response: %w", err)
	}
	if !resp.ServiceResult.IsGood() {
		return fmt.Errorf("opcua: ActivateSession 服务失败: %s", resp.ServiceResult)
	}
	if len(resp.Results) > 0 && !resp.Results[0].IsGood() {
		return fmt.Errorf("opcua: ActivateSession 身份校验失败: %s", resp.Results[0])
	}
	return nil
}

// Read 批量读取节点属性（AttributeId=Value）。返回的 DataValue 与
// nodes 一一对应（服务端按序）。
func (c *Client) Read(nodes []NodeId) ([]DataValue, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	var e encoder
	toRead := make([]ReadValueId, 0, len(nodes))
	for _, n := range nodes {
		toRead = append(toRead, ReadValueId{NodeId: n, AttributeId: AttributeIdValue})
	}
	req := ReadRequest{
		RequestHeader: RequestHeader{
			AuthenticationToken: c.authTok,
			Timestamp:           DateTimeFromTime(time.Now()),
			RequestHandle:       c.sc.nextReqID(),
		},
		MaxAge:             0,
		TimestampsToReturn: 2, // Both
		NodesToRead:        toRead,
	}
	if err := req.encodeUA(&e); err != nil {
		return nil, err
	}
	reqID, err := c.sc.sendSecure(e.buf)
	if err != nil {
		return nil, fmt.Errorf("opcua: send Read: %w", err)
	}
	body, err := c.sc.recvSecure(reqID, c.timeout)
	if err != nil {
		return nil, err
	}
	var d decoder
	d.b = body
	resp, err := decodeReadResponse(&d)
	if err != nil {
		return nil, fmt.Errorf("opcua: decode Read response: %w", err)
	}
	if !resp.ServiceResult.IsGood() {
		return nil, fmt.Errorf("opcua: Read 服务失败: %s", resp.ServiceResult)
	}
	return resp.Results, nil
}

// Write 写单个节点属性（AttributeId=Value）。返回服务端结果状态码
// （Good 表示写入被接受）。
func (c *Client) Write(node NodeId, v Variant) (StatusCode, error) {
	var e encoder
	req := WriteRequest{
		RequestHeader: RequestHeader{
			AuthenticationToken: c.authTok,
			Timestamp:           DateTimeFromTime(time.Now()),
			RequestHandle:       c.sc.nextReqID(),
		},
		NodesToWrite: []WriteValue{{
			NodeId:      node,
			AttributeId: AttributeIdValue,
			Value: DataValue{
				Value:  &v,
				Status: statusPtr(0),
			},
		}},
	}
	if err := req.encodeUA(&e); err != nil {
		return 0, err
	}
	reqID, err := c.sc.sendSecure(e.buf)
	if err != nil {
		return 0, fmt.Errorf("opcua: send Write: %w", err)
	}
	body, err := c.sc.recvSecure(reqID, c.timeout)
	if err != nil {
		return 0, err
	}
	var d decoder
	d.b = body
	resp, err := decodeWriteResponse(&d)
	if err != nil {
		return 0, fmt.Errorf("opcua: decode Write response: %w", err)
	}
	if !resp.ServiceResult.IsGood() {
		return 0, fmt.Errorf("opcua: Write 服务失败: %s", resp.ServiceResult)
	}
	if len(resp.Results) == 0 {
		return 0, fmt.Errorf("opcua: Write 响应缺少 Results")
	}
	return resp.Results[0], nil
}

// Close 关闭会话与通道：CloseSession → CloseSecureChannel → TCP 关闭。
func (c *Client) Close() error {
	if c.sc == nil {
		return nil
	}
	var e encoder
	req := CloseSessionRequest{
		RequestHeader: RequestHeader{
			AuthenticationToken: c.authTok,
			Timestamp:           DateTimeFromTime(time.Now()),
			RequestHandle:       c.sc.nextReqID(),
		},
		DeleteSubscriptions: true,
	}
	if err := req.encodeUA(&e); err == nil {
		if reqID, err := c.sc.sendSecure(e.buf); err == nil {
			_, _ = c.sc.recvSecure(reqID, c.timeout)
		}
	}
	return c.sc.Close()
}

// randomNonce 生成指定长度的随机字节（32 字节客户端 nonce）。
func randomNonce(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见；退化为时间戳派生（仅影响 nonce 熵）
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (i * 8 % 63))
		}
	}
	return b
}

func statusPtr(s uint32) *StatusCode {
	st := StatusCode(s)
	return &st
}
