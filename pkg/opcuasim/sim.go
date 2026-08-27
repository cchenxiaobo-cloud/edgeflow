// Package opcuasim 提供高保真 OPC-UA 模拟服务器（WBS 5.2，SecurityPolicy
// None，匿名会话）。
//
// 设计目标：在无真实设备时验证 OPC-UA Mapper 的完整读写链路与操作台账。
// 协议处理（HEL/ACK、OPN/OPNF、MSG 服务分派、CLO）完全自实现——基于
// pkg/opcua 的协议栈核心与 v0.14.0 服务层；客户端侧同样用 pkg/opcua 的
// Client（自实现客户端 × 自实现服务端双向交叉验证，与 modbussim 同构）。
//
// 设备模型（1 台模拟设备，点位表是 Mapper 对接的事实标准）：
//
//	ns=2;i=1001  temperature  Double  只读  温度 °C（向目标值收敛 + 随机扰动）
//	ns=2;i=1002  humidity     Double  只读  湿度 %（随机游走）
//	ns=2;i=1003  pressure     Double  只读  压力 kPa（随机游走）
//	ns=2;i=2001  running      Boolean 只读  运行状态 true
//	ns=2;i=2002  label        String  只读  "opcua-sim"
//	ns=2;i=3001  setpoint     Double  可写  目标温度（写后温度向其收敛）
//
// 动态行为：500ms 波动周期（WithStep 可调）刷新点位；温度按 0.2 收敛因子
// 趋向目标值并叠加 0.5°C 扰动，湿度 ±0.8%/周期，压力 ±0.3/周期。
//
// 安全：默认绑定 127.0.0.1:14840（只回环防意外暴露，env OPCUA_SIM_PORT
// 覆盖）；明文无认证，仅限本机/可信隔离网络（与 pkg/opcua 的
// SecurityPolicy None 边界一致）。
package opcuasim

import (
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"sync"
	"time"

	"edgeflow/pkg/log"
	"edgeflow/pkg/opcua"
)

// DefaultPort 是模拟器默认端口。
const DefaultPort = 14840

// EnvPort 是覆盖模拟器端口的环境变量名。
const EnvPort = "OPCUA_SIM_PORT"

// 点位表常量（与 docs/OPCUA-GUIDE.md 一致）。
const (
	// NodeTemperature 是温度节点（只读，Double）。
	NodeTemperature = 1001
	// NodeHumidity 是湿度节点（只读，Double）。
	NodeHumidity = 1002
	// NodePressure 是压力节点（只读，Double）。
	NodePressure = 1003
	// NodeRunning 是运行状态节点（只读，Boolean）。
	NodeRunning = 2001
	// NodeLabel 是标签节点（只读，String）。
	NodeLabel = 2002
	// NodeSetpoint 是目标温度节点（可写，Double）。
	NodeSetpoint = 3001

	// Namespace 是点位所在命名空间。
	Namespace = 2

	// DefaultSetpoint 是初始目标温度。
	DefaultSetpoint = 25.0
	// DefaultTemperature 是初始温度。
	DefaultTemperature = 24.5

	// tempConverge 是温度收敛因子（每周期向目标靠近的比例）。
	tempConverge = 0.2
	// tempNoise 是温度每周期最大扰动（°C）。
	tempNoise = 0.5
	// humNoise 是湿度每周期最大扰动（%）。
	humNoise = 0.8
	// pressNoise 是压力每周期最大扰动（kPa）。
	pressNoise = 0.3
)

// Option 是 Simulator 的可选配置项。
type Option func(*Simulator)

// WithStep 设置点位波动周期（默认 500ms；测试可调小加速收敛验证）。
func WithStep(d time.Duration) Option {
	return func(s *Simulator) { s.step = d }
}

// WithSeed 设置随机数种子（默认随机；测试传固定种子保证确定性）。
func WithSeed(seed int64) Option {
	return func(s *Simulator) { s.seed = seed }
}

// WithMaxConns 设置并发连接数上限（默认 8；达到上限后的新连接直接拒绝关闭）。
func WithMaxConns(n int) Option {
	return func(s *Simulator) { s.maxConns = n }
}

// WithEndpointURL 自定义端点 URL 字符串（默认 opc.tcp://<addr>）。
func WithEndpointURL(u string) Option {
	return func(s *Simulator) { s.endpointURL = u }
}

// Simulator 是一台 OPC-UA 模拟服务器。
type Simulator struct {
	ln          net.Listener
	addr        string
	realAddr    string // 监听后实际地址（端口 0 时由 ln.Addr() 回填）
	endpointURL string
	step        time.Duration
	seed        int64
	maxConns    int

	mu        sync.Mutex
	connCount int
	stopping  bool

	temp     float64
	hum      float64
	press    float64
	setpoint float64
	stopCh   chan struct{}
}

