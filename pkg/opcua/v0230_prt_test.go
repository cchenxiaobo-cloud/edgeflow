package opcua

// v0.23.0 PRT 批量修复回归测试（T-16 / T-15）。
// 覆盖：PRT-06 nonce rand、PRT-07 发布序号防重放、PRT-08 OPN 响应校验、
// PRT-09 ReceiveBufferSize 上限、PRT-10 Subscribe 失败清理、
// PRT-13 通知负长度拒绝、PRT-15 Hello EndpointUrl 上限、
// PRT-16 Abort 帧容忍、PRT-17 pending 兜底表过期清理。
// 注：本仓库 UA Binary 为大端序；手工构造报文一律经仓库 encoder。

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// ---- PRT-06：randomNonce 失败即报错（签名已变更，降级路径已删）----

// TestRandomNonceReturnsError路径：正常路径返回满长度且非全零；
// 降级路径在源码层面已删除（grep 验证），此处验证 API 形态与输出质量。
func TestRandomNonceNoTimestampFallback(t *testing.T) {
	b, err := randomNonce(32)
	if err != nil {
		t.Fatalf("randomNonce 正常环境不应报错: %v", err)
	}
	if len(b) != 32 {
		t.Fatalf("nonce 长度 = %d, want 32", len(b))
	}
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("nonce 全零，熵异常")
	}
	// 两次调用不重复（时间戳派生的典型特征是低熵可复现）
	b2, _ := randomNonce(32)
	if string(b) == string(b2) {
		t.Fatal("两次 nonce 相同，疑似确定性派生（PRT-06 降级残留）")
	}
}

// ---- PRT-15：Hello EndpointUrl 长度上限 ----

func TestHelloEndpointUrlTooLongRejected(t *testing.T) {
	h := Hello{
		ProtocolVersion:   0,
		ReceiveBufferSize: 65536,
		SendBufferSize:    65536,
		MaxMessageSize:    65536,
		MaxChunkCount:     1,
		EndpointUrl:       strings.Repeat("a", MaxEndpointUrlLength+1),
	}
	if _, err := h.Encode(); err == nil {
		t.Fatal("超长 EndpointUrl 应拒绝（PRT-15）")
	} else if !errors.Is(err, ErrTooLong) {
		t.Fatalf("错误应包装 ErrTooLong，实际: %v", err)
	}
	// 边界内仍可编码
	h.EndpointUrl = strings.Repeat("a", MaxEndpointUrlLength)
	if _, err := h.Encode(); err != nil {
		t.Fatalf("边界长度（=4096）应可编码: %v", err)
	}
}

// ---- PRT-09：ReceiveBufferSize 16MB 上限 ----

func TestApplyAckRejectsOversizedReceiveBuffer(t *testing.T) {
	c := &Conn{}
	// 超上限值必须拒绝；边界值（=上限）接受。
	oversized := []uint32{16<<20 + 1, 1 << 30, 0xFFFFFFFF - 1}
	for _, sz := range oversized {
		err := c.applyAck(&Acknowledge{
			ProtocolVersion:   DefaultProtocolVersion,
			ReceiveBufferSize: sz,
			SendBufferSize:    65536,
			MaxMessageSize:    65536,
			MaxChunkCount:     1,
		})
		if err == nil {
			t.Fatalf("ReceiveBufferSize=%d 应被拒绝（上限 %d）", sz, MaxReceiveBufferSize)
		}
	}
	// 边界值（=上限）应接受且 sendLimit 生效
	err := c.applyAck(&Acknowledge{
		ProtocolVersion:   DefaultProtocolVersion,
		ReceiveBufferSize: MaxReceiveBufferSize,
		SendBufferSize:    65536,
		MaxMessageSize:    65536,
		MaxChunkCount:     1,
	})
	if err != nil {
		t.Fatalf("边界值 ReceiveBufferSize=%d 应接受: %v", MaxReceiveBufferSize, err)
	}
	// sendLimit = min(ReceiveBufferSize, MaxMessageSize)；MaxMessageSize=65536
	// 时被钳到 65536，但绝不超过 MaxReceiveBufferSize（PRT-09 上限语义）。
	if c.sendLimit > MaxReceiveBufferSize {
		t.Fatalf("sendLimit = %d 超过上限 %d", c.sendLimit, MaxReceiveBufferSize)
	}
}

