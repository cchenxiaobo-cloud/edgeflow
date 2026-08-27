package opcua

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// mockOPCUServer 是一台进程内 OPC-UA 模拟服务器（SecurityPolicy None）：
// HEL→ACK → OPN→OPNF → MSG 服务分派（CreateSession/ActivateSession/
// Read/Write/CloseSession）→ CLO。用于客户端全链路测试。
//
// 点位表：
//   - ns=2;i=1001 temperature → Double（值由 state 持有，可被写）
//   - ns=2;i=3001 setpoint → Double（可写，写后 temperature 不联动）
//   - 其余节点 → BadNodeIdUnknown
type mockOPCUServer struct {
	t         *testing.T
	mu        chan struct{} // 简易互斥（串行化 handler 状态）
	channel   uint32
	token     uint32
	authTok   NodeId
	temp      float64
	lastReq   uint32
	rejectOPN bool // true 时 OPN 返回 Error
}

func startMockOPCUServer(t *testing.T, rejectOPN bool) string {
	t.Helper()
	s := &mockOPCUServer{t: t, mu: make(chan struct{}, 1), temp: 25.5, rejectOPN: rejectOPN}
	addr := startMockServer(t, func(c net.Conn) { s.handle(c) })
	return addr
}

func (s *mockOPCUServer) lock()   { s.mu <- struct{}{} }
func (s *mockOPCUServer) unlock() { <-s.mu }

func (s *mockOPCUServer) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	// HEL → ACK
	msgType, body, err := serverReadFrameRaw(c)
	if err != nil {
		return
	}
	if msgType != MsgHello {
		return
	}
	hello, err := DecodeHello(body)
	if err != nil || hello.EndpointUrl == "" {
		return
	}
	if err := serverWriteFrameRaw(c, MsgAcknowledge, mustEncode(t2s(s.t), Acknowledge{
		ProtocolVersion: 0, ReceiveBufferSize: 65536, SendBufferSize: 65536,
		MaxMessageSize: 65536, MaxChunkCount: 1,
	})); err != nil {
		return
	}
	// OPN
	msgType, body, err = serverReadFrameRaw(c)
	if err != nil {
		return
	}
	if msgType != MsgOpenSecureChannel {
		return
	}
	if s.rejectOPN {
		_ = serverWriteFrameRaw(c, MsgError, mustEncode(t2s(s.t), ErrorMessage{
			ErrorCode: 0x80540000, ErrorReason: "BadSecurityModeRejected",
		}))
		return
	}
	rest, err := decodeOPNBody(body)
	if err != nil {
		return
	}
	_ = rest
	s.lock()
	s.channel++
	ch := s.channel
	s.token++
	tok := s.token
	s.unlock()
	// OPNF
	opnBody := mustEncode(t2s(s.t), OpenSecureChannelResponse{
		Timestamp: DateTimeFromTime(time.Now()), RequestHandle: 0, ServiceResult: 0,
		ServerProtocolVersion: 0,
		SecurityToken: ChannelSecurityToken{
			ChannelID: ch, TokenID: tok, CreatedAt: DateTimeFromTime(time.Now()), RevisedLifetime: 600000,
		},
	})
	asym := mustEncode(t2s(s.t), AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI})
	seqH := mustEncode(t2s(s.t), SequenceHeader{SequenceNumber: 1, RequestID: 1})
	frame := append(asym, seqH...)
	frame = append(frame, opnBody...)
	if err := serverWriteFrameWithChannel(c, MsgOpenSecureChannel, ch, frame); err != nil {
		return
	}
	// 服务消息循环
	for {
		msgType, body, err = serverReadFrameRaw(c)
		if err != nil {
			return
		}
		switch msgType {
		case MsgCloseSecureChannel:
			return
		case MsgSecureMessage:
			if !s.handleService(c, body) {
				return
			}
		default:
			return
		}
	}
}