// New 创建模拟器（未启动）。listenAddr 为空时默认 "127.0.0.1:14840"。
func New(listenAddr string, opts ...Option) *Simulator {
	if listenAddr == "" {
		listenAddr = fmt.Sprintf("127.0.0.1:%d", DefaultPort)
	}
	s := &Simulator{
		addr:     listenAddr,
		step:     500 * time.Millisecond,
		seed:     time.Now().UnixNano(),
		maxConns: 8,
		temp:     DefaultTemperature,
		hum:      60,
		press:    101.3,
		setpoint: DefaultSetpoint,
	}
	for _, o := range opts {
		o(s)
	}
	if s.endpointURL == "" {
		s.endpointURL = "opc.tcp://" + listenAddr
	}
	return s
}

// Addr 返回监听地址（端口 0 时返回实际分配的端口）。
func (s *Simulator) Addr() string {
	if s.realAddr != "" {
		return s.realAddr
	}
	return s.addr
}

// EndpointURL 返回客户端应使用的端点 URL。
func (s *Simulator) EndpointURL() string { return s.endpointURL }

// Setpoint 返回当前目标温度（测试断言用）。
func (s *Simulator) Setpoint() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setpoint
}

// Temperature 返回当前温度（测试断言用）。
func (s *Simulator) Temperature() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.temp
}

// Start 启动监听与波动 goroutine。返回错误表示监听失败。
func (s *Simulator) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("opcuasim: listen %s: %w", s.addr, err)
	}
	s.ln = ln
	if real := ln.Addr().String(); real != "" {
		s.realAddr = real
	}
	s.stopCh = make(chan struct{})
	go s.fluctuate()
	go s.acceptLoop()
	log.Infof("OPC-UA 模拟器已启动（%s，端点 %s，点位 6 个）", s.addr, s.endpointURL)
	return nil
}

// Stop 停止监听与波动 goroutine。
func (s *Simulator) Stop() error {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	if s.stopCh != nil {
		select {
		case <-s.stopCh:
		default:
			close(s.stopCh)
		}
	}
	return nil
}

// NodeTable 返回点位表打印（hack/opcua-sim 输出用）。
func (s *Simulator) NodeTable() string {
	return `点位表（命名空间 ns=2）：
  ns=2;i=1001  temperature  Double   只读  温度 °C（向目标收敛 + 扰动）
  ns=2;i=1002  humidity     Double   只读  湿度 %（随机游走）
  ns=2;i=1003  pressure     Double   只读  压力 kPa（随机游走）
  ns=2;i=2001  running      Boolean  只读  运行状态
  ns=2;i=2002  label        String   只读  "opcua-sim"
  ns=2;i=3001  setpoint     Double   可写  目标温度（写后温度向其收敛）`
}

// fluctuate 按周期刷新动态点位（温度收敛 + 扰动，湿度/压力游走）。
func (s *Simulator) fluctuate() {
	rng := rand.New(rand.NewSource(s.seed))
	for {
		select {
		case <-s.stopCh:
			return
		case <-time.After(s.step):
			s.mu.Lock()
			// 温度向目标收敛
			s.temp += (s.setpoint - s.temp) * tempConverge
			s.temp += s.randSigned(rng, tempNoise)
			s.temp = clamp(s.temp, -20, 200)
			s.hum += s.randSigned(rng, humNoise)
			s.hum = clamp(s.hum, 0, 100)
			s.press += s.randSigned(rng, pressNoise)
			s.press = clamp(s.press, 50, 200)
			s.mu.Unlock()
		}
	}
}

func (s *Simulator) randSigned(rng *rand.Rand, max float64) float64 {
	return (rng.Float64()*2 - 1) * max
}

func clamp(v, lo, hi float64) float64 {
	return math.Min(hi, math.Max(lo, v))
}

// acceptLoop 接受连接并派发到 per-conn goroutine。
func (s *Simulator) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.connCount >= s.maxConns {
			s.mu.Unlock()
			_ = c.Close()
			continue
		}
		s.connCount++
		s.mu.Unlock()
		go s.handleConn(c)
	}
}