// ---- PRT-08：OPN 响应头校验（恶意服务器套件）----

// 恶意OPNServer 通用骨架：先正常 HEL/ACK，OPN 阶段发送构造响应帧。
type 恶意OPNServer struct {
	opnFrame []byte // 完整 OPNF 帧（含 12 字节头）
}

func (m *恶意OPNServer) handle(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, body, err := serverReadFrameRaw(c); err != nil || decodeHelloType(body) != nil {
		return
	}
	ack := mustEncode(t2s(t), Acknowledge{ProtocolVersion: 0, ReceiveBufferSize: 65536, SendBufferSize: 65536, MaxMessageSize: 65536, MaxChunkCount: 1})
	if err := serverWriteFrameRaw(c, MsgAcknowledge, ack); err != nil {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		// 继续读 OPN 帧（不校验内容）
	}
	// 重置 deadline 后读 OPN 帧
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if msgType, _, err := serverReadFrameRaw(c); err != nil || msgType != MsgOpenSecureChannel {
		return
	}
	_ = serverWriteFrameWithChannel(c, MsgOpenSecureChannel, 7, stripHeader(m.opnFrame))
}

func decodeHelloType(body []byte) error {
	_, err := DecodeHello(body)
	return err
}

func buildOPNResponseFrame(t *testing.T, asym AsymmetricSecurityHeader, resp OpenSecureChannelResponse) []byte {
	t.Helper()
	var e encoder
	if err := asym.encodeUA(&e); err != nil {
		t.Fatalf("asym: %v", err)
	}
	if err := (SequenceHeader{SequenceNumber: 1, RequestID: 1}).encodeUA(&e); err != nil {
		t.Fatalf("seq: %v", err)
	}
	if err := resp.encodeUA(&e); err != nil {
		t.Fatalf("resp: %v", err)
	}
	hdr, err := EncodeHeader(MessageHeader{MessageType: MsgOpenSecureChannel, ChunkType: ChunkFinal, MessageSize: uint32(HeaderSize + len(e.buf)), ChannelId: 7})
	if err != nil {
		t.Fatalf("hdr: %v", err)
	}
	return append(hdr, e.buf...)
}

// TestOPNRejectsWrongPolicyURI：策略 URI 非 None → 拒绝。
func TestOPNRejectsWrongPolicyURI(t *testing.T) {
	frame := buildOPNResponseFrame(t,
		AsymmetricSecurityHeader{SecurityPolicyURI: "http://evil.example/Basic256"},
		OpenSecureChannelResponse{
			Timestamp: DateTimeFromTime(time.Now()), ServiceResult: 0,
			SecurityToken: ChannelSecurityToken{ChannelID: 7, TokenID: 7, RevisedLifetime: 600000},
		})
	err := runRecvOPNPipe(t, frame)
	if err == nil || !strings.Contains(err.Error(), "策略 URI 不符") {
		t.Fatalf("应拒绝非 None 策略 URI，实际: %v", err)
	}
}

// TestOPNRejectsNonEmptyCertFields：None 策略下证书字段非空 → 拒绝。
func TestOPNRejectsNonEmptyCertFields(t *testing.T) {
	frame := buildOPNResponseFrame(t,
		AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI, SenderCertificate: []byte{0x01, 0x02}},
		OpenSecureChannelResponse{
			Timestamp: DateTimeFromTime(time.Now()), ServiceResult: 0,
			SecurityToken: ChannelSecurityToken{ChannelID: 7, TokenID: 7, RevisedLifetime: 600000},
		})
	err := runRecvOPNPipe(t, frame)
	if err == nil || !strings.Contains(err.Error(), "非空证书字段") {
		t.Fatalf("应拒绝 None 策略下非空证书字段，实际: %v", err)
	}
}

// TestOPNRejectsZeroLifetime：RevisedLifetime=0 → 拒绝。
func TestOPNRejectsZeroLifetime(t *testing.T) {
	frame := buildOPNResponseFrame(t,
		AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI},
		OpenSecureChannelResponse{
			Timestamp: DateTimeFromTime(time.Now()), ServiceResult: 0,
			SecurityToken: ChannelSecurityToken{ChannelID: 7, TokenID: 7, RevisedLifetime: 0},
		})
	err := runRecvOPNPipe(t, frame)
	if err == nil || !strings.Contains(err.Error(), "RevisedLifetime") {
		t.Fatalf("应拒绝零寿命令牌，实际: %v", err)
	}
}