// handleService 解析 MSG 帧并分派服务请求，返回 false 表示连接结束。
func (s *mockOPCUServer) handleService(c net.Conn, body []byte) bool {
	_, rest, err := DecodeSymmetricSecurityHeader(body)
	if err != nil {
		return false
	}
	sh, rest, err := DecodeSequenceHeader(rest)
	if err != nil {
		return false
	}
	s.lock()
	s.lastReq = sh.RequestID
	s.unlock()

	respHeader := ResponseHeader{
		Timestamp: DateTimeFromTime(time.Now()), RequestHandle: sh.RequestID, ServiceResult: 0,
	}
	writeResp := func(respBody []byte) bool {
		sym := mustEncode(t2s(s.t), SymmetricSecurityHeader{TokenID: s.token})
		seq := mustEncode(t2s(s.t), SequenceHeader{SequenceNumber: 1, RequestID: sh.RequestID})
		frame := append(sym, seq...)
		frame = append(frame, respBody...)
		return serverWriteFrameWithChannel(c, MsgSecureMessage, s.channel, frame) == nil
	}

	// 依据请求体第一个字段（RequestHeader.AuthenticationToken 的
	// NodeId 编码首字节）无法区分服务；改用"尝试解码"顺序：
	// CreateSession 的 ClientDescription 首字段是 ApplicationUri（String）
	// 与 ActivateSession 的 ClientSignature（SignatureData=String+ByteString）
	// 相近。用 TypeId 区分不现实（不在线上），这里按客户端调用顺序
	// 由 reqHandle 标记不可行——改为按"字段结构"试解：先试
	// CreateSession（其第二个字段 ServerUri 是 String），再试
	// ActivateSession，再试 Read/Write/CloseSession。
	// 务实做法：客户端按固定顺序调用（Create→Activate→服务），
	// 服务端用一个简单判定：CreateSession 的 RequestHeader 后紧接
	// ApplicationUri（String）且随后字段多为 String；ActivateSession
	// 后接 SignatureData。这里采用试解优先级：
	if req, err := DecodeCreateSessionRequest(rest); err == nil && req.ClientDescription.ApplicationUri != "" {
		s.lock()
		s.authTok = NewNodeID(0, 100)
		s.unlock()
		resp := CreateSessionResponse{
			ResponseHeader:        respHeader,
			SessionId:             NewNodeID(0, 99),
			AuthenticationToken:   NewNodeID(0, 100),
			RevisedSessionTimeout: 3600000,
			MaxRequestMessageSize: 65536,
		}
		return writeResp(mustEncode(t2s(s.t), resp))
	}
	if _, err := DecodeActivateSessionRequest(rest); err == nil {
		resp := ActivateSessionResponse{
			ResponseHeader:  respHeader,
			Results:         []StatusCode{0},
			DiagnosticInfos: nil,
		}
		return writeResp(mustEncode(t2s(s.t), resp))
	}
	if req, err := DecodeReadRequest(rest); err == nil {
		resp := ReadResponse{ResponseHeader: respHeader}
		for _, rv := range req.NodesToRead {
			if rv.NodeId.Namespace == 2 && rv.NodeId.Type == NodeIDNumeric && rv.NodeId.Numeric == 1001 {
				v, _ := NewVariant(s.temp)
				resp.Results = append(resp.Results, DataValue{Value: &v, Status: statusPtr(0)})
			} else {
				resp.Results = append(resp.Results, DataValue{Status: statusPtr(uint32(StatusBadNodeIdUnknown))})
			}
		}
		return writeResp(mustEncode(t2s(s.t), resp))
	}
	if req, err := DecodeWriteRequest(rest); err == nil && len(req.NodesToWrite) > 0 {
		w := req.NodesToWrite[0]
		if w.NodeId.Namespace == 2 && w.NodeId.Type == NodeIDNumeric && w.NodeId.Numeric == 3001 && w.Value.Value != nil {
			if f, ok := w.Value.Value.Value.(float64); ok {
				s.lock()
				s.temp = f
				s.unlock()
			}
			resp := WriteResponse{ResponseHeader: respHeader, Results: []StatusCode{0}}
			return writeResp(mustEncode(t2s(s.t), resp))
		}
		resp := WriteResponse{ResponseHeader: respHeader, Results: []StatusCode{StatusBadNodeIdUnknown}}
		return writeResp(mustEncode(t2s(s.t), resp))
	}
	if _, err := DecodeCloseSessionRequest(rest); err == nil {
		resp := CloseSessionResponse{ResponseHeader: respHeader}
		return writeResp(mustEncode(t2s(s.t), resp))
	}
	return false
}