// handleConn 处理单条连接：HEL→ACK → OPN→OPNF → MSG 服务分派 → CLO。
func (s *Simulator) handleConn(c net.Conn) {
	defer func() {
		s.mu.Lock()
		s.connCount--
		s.mu.Unlock()
		_ = c.Close()
	}()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))

	// HEL → ACK
	msgType, body, err := readFrame(c)
	if err != nil {
		return
	}
	if msgType != opcua.MsgHello {
		return
	}
	hello, err := opcua.DecodeHello(body)
	if err != nil || hello.EndpointUrl == "" {
		return
	}
	ackBody, err := opcua.Acknowledge{
		ProtocolVersion: 0, ReceiveBufferSize: 65536, SendBufferSize: 65536,
		MaxMessageSize: 65536, MaxChunkCount: 1,
	}.Encode()
	if err != nil {
		return
	}
	if err := writeFrame(c, opcua.MsgAcknowledge, 0, stripHeader(ackBody)); err != nil {
		return
	}

	// OPN → OPNF
	msgType, body, err = readFrame(c)
	if err != nil {
		return
	}
	if msgType != opcua.MsgOpenSecureChannel {
		return
	}
	_, rest, err := opcua.DecodeAsymmetricSecurityHeader(body)
	if err != nil {
		return
	}
	_, rest, err = opcua.DecodeSequenceHeader(rest)
	if err != nil {
		return
	}
	opnReq, err := opcua.DecodeOpenSecureChannelRequest(rest)
	if err != nil {
		return
	}
	if opnReq.RequestType != opcua.SecurityTokenRequestTypeIssue {
		return
	}
	channelID, tokenID := uint32(rand.Int31n(1<<31-1)+1), uint32(rand.Int31n(1<<31-1)+1)
	opnResp, err := opcua.EncodeOpenSecureChannelResponse(opcua.OpenSecureChannelResponse{
		Timestamp:             opcua.DateTimeFromTime(time.Now()),
		ServiceResult:         0,
		ServerProtocolVersion: 0,
		SecurityToken:         opcua.ChannelSecurityToken{ChannelID: channelID, TokenID: tokenID, CreatedAt: opcua.DateTimeFromTime(time.Now()), RevisedLifetime: 600000},
	})
	if err != nil {
		return
	}
	asym, err := opcua.EncodeAsymmetricSecurityHeader(opcua.AsymmetricSecurityHeader{SecurityPolicyURI: opcua.SecurityPolicyNoneURI})
	if err != nil {
		return
	}
	seqH, err := opcua.EncodeSequenceHeader(opcua.SequenceHeader{SequenceNumber: 1, RequestID: opnReqSeq()})
	if err != nil {
		return
	}
	frame := append(append(append([]byte{}, asym...), seqH...), opnResp...)
	if err := writeFrame(c, opcua.MsgOpenSecureChannel, channelID, frame); err != nil {
		return
	}

	// MSG 服务循环
	for {
		msgType, body, err = readFrame(c)
		if err != nil {
			return
		}
		switch msgType {
		case opcua.MsgCloseSecureChannel:
			return
		case opcua.MsgSecureMessage:
			if !s.handleService(c, channelID, tokenID, body) {
				return
			}
		default:
			return
		}
	}
}

func opnReqSeq() uint32 { return 1 }

// handleService 解析 MSG 帧并分派服务请求。
func (s *Simulator) handleService(c net.Conn, channelID, tokenID uint32, body []byte) bool {
	_, rest, err := opcua.DecodeSymmetricSecurityHeader(body)
	if err != nil {
		return false
	}
	sh, rest, err := opcua.DecodeSequenceHeader(rest)
	if err != nil {
		return false
	}
	writeResp := func(respBody []byte) bool {
		sym, err := opcua.EncodeSymmetricSecurityHeader(opcua.SymmetricSecurityHeader{TokenID: tokenID})
		if err != nil {
			return false
		}
		seq, err := opcua.EncodeSequenceHeader(opcua.SequenceHeader{SequenceNumber: sh.SequenceNumber, RequestID: sh.RequestID})
		if err != nil {
			return false
		}
		frame := append(append(append([]byte{}, sym...), seq...), respBody...)
		return writeFrame(c, opcua.MsgSecureMessage, channelID, frame) == nil
	}
	respHeader := opcua.ResponseHeader{
		Timestamp:     opcua.DateTimeFromTime(time.Now()),
		RequestHandle: sh.RequestID,
		ServiceResult: 0,
	}

	if req, err := opcua.DecodeCreateSessionRequest(rest); err == nil && req.ClientDescription.ApplicationUri != "" {
		resp, err := opcua.EncodeCreateSessionResponse(opcua.CreateSessionResponse{
			ResponseHeader:        respHeader,
			SessionId:             opcua.NewNodeID(0, 1),
			AuthenticationToken:   opcua.NewNodeID(0, 1),
			RevisedSessionTimeout: 3600000,
			MaxRequestMessageSize: 65536,
		})
		if err != nil {
			return false
		}
		return writeResp(resp)
	}
	if _, err := opcua.DecodeActivateSessionRequest(rest); err == nil {
		resp, err := opcua.EncodeActivateSessionResponse(opcua.ActivateSessionResponse{
			ResponseHeader: respHeader,
			Results:        []opcua.StatusCode{0},
		})
		if err != nil {
			return false
		}
		return writeResp(resp)
	}
	if req, err := opcua.DecodeReadRequest(rest); err == nil {
		resp := opcua.ReadResponse{ResponseHeader: respHeader}
		for _, rv := range req.NodesToRead {
			resp.Results = append(resp.Results, s.readNode(rv.NodeId))
		}
		out, err := opcua.EncodeReadResponse(resp)
		if err != nil {
			return false
		}
		return writeResp(out)
	}
	if req, err := opcua.DecodeWriteRequest(rest); err == nil && len(req.NodesToWrite) > 0 {
		resp := opcua.WriteResponse{ResponseHeader: respHeader}
		for _, wv := range req.NodesToWrite {
			resp.Results = append(resp.Results, s.writeNode(wv))
		}
		out, err := opcua.EncodeWriteResponse(resp)
		if err != nil {
			return false
		}
		return writeResp(out)
	}
	if _, err := opcua.DecodeCloseSessionRequest(rest); err == nil {
		out, err := opcua.EncodeCloseSessionResponse(opcua.CloseSessionResponse{ResponseHeader: respHeader})
		if err != nil {
			return false
		}
		return writeResp(out)
	}
	return false
}