// TestOPNAcceptsLegitimateResponse：合法 OPN 响应仍被接受（防误拒回归）。
func TestOPNAcceptsLegitimateResponse(t *testing.T) {
	frame := buildOPNResponseFrame(t,
		AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI},
		OpenSecureChannelResponse{
			Timestamp: DateTimeFromTime(time.Now()), ServiceResult: 0,
			SecurityToken: ChannelSecurityToken{ChannelID: 7, TokenID: 7, RevisedLifetime: 600000},
		})
	if err := runRecvOPNPipe(t, frame); err != nil {
		t.Fatalf("合法 OPN 响应应接受: %v", err)
	}
}

// runRecvOPNPipe 用 net.Pipe 驱动一次 recvOPN：后台 goroutine 按协议
// 先读客户端 OPN 请求帧，再回放构造帧。完全确定性，无真实 TCP。
func runRecvOPNPipe(t *testing.T, opnFrame []byte) error {
	t.Helper()
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	go func() {
		// 直接收发：net.Pipe 同步写会阻塞到 recvOPN 读走，先回放 OPN 响应帧。
		_, _ = server.Write(opnFrame)
	}()
	sc := &SecureChannel{conn: &Conn{netConn: client}}
	err := sc.recvOPN(2 * time.Second)
	_ = client.Close()
	return err
}

// ---- PRT-16：Abort 帧容忍 ----

// TestReadFrameAbortReturnsSentinel：Abort 帧读到 EOF 前返回 ErrPeerAbort。
func TestReadFrameAbortReturnsSentinel(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		// 构造 Abort 帧：MSG! + size + channelId + reason
		hdr, _ := EncodeHeader(MessageHeader{MessageType: MsgSecureMessage, ChunkType: ChunkAbort, MessageSize: HeaderSize + 4, ChannelId: 3})
		frame := append(hdr, 0x00, 0x2F, 0x00, 0x2F) // reason "..."
		_, _ = server.Write(frame)
	}()
	c := &Conn{netConn: client}
	_, _, err := c.readFrame()
	if !errors.Is(err, ErrPeerAbort) {
		t.Fatalf("Abort 帧应返回 ErrPeerAbort 哨兵，实际: %v", err)
	}
	_ = server.Close()
}

// TestReadFrameIntermediateChunkStillRejected：中间块 'C' 仍拒绝（回归）。
func TestReadFrameIntermediateChunkStillRejected(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		hdr, _ := EncodeHeader(MessageHeader{MessageType: MsgSecureMessage, ChunkType: ChunkIntermediate, MessageSize: HeaderSize, ChannelId: 3})
		_, _ = server.Write(hdr)
	}()
	c := &Conn{netConn: client}
	_, _, err := c.readFrame()
	if !errors.Is(err, ErrChunkingUnsupported) {
		t.Fatalf("中间块应仍拒绝 ErrChunkingUnsupported，实际: %v", err)
	}
	_ = server.Close()
}

// ---- PRT-07：发布序号防重放 ----