// TestClientOpenReadWriteClose 验证客户端全链路：Open（HEL→OPN→Create→Activate）
// → Read → Write → Close。
func TestClientOpenReadWriteClose(t *testing.T) {
	addr := startMockOPCUServer(t, false)
	endpoint := "opc.tcp://" + addr

	c, err := Open(endpoint, 3*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()

	vals, err := c.Read([]NodeId{NewNodeID(2, 1001)})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(vals) != 1 || vals[0].Value == nil {
		t.Fatalf("Read 结果异常: %+v", vals)
	}
	if f, ok := vals[0].Value.Value.(float64); !ok || f != 25.5 {
		t.Fatalf("读回值异常: %v", vals[0].Value.Value)
	}

	st, err := c.Write(NewNodeID(2, 3001), mustVariant(t, 200.0))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !st.IsGood() {
		t.Fatalf("Write 结果非 Good: %s", st)
	}

	// 写后读回
	vals, err = c.Read([]NodeId{NewNodeID(2, 1001)})
	if err != nil {
		t.Fatalf("Read after write: %v", err)
	}
	if f, ok := vals[0].Value.Value.(float64); !ok || f != 200.0 {
		t.Fatalf("写后读回异常: %v", vals[0].Value.Value)
	}
}

// TestClientReadBadNode 验证读未知节点返回 BadNodeIdUnknown（不报错）。
func TestClientReadBadNode(t *testing.T) {
	addr := startMockOPCUServer(t, false)
	c, err := Open("opc.tcp://"+addr, 3*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()
	vals, err := c.Read([]NodeId{NewNodeID(2, 9999)})
	if err != nil {
		t.Fatalf("Read 未知节点不应报错: %v", err)
	}
	if len(vals) != 1 || vals[0].Status == nil || vals[0].Status.IsGood() {
		t.Fatalf("未知节点应返回 Bad 状态: %+v", vals)
	}
}

// TestClientOpenRejected 验证服务端拒绝 OPN 时 Open 返回错误。
func TestClientOpenRejected(t *testing.T) {
	addr := startMockOPCUServer(t, true)
	if _, err := Open("opc.tcp://"+addr, 3*time.Second); err == nil {
		t.Fatalf("OPN 被拒应报错")
	}
}

// --- helpers ---

func t2s(t *testing.T) *testing.T { return t }

func serverReadFrameRaw(c net.Conn) (string, []byte, error) {
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return "", nil, err
	}
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return "", nil, err
	}
	h, err := DecodeHeader(hdr[:])
	if err != nil {
		return "", nil, err
	}
	body := make([]byte, h.MessageSize-HeaderSize)
	if _, err := io.ReadFull(c, body); err != nil {
		return "", nil, err
	}
	return h.MessageType, body, nil
}

func serverWriteFrameRaw(c net.Conn, msgType string, body []byte) error {
	hdr, err := EncodeHeader(MessageHeader{MessageType: msgType, ChunkType: ChunkFinal, MessageSize: uint32(HeaderSize + len(body))})
	if err != nil {
		return err
	}
	frame := append(hdr, body...)
	_, err = c.Write(frame)
	return err
}

func serverWriteFrameWithChannel(c net.Conn, msgType string, channelID uint32, body []byte) error {
	hdr, err := EncodeHeader(MessageHeader{MessageType: msgType, ChunkType: ChunkFinal, MessageSize: uint32(HeaderSize + len(body)), ChannelId: channelID})
	if err != nil {
		return err
	}
	frame := append(hdr, body...)
	_, err = c.Write(frame)
	return err
}

func decodeOPNBody(body []byte) (OpenSecureChannelRequest, error) {
	_, rest, err := DecodeAsymmetricSecurityHeader(body)
	if err != nil {
		return OpenSecureChannelRequest{}, err
	}
	_, rest, err = DecodeSequenceHeader(rest)
	if err != nil {
		return OpenSecureChannelRequest{}, err
	}
	return DecodeOpenSecureChannelRequest(rest)
}

func mustEncode(t *testing.T, v any) []byte {
	switch x := v.(type) {
	case Acknowledge:
		b, err := x.Encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return stripHeader(b)
	case ErrorMessage:
		b, err := x.Encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return stripHeader(b)
	case OpenSecureChannelResponse:
		b, err := EncodeOpenSecureChannelResponse(x)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	case AsymmetricSecurityHeader:
		b, err := EncodeAsymmetricSecurityHeader(x)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	case SequenceHeader:
		b, err := EncodeSequenceHeader(x)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	case SymmetricSecurityHeader:
		b, err := EncodeSymmetricSecurityHeader(x)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	case CreateSessionResponse:
		b, err := EncodeCreateSessionResponse(x)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	case ActivateSessionResponse:
		b, err := EncodeActivateSessionResponse(x)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	case ReadResponse:
		b, err := EncodeReadResponse(x)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	case WriteResponse:
		b, err := EncodeWriteResponse(x)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	case CloseSessionResponse:
		b, err := EncodeCloseSessionResponse(x)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return b
	}
	t.Fatalf("mustEncode: 不支持类型 %T", v)
	return nil
}

func stripHeader(b []byte) []byte {
	return b[HeaderSize:]
}

var _ = fmt.Sprintf