// readNode 读取节点值（未知节点 → BadNodeIdUnknown DataValue）。
func (s *Simulator) readNode(n opcua.NodeId) opcua.DataValue {
	if n.Namespace != Namespace || n.Type != opcua.NodeIDNumeric {
		return opcua.DataValue{Status: statusPtr(uint32(opcua.StatusBadNodeIdUnknown))}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	good := opcua.StatusCode(0)
	switch n.Numeric {
	case NodeTemperature:
		v, _ := opcua.NewVariant(s.temp)
		return opcua.DataValue{Value: &v, Status: &good}
	case NodeHumidity:
		v, _ := opcua.NewVariant(s.hum)
		return opcua.DataValue{Value: &v, Status: &good}
	case NodePressure:
		v, _ := opcua.NewVariant(s.press)
		return opcua.DataValue{Value: &v, Status: &good}
	case NodeRunning:
		v, _ := opcua.NewVariant(true)
		return opcua.DataValue{Value: &v, Status: &good}
	case NodeLabel:
		v, _ := opcua.NewVariant("opcua-sim")
		return opcua.DataValue{Value: &v, Status: &good}
	case NodeSetpoint:
		v, _ := opcua.NewVariant(s.setpoint)
		return opcua.DataValue{Value: &v, Status: &good}
	default:
		return opcua.DataValue{Status: statusPtr(uint32(opcua.StatusBadNodeIdUnknown))}
	}
}

// writeNode 写节点值（仅 setpoint 可写；类型不匹配 → BadTypeMismatch）。
func (s *Simulator) writeNode(w opcua.WriteValue) opcua.StatusCode {
	if w.NodeId.Namespace != Namespace || w.NodeId.Type != opcua.NodeIDNumeric {
		return opcua.StatusBadNodeIdUnknown
	}
	switch w.NodeId.Numeric {
	case NodeSetpoint:
		if w.Value.Value == nil {
			return opcua.StatusBadNothingToDo
		}
		f, ok := w.Value.Value.Value.(float64)
		if !ok {
			return 0x80740000 // BadTypeMismatch
		}
		s.mu.Lock()
		s.setpoint = f
		s.mu.Unlock()
		return 0
	default:
		return 0x80690000 // BadNotWritable
	}
}

func statusPtr(s uint32) *opcua.StatusCode {
	st := opcua.StatusCode(s)
	return &st
}

// readFrame 读取一帧（客户端侧相同；服务端复用）。
func readFrame(c net.Conn) (string, []byte, error) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	var hdr [opcua.HeaderSize]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return "", nil, err
	}
	h, err := opcua.DecodeHeader(hdr[:])
	if err != nil {
		return "", nil, err
	}
	body := make([]byte, h.MessageSize-opcua.HeaderSize)
	if _, err := io.ReadFull(c, body); err != nil {
		return "", nil, err
	}
	return h.MessageType, body, nil
}

// writeFrame 写出完整帧（header + body）。
func writeFrame(c net.Conn, msgType string, channelID uint32, body []byte) error {
	hdr, err := opcua.EncodeHeader(opcua.MessageHeader{
		MessageType: msgType, ChunkType: opcua.ChunkFinal,
		MessageSize: uint32(opcua.HeaderSize + len(body)), ChannelId: channelID,
	})
	if err != nil {
		return err
	}
	frame := append(append([]byte{}, hdr...), body...)
	_, err = c.Write(frame)
	return err
}

func stripHeader(b []byte) []byte {
	if len(b) >= opcua.HeaderSize {
		return b[opcua.HeaderSize:]
	}
	return b
}