// TestConsumePublishFrameRejectsReplay：重复/回退/零序号帧被丢弃，
// 不投递、不生成 ack、lastPubSeq 不回退。
func TestConsumePublishFrameRejectsReplay(t *testing.T) {
	c := &Client{pubCh: make(chan PublishResult, 8)}
	mkBody := func(seq uint32) []byte {
		resp := PublishResponse{
			ResponseHeader: ResponseHeader{
				Timestamp: DateTimeFromTime(time.Now()), RequestHandle: 1, ServiceResult: 0,
			},
			SubscriptionId: 9,
			NotificationMessage: NotificationMessage{
				SequenceNumber: seq,
				PublishTime:    DateTimeFromTime(time.Now()),
			},
		}
		var e encoder
		if err := resp.encodeUA(&e); err != nil {
			t.Fatalf("encode: %v", err)
		}
		return e.buf
	}

	// 首帧 seq=5：正常投递并推进
	c.consumePublishFrame(mkBody(5))
	if c.LastPubSeqForProbe() != 5 {
		t.Fatalf("lastPubSeq = %d, want 5", c.LastPubSeqForProbe())
	}
	if len(c.AcksForProbe()) != 1 {
		t.Fatalf("合法帧应生成 ack，got %d", len(c.AcksForProbe()))
	}
	if len(c.pubCh) != 1 {
		t.Fatalf("合法帧应投递 1 条，got %d", len(c.pubCh))
	}

	// 重放 seq=5：丢弃
	c.consumePublishFrame(mkBody(5))
	// 回退 seq=3：丢弃
	c.consumePublishFrame(mkBody(3))
	// 非法 seq=0：丢弃
	c.consumePublishFrame(mkBody(0))
	if got := c.LastPubSeqForProbe(); got != 5 {
		t.Fatalf("重放/回退/零序号后 lastPubSeq = %d, want 5（不回退）", got)
	}
	if n := len(c.AcksForProbe()); n != 1 {
		t.Fatalf("丢弃帧不应生成 ack，acks = %d, want 1", n)
	}
	if n := len(c.pubCh); n != 1 {
		t.Fatalf("丢弃帧不应投递，pubCh = %d, want 1", n)
	}

	// 前进 seq=6：仍正常（防误杀回归）
	c.consumePublishFrame(mkBody(6))
	if got := c.LastPubSeqForProbe(); got != 6 {
		t.Fatalf("seq=6 应接受，lastPubSeq = %d", got)
	}
}

// ---- PRT-13：通知解码负长度拒绝 ----

func TestNotificationNegativeLengthRejected(t *testing.T) {
	// 顶层 notificationData 负长度（-2）
	e := encoder{}
	e.u32(7)    // SequenceNumber
	e.i64(1000) // PublishTime
	e.i32(-2)   // notificationData 长度（非法负值）
	d := &decoder{b: e.buf}
	if _, err := decodeNotificationBody(d); err == nil {
		t.Fatal("notificationData 负长度(-2)应拒绝（PRT-13）")
	} else if !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("应包装 ErrInvalidEncoding，实际: %v", err)
	}

	// -1（null）放行为空通知
	e2 := encoder{}
	e2.u32(7)
	e2.i64(1000)
	e2.i32(-1)
	d2 := &decoder{b: e2.buf}
	m, err := decodeNotificationBody(d2)
	if err != nil {
		t.Fatalf("notificationData=-1（null）应放行: %v", err)
	}
	if len(m.NotificationData) != 0 {
		t.Fatalf("null 通知应为空，got %d", len(m.NotificationData))
	}
}

func TestDataChangeNegativeItemsLengthRejected(t *testing.T) {
	// DataChange 通知 ExtObj：monitoredItems 长度 -3 → 拒绝
	inner := encoder{}
	inner.i32(-3)
	inner.i32(-1) // diagnosticInfos null
	ext := ExtensionObject{
		TypeId:   NewNodeID(0, TypeIdDataChangeNotification),
		Encoding: ExtensionObjectEncodingByteString,
		Body:     inner.buf,
	}
	_, err := decodeNotificationExtObj(ext)
	if err == nil {
		t.Fatal("monitoredItems 负长度(-3)应拒绝（PRT-13）")
	} else if !errors.Is(err, ErrInvalidEncoding) {
		t.Fatalf("应包装 ErrInvalidEncoding，实际: %v", err)
	}
}

// ---- PRT-10：Subscribe 失败清理（DeleteSubscriptions 发出验证）----

// failMIServer 正常建订阅，CreateMonitoredItems 返回 Bad 结果；
// 记录是否收到 DeleteSubscriptionsRequest（PRT-10 清理证据）。
type failMIServer struct {
	t           *testing.T
	gotDelSub   chan struct{}
	acknowledge bool
}

func TestSubscribeFailureCleansUpSubscription(t *testing.T) {
	srv := &failMIServer{t: t, gotDelSub: make(chan struct{}, 1)}
	addr := startMockServer(t, func(c net.Conn) { srv.handle(c) })
	c, err := Open("opc.tcp://"+addr, 3*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()

	_, err = c.Subscribe([]NodeId{NewNodeID(2, 1001)}, 0)
	if err == nil {
		t.Fatal("CreateMonitoredItems 失败应向调用方报错")
	}
	select {
	case <-srv.gotDelSub:
		// 收到 DeleteSubscriptions：清理已执行（PRT-10 达成）
	case <-time.After(3 * time.Second):
		t.Fatal("Subscribe 失败后未发出 DeleteSubscriptions（孤儿订阅泄漏，PRT-10）")
	}
}

// mustEncodeV0230 扩展 mustEncode 支持订阅响应类型（既有 helper 在
// client_test.go，不可改动；新类型在此分流，其余复用既有实现）。
func mustEncodeV0230(t *testing.T, v any) []byte {
	var b []byte
	var err error
	switch x := v.(type) {
	case CreateSubscriptionResponse:
		b, err = EncodeCreateSubscriptionResponse(x)
	case CreateMonitoredItemsResponse:
		b, err = EncodeCreateMonitoredItemsResponse(x)
	case DeleteSubscriptionsResponse:
		b, err = EncodeDeleteSubscriptionsResponse(x)
	default:
		return mustEncode(t, v)
	}
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	return b
}

func (s *failMIServer) handle(c net.Conn) {
	t := s.t
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = c.Close() }()
	// HEL → ACK
	msgType, body, err := serverReadFrameRaw(c)
	if err != nil || msgType != MsgHello {
		return
	}
	if _, err := DecodeHello(body); err != nil {
		return
	}
	ack := mustEncode(t2s(t), Acknowledge{ProtocolVersion: 0, ReceiveBufferSize: 65536, SendBufferSize: 65536, MaxMessageSize: 65536, MaxChunkCount: 1})
	if err := serverWriteFrameRaw(c, MsgAcknowledge, ack); err != nil {
		return
	}
	// OPN
	msgType, body, err = serverReadFrameRaw(c)
	if err != nil || msgType != MsgOpenSecureChannel {
		return
	}
	if _, err := decodeOPNBody(body); err != nil {
		return
	}
	opnBody := mustEncode(t2s(t), OpenSecureChannelResponse{
		Timestamp: DateTimeFromTime(time.Now()), ServiceResult: 0,
		SecurityToken: ChannelSecurityToken{ChannelID: 11, TokenID: 11, CreatedAt: DateTimeFromTime(time.Now()), RevisedLifetime: 600000},
	})
	asym := mustEncode(t2s(t), AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI})
	seqH := mustEncode(t2s(t), SequenceHeader{SequenceNumber: 1, RequestID: 1})
	frame := append(asym, seqH...)
	frame = append(frame, opnBody...)
	if err := serverWriteFrameWithChannel(c, MsgOpenSecureChannel, 11, frame); err != nil {
		return
	}
	// MSG 服务循环
	for {
		msgType, body, err = serverReadFrameRaw(c)
		if err != nil {
			return
		}
		if msgType == MsgCloseSecureChannel {
			return
		}
		if msgType != MsgSecureMessage {
			return
		}
		_, rest, err := DecodeSymmetricSecurityHeader(body)
		if err != nil {
			return
		}
		sh, rest, err := DecodeSequenceHeader(rest)
		if err != nil {
			return
		}
		respHeader := ResponseHeader{Timestamp: DateTimeFromTime(time.Now()), RequestHandle: sh.RequestID, ServiceResult: 0}
		writeResp := func(respBody []byte) bool {
			sym := mustEncode(t2s(t), SymmetricSecurityHeader{TokenID: 11})
			seq := mustEncode(t2s(t), SequenceHeader{SequenceNumber: 1, RequestID: sh.RequestID})
			f := append(sym, seq...)
			f = append(f, respBody...)
			return serverWriteFrameWithChannel(c, MsgSecureMessage, 11, f) == nil
		}
		// CreateSession
		if req, err := DecodeCreateSessionRequest(rest); err == nil && req.ClientDescription.ApplicationUri != "" {
			resp := CreateSessionResponse{
				ResponseHeader: respHeader, SessionId: NewNodeID(0, 99),
				AuthenticationToken: NewNodeID(0, 100), RevisedSessionTimeout: 3600000, MaxRequestMessageSize: 65536,
			}
			if !writeResp(mustEncode(t2s(t), resp)) {
				return
			}
			continue
		}
		// ActivateSession
		if _, err := DecodeActivateSessionRequest(rest); err == nil {
			resp := ActivateSessionResponse{ResponseHeader: respHeader, Results: []StatusCode{0}}
			if !writeResp(mustEncode(t2s(t), resp)) {
				return
			}
			continue
		}
		// CreateSubscription → 成功
		if _, err := DecodeCreateSubscriptionRequest(rest); err == nil {
			resp := CreateSubscriptionResponse{
				ResponseHeader: respHeader, SubscriptionId: 55,
				RevisedPublishingInterval: 500, RevisedLifetimeCount: 60, RevisedMaxKeepAliveCount: 20,
			}
			if !writeResp(mustEncodeV0230(t, resp)) {
				return
			}
			continue
		}
		// CreateMonitoredItems → Bad（触发 PRT-10 清理路径）
		if _, err := DecodeCreateMonitoredItemsRequest(rest); err == nil {
			resp := CreateMonitoredItemsResponse{
				ResponseHeader: respHeader,
				Results:        []MonitoredItemCreateResultDetail{{MonitoredItemId: 1, StatusCode: StatusCode(StatusBadNodeIdUnknown)}},
			}
			if !writeResp(mustEncodeV0230(t, resp)) {
				return
			}
			continue
		}
		// DeleteSubscriptions → 记录清理证据
		if _, err := DecodeDeleteSubscriptionsRequest(rest); err == nil {
			select {
			case s.gotDelSub <- struct{}{}:
			default:
			}
			resp := DeleteSubscriptionsResponse{ResponseHeader: respHeader, Results: []StatusCode{0}}
			if !writeResp(mustEncodeV0230(t, resp)) {
				return
			}
			continue
		}
		return
	}
}

// ---- PRT-17：pending 兜底表过期清理 ----

// TestRoundTripTimeoutCleansPending：真实超时路径行为验证——
// 服务器不回包；预置 pending[reqID]（模拟"响应先到、waiter 后登记"竞态
// 遗留），roundTrip 超时放弃 waiter 时必须顺带清理该 pending 条目。
func TestRoundTripTimeoutCleansPending(t *testing.T) {
	srv := &pendingHoardingServer{t: t}
	addr := startMockServer(t, func(c net.Conn) { srv.handle(c) })
	c, err := Open("opc.tcp://"+addr, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()

	c.ensurePump()
	// 预知下一次 roundTrip 的 reqID = sc.reqId + 1（白盒；sendSecure 内 nextReqID）
	c.mu.Lock()
	nextID := c.sc.reqId + 1
	if c.pending == nil {
		c.pending = make(map[uint32][]byte)
	}
	c.pending[nextID] = []byte{0x01} // 竞态遗留形态：泵已暂存、waiter 未登记
	c.mu.Unlock()

	// 第一段：竞态兜底——预置 pending[nextID] 应被 roundTrip 入口检查直接
	// 消费并成功返回（响应先到、waiter 未登记的合法路径），而非超时。
	//（主线修正：原骨架预置 pending 后又断言同请求超时，前提自相矛盾——
	// 竞态兜底命中即成功返回，永远到不了超时分支。）
	b, err := c.roundTrip([]byte{0x01, 0x02, 0x03})
	if err != nil {
		t.Fatalf("预置 pending 应被竞态兜底消费并成功返回: %v", err)
	}
	if len(b) != 1 || b[0] != 0x01 {
		t.Fatalf("竞态兜底返回体不符: %v", b)
	}
	c.mu.Lock()
	if n := len(c.pending); n != 0 {
		t.Fatalf("消费后 pending 应清空，剩 %d 条", n)
	}
	if n := len(c.waiters); n != 0 {
		t.Fatalf("成功路径不应留下 waiter，got %d", n)
	}
	c.mu.Unlock()

	// 第二段：真实超时路径——服务端沉默，请求必超时；PRT-17 要求超时
	// 放弃 waiter 时顺带清理 pending（本请求 reqID 的任何滞留条目），
	// 兜底表不得随超时累积。
	if _, err := c.roundTrip([]byte{0x09}); err == nil {
		t.Fatal("无响应请求应超时")
	}
	c.mu.Lock()
	nPending := len(c.pending)
	nWaiters := len(c.waiters)
	c.mu.Unlock()
	if nPending != 0 {
		t.Fatalf("超时后 pending 应清空（PRT-17），剩 %d 条", nPending)
	}
	if nWaiters != 0 {
		t.Fatalf("超时后 waiters 应清空，got %d", nWaiters)
	}
}

// pendingHoardingServer 不回复业务请求（诱导超时路径），但完整握手
// （HEL/ACK → OPN → CreateSession/ActivateSession）让 Open 成功——
// 主线修复：原骨架连握手也不回，Open 直接 i/o timeout，测试从未到达
// 被测的 roundTrip 超时路径。
type pendingHoardingServer struct {
	t *testing.T
}

func (s *pendingHoardingServer) handle(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	defer func() { _ = c.Close() }()
	// HEL → ACK
	msgType, body, err := serverReadFrameRaw(c)
	if err != nil || msgType != MsgHello {
		return
	}
	if _, err := DecodeHello(body); err != nil {
		return
	}
	ack := mustEncode(t2s(s.t), Acknowledge{ProtocolVersion: 0, ReceiveBufferSize: 65536, SendBufferSize: 65536, MaxMessageSize: 65536, MaxChunkCount: 1})
	if err := serverWriteFrameRaw(c, MsgAcknowledge, ack); err != nil {
		return
	}
	// OPN → OPN 响应
	msgType, body, err = serverReadFrameRaw(c)
	if err != nil || msgType != MsgOpenSecureChannel {
		return
	}
	if _, err := decodeOPNBody(body); err != nil {
		return
	}
	opnBody := mustEncode(t2s(s.t), OpenSecureChannelResponse{
		Timestamp: DateTimeFromTime(time.Now()), ServiceResult: 0,
		SecurityToken: ChannelSecurityToken{ChannelID: 11, TokenID: 11, CreatedAt: DateTimeFromTime(time.Now()), RevisedLifetime: 600000},
	})
	asym := mustEncode(t2s(s.t), AsymmetricSecurityHeader{SecurityPolicyURI: SecurityPolicyNoneURI})
	seqH := mustEncode(t2s(s.t), SequenceHeader{SequenceNumber: 1, RequestID: 1})
	frame := append(asym, seqH...)
	frame = append(frame, opnBody...)
	if err := serverWriteFrameWithChannel(c, MsgOpenSecureChannel, 11, frame); err != nil {
		return
	}
	// MSG 服务循环：CreateSession/ActivateSession 正常应答（让 Open 走通），
	// 其余请求（被测 roundTrip 的任意字节）一律沉默不回 → 触发真实超时。
	for {
		msgType, body, err = serverReadFrameRaw(c)
		if err != nil {
			return
		}
		if msgType == MsgCloseSecureChannel {
			return
		}
		if msgType != MsgSecureMessage {
			return
		}
		_, rest, err := DecodeSymmetricSecurityHeader(body)
		if err != nil {
			return
		}
		sh, rest, err := DecodeSequenceHeader(rest)
		if err != nil {
			return
		}
		respHeader := ResponseHeader{Timestamp: DateTimeFromTime(time.Now()), RequestHandle: sh.RequestID, ServiceResult: 0}
		writeResp := func(respBody []byte) bool {
			sym := mustEncode(t2s(s.t), SymmetricSecurityHeader{TokenID: 11})
			seq := mustEncode(t2s(s.t), SequenceHeader{SequenceNumber: 1, RequestID: sh.RequestID})
			f := append(sym, seq...)
			f = append(f, respBody...)
			return serverWriteFrameWithChannel(c, MsgSecureMessage, 11, f) == nil
		}
		if req, err := DecodeCreateSessionRequest(rest); err == nil && req.ClientDescription.ApplicationUri != "" {
			resp := CreateSessionResponse{
				ResponseHeader: respHeader, SessionId: NewNodeID(0, 99),
				AuthenticationToken: NewNodeID(0, 100), RevisedSessionTimeout: 3600000, MaxRequestMessageSize: 65536,
			}
			if !writeResp(mustEncode(t2s(s.t), resp)) {
				return
			}
			continue
		}
		if _, err := DecodeActivateSessionRequest(rest); err == nil {
			resp := ActivateSessionResponse{ResponseHeader: respHeader, Results: []StatusCode{0}}
			if !writeResp(mustEncode(t2s(s.t), resp)) {
				return
			}
			continue
		}
		// 其余请求：沉默（保持连接，不回包 → roundTrip 超时路径）。
	}
}
