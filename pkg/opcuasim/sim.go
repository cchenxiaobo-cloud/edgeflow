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
	"bytes"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"

	"edgeflow/pkg/log"
	"edgeflow/pkg/opcua"
)

// fmtSscanf 解析字符串开头浮点（dataValueToFloat 用）。
func fmtSscanf(s string, f *float64) error {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = v
	return nil
}

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

// WithIdentity 为模拟器注入服务端身份（v0.28.1 opt-in）：配置后模拟器
// 接受 Basic256Sha256 OPN（解封/验签/密钥派生/加密响应）；不配置时保持
// v0.28.0 语义——非 None 策略显式拒绝（ERR Bad_SecurityPolicyRejected），
// v0280 冻结测试零改动。
func WithIdentity(cert *x509.Certificate, key *rsa.PrivateKey) Option {
	return func(s *Simulator) { s.serverCert, s.serverKey = cert, key }
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

	// serverCert/serverKey 是 v0.28.1 opt-in 服务端身份（WithIdentity）：
	// 均非 nil 时启用 Basic256Sha256 OPN 对等处理。
	serverCert *x509.Certificate
	serverKey  *rsa.PrivateKey

	mu        sync.Mutex
	connCount int
	stopping  bool

	sessMu   sync.Mutex
	sessions map[*connSession]struct{}

	temp     float64
	hum      float64
	press    float64
	setpoint float64
	stopCh   chan struct{}

	// PRT-23：优雅退出——Stop 等待 fluctuate/acceptLoop/全部连接
	// goroutine 结束，测试频繁启停不再短暂堆积。
	wg sync.WaitGroup
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
		sessions: make(map[*connSession]struct{}),
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
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		s.fluctuate()
	}()
	go func() {
		defer s.wg.Done()
		s.acceptLoop()
	}()
	log.Infof("OPC-UA 模拟器已启动（%s，端点 %s，点位 6 个）", s.addr, s.endpointURL)
	return nil
}

// Stop 停止监听与波动 goroutine，并等待全部后台 goroutine 退出（PRT-23）。
func (s *Simulator) Stop() error {
	s.mu.Lock()
	already := s.stopping
	s.stopping = true
	s.mu.Unlock()
	if already {
		return nil // 幂等：不重复等待（WaitGroup 复用会 panic）
	}
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
	// 关闭全部存活连接（解除 readFrame 阻塞），连接 goroutine 退出时
	// 自会从 sessions 删除自身。
	s.sessMu.Lock()
	for cs := range s.sessions {
		_ = cs.out.Close()
	}
	s.sessMu.Unlock()
	// 等待 fluctuate/acceptLoop/连接 goroutine 收敛（带上限防异常悬挂）。
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Infof("opcuasim: Stop 等待 goroutine 退出超时（5s），继续")
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
			s.notifyPush() // v0.15.0：订阅评估与通知投递
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
	s.wg.Add(1) // PRT-23：Stop 等待全部连接 goroutine
	defer s.wg.Done()
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
	asymHdr, rest, err := opcua.DecodeAsymmetricSecurityHeader(body)
	if err != nil {
		return
	}
	// v0.28.0/v0.28.1：策略分支处理。
	//   - None：原路径（逐字保留，v0280 冻结测试零改动）。
	//   - Basic256Sha256：仅当 WithIdentity 注入服务端身份后接受——解封、
	//     验签、密钥派生、加密响应；未注入身份时保持 v0.28.0 语义：显式
	//     拒绝（ERR Bad_SecurityPolicyRejected），不静默降级。
	switch {
	case asymHdr.SecurityPolicyURI == opcua.SecurityPolicyNoneURI:
		// 原 None 路径，见下方既有逻辑。
	case asymHdr.SecurityPolicyURI == opcua.SecurityPolicyBasic256Sha256URI && s.serverCert != nil && s.serverKey != nil:
		h, hok := s.handleB256OpenSecureChannel(c, asymHdr, rest)
		if !hok || h == nil {
			return
		}
		// B256 通道建立后的 MSG 循环：本段 MSG 仍为明文对称头（对称覆盖
		// v0.29.0），会话态携带派生密钥备用（仅存不用，避免 v0.29.0 重复握手）。
		cs := newConnSession(c, h.channelID, h.tokenID)
		cs.b256Keys = h.keys
		s.sessMu.Lock()
		s.sessions[cs] = struct{}{}
		s.sessMu.Unlock()
		defer func() {
			s.sessMu.Lock()
			delete(s.sessions, cs)
			s.sessMu.Unlock()
			cs.closeSession()
		}()
		for {
			var mt string
			mt, body, err = readFrame(c)
			if err != nil {
				return
			}
			switch mt {
			case opcua.MsgCloseSecureChannel:
				return
			case opcua.MsgSecureMessage:
				if !s.handleService(cs, c, cs.channelID, cs.tokenID, body) {
					return
				}
			default:
				return
			}
		}
	default:
		errBody, encErr := opcua.ErrorMessage{ErrorCode: 0x80550000, ErrorReason: "Bad_SecurityPolicyRejected"}.Encode()
		if encErr == nil {
			_ = writeFrame(c, opcua.MsgError, 0, stripHeader(errBody))
		}
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
	cs := newConnSession(c, channelID, tokenID)
	s.sessMu.Lock()
	s.sessions[cs] = struct{}{}
	s.sessMu.Unlock()
	defer func() {
		s.sessMu.Lock()
		delete(s.sessions, cs)
		s.sessMu.Unlock()
		cs.closeSession() // PRT-23：广播 done，悬挂 publish goroutine 立即退出
	}()
	for {
		msgType, body, err = readFrame(c)
		if err != nil {
			return
		}
		switch msgType {
		case opcua.MsgCloseSecureChannel:
			return
		case opcua.MsgSecureMessage:
			if !s.handleService(cs, c, channelID, tokenID, body) {
				return
			}
		default:
			return
		}
	}
}

func opnReqSeq() uint32 { return 1 }

// b256Handshake 是一次 Basic256Sha256 握手产物（per-conn，经函数返回值
// 传递，无共享状态；密钥不落盘）。
type b256Handshake struct {
	keys      *opcua.DerivedKeys
	channelID uint32
	tokenID   uint32
}

// clientCertPub 从 DER 证书解析 RSA 公钥（验签用）。
func clientCertPub(der []byte) (*rsa.PublicKey, bool) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, false
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	return pub, ok && pub != nil
}

// handleB256OpenSecureChannel 是 v0.28.1 服务端 Basic256Sha256 OPN 对等处理：
// 解封（RSA-OAEP-SHA1 本端私钥）→ 验签（客户端证书公钥）→ 解析 legacyBody
// → 派生密钥（ClientNonce+ServerNonce+双证书，与客户端同函数同输入）
// → 回送加密 OPN 响应。返回 false 表示已回 ERR/断开。
//
// 线格式（与客户端 sendOPN/recvOPN 镜像，见 securechannel.go 注释）：
//
//	请求剩余 = SignatureData{alg, sig(AsymHeader线编码||legacyBody)} +
//	           EncryptedData(RSA-OAEP(serverPub, ClientNonce(32B)||legacyBody))
//	legacyBody = SequenceHeader + 旧三字段 OpenSecureChannelRequest
//	响应体   = AsymHeader(策略+ServerCert+ClientCertThumbprint) + SignatureData
//	           + EncryptedData(RSA-OAEP(clientPub, ServerNonce(32B)||inner))
//	inner    = SequenceHeader + OpenSecureChannelResponse(ServerNonce=同前缀)
func (s *Simulator) handleB256OpenSecureChannel(c net.Conn, asymHdr opcua.AsymmetricSecurityHeader, rest []byte) (*b256Handshake, bool) {
	sendErr := func(code uint32, reason string) (*b256Handshake, bool) {
		errBody, encErr := opcua.ErrorMessage{ErrorCode: opcua.StatusCode(code), ErrorReason: reason}.Encode()
		if encErr == nil {
			_ = writeFrame(c, opcua.MsgError, 0, stripHeader(errBody))
		}
		return nil, false
	}
	// 身份一致性：ReceiverCertificateThumbprint 必须指向本端证书。
	if !bytes.Equal(asymHdr.ReceiverCertificateThumbprint, opcua.SHA1Thumbprint(s.serverCert.Raw)) {
		return sendErr(0x80130000, "Bad_SecurityChecksFailed")
	}
	sd, tail, err := opcua.DecodeSignatureData(rest)
	if err != nil {
		return sendErr(0x80070000, "Bad_DecodeError")
	}
	plain, err := opcua.UnwrapOPNBody(s.serverKey, tail)
	if err != nil {
		return sendErr(0x80130000, "BadSecurityChecksFailed")
	}
	if len(plain) < opcua.B256NonceLen {
		return sendErr(0x80070000, "Bad_DecodeError")
	}
	clientNonce, legacyBody := plain[:opcua.B256NonceLen], plain[opcua.B256NonceLen:]
	clientPub, ok := clientCertPub(asymHdr.SenderCertificate)
	if !ok {
		return sendErr(0x80130000, "BadSecurityChecksFailed")
	}
	asymWire, err := opcua.EncodeAsymmetricSecurityHeader(asymHdr)
	if err != nil {
		return sendErr(0x80070000, "Bad_DecodeError")
	}
	if !opcua.VerifyOPNBody(clientPub, asymWire, legacyBody, sd.Signature) {
		return sendErr(0x80130000, "BadApplicationSignatureInvalid")
	}
	_, lbRest, err := opcua.DecodeSequenceHeader(legacyBody)
	if err != nil {
		return sendErr(0x80070000, "Bad_DecodeError")
	}
	opnReq, err := opcua.DecodeOpenSecureChannelRequest(lbRest)
	if err != nil {
		return sendErr(0x80070000, "Bad_DecodeError")
	}
	if opnReq.RequestType != opcua.SecurityTokenRequestTypeIssue {
		return sendErr(0x800A0000, "BadNothingToDo")
	}
	serverNonce := make([]byte, opcua.B256NonceLen)
	if _, err := crand.Read(serverNonce); err != nil {
		return sendErr(0x80020000, "Bad_InternalError")
	}
	keys := opcua.DeriveKeys(clientNonce, serverNonce, asymHdr.SenderCertificate, s.serverCert.Raw)
	channelID, tokenID := uint32(rand.Int31n(1<<31-1)+1), uint32(rand.Int31n(1<<31-1)+1)
	hs := &b256Handshake{keys: keys, channelID: channelID, tokenID: tokenID}
	resp, err := opcua.EncodeOpenSecureChannelResponse(opcua.OpenSecureChannelResponse{
		Timestamp:             opcua.DateTimeFromTime(time.Now()),
		ServiceResult:         0,
		ServerProtocolVersion: 0,
		SecurityToken:         opcua.ChannelSecurityToken{ChannelID: channelID, TokenID: tokenID, CreatedAt: opcua.DateTimeFromTime(time.Now()), RevisedLifetime: 600000},
		ServerNonce:           serverNonce,
	})
	if err != nil {
		return sendErr(0x80080000, "BadEncodingError")
	}
	seqH, err := opcua.EncodeSequenceHeader(opcua.SequenceHeader{SequenceNumber: 1, RequestID: opnReqSeq()})
	if err != nil {
		return sendErr(0x80080000, "BadEncodingError")
	}
	inner := append(append(append([]byte{}, serverNonce...), seqH...), resp...)
	respAsym, err := opcua.EncodeAsymmetricSecurityHeader(opcua.AsymmetricSecurityHeader{
		SecurityPolicyURI:             opcua.SecurityPolicyBasic256Sha256URI,
		SenderCertificate:             s.serverCert.Raw,
		ReceiverCertificateThumbprint: opcua.SHA1Thumbprint(asymHdr.SenderCertificate),
	})
	if err != nil {
		return sendErr(0x80080000, "BadEncodingError")
	}
	blob, err := opcua.WrapOPNBody(clientPub, inner)
	if err != nil {
		return sendErr(0x80020000, "Bad_InternalError")
	}
	respSig, err := opcua.SignOPNBody(s.serverKey, respAsym, inner[opcua.B256NonceLen:])
	if err != nil {
		return sendErr(0x80020000, "Bad_InternalError")
	}
	sdWire, err := opcua.EncodeSignatureData(opcua.SignatureData{Algorithm: opcua.AsymmetricSignatureAlgorithmRsaSha1, Signature: respSig})
	if err != nil {
		return sendErr(0x80080000, "BadEncodingError")
	}
	frame := append(append(append([]byte{}, respAsym...), sdWire...), blob...)
	if err := writeFrame(c, opcua.MsgOpenSecureChannel, channelID, frame); err != nil {
		return nil, false
	}
	return hs, true
}

// handleService 解析 MSG 帧并分派服务请求。
func (s *Simulator) handleService(cs *connSession, c net.Conn, channelID, tokenID uint32, body []byte) bool {
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
		// 关会话时清理本连接订阅（服务端语义：DeleteSubscriptions 随行）
		cs.mu.Lock()
		cs.subs = make(map[uint32]*simSubscription)
		cs.mu.Unlock()
		return writeResp(out)
	}

	// ---- v0.15.0 新增分派（顺序：Publish 刚性最短 → CreateSubscription →
	// CreateMonitoredItems → DeleteSubscriptions → Browse；每级前置形状强校验）----
	if ok, perr := s.handlePublish(cs, sh, rest, writeResp); ok || perr != nil {
		return perr == nil
	}
	if ok, err := s.handleCreateSubscription(cs, sh, rest, writeResp); ok || err != nil {
		return err == nil
	}
	if ok, err := s.handleCreateMonitoredItems(cs, sh, rest, writeResp); ok || err != nil {
		return err == nil
	}
	if ok, err := s.handleDeleteSubscriptions(cs, sh, rest, writeResp); ok || err != nil {
		return err == nil
	}
	if ok, err := s.handleBrowse(sh, rest, writeResp); ok || err != nil {
		return err == nil
	}
	return false
}

// handlePublish 悬挂 Publish：登记后尝试即出队；无信封则等通知线程唤醒。
// 返回 (handled=true 表示本帧是 Publish 且已处理完毕；err 非 nil 表示连接级故障)。
func (s *Simulator) handlePublish(cs *connSession, sh opcua.SequenceHeader, rest []byte, writeResp func([]byte) bool) (bool, error) {
	if _, err := opcua.DecodePublishRequest(rest); err != nil {
		return false, nil // 试解失败：交给下一级
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.pending) > 0 {
		env := cs.pending[0]
		cs.pending = cs.pending[1:]
		env.resp.ResponseHeader = opcua.ResponseHeader{
			Timestamp:     opcua.DateTimeFromTime(time.Now()),
			RequestHandle: sh.RequestID,
			ServiceResult: 0,
		}
		out, oerr := opcua.EncodePublishResponse(env.resp)
		if oerr != nil {
			return true, oerr
		}
		return writeResp(out), nil
	}
	// 无积压：登记悬挂窗口（同一时刻仅一个，重复悬挂视为协议错误断连）
	if cs.pubWaiting {
		return true, fmt.Errorf("opcua: 双重悬挂 Publish")
	}
	cs.pubWaiting = true
	cs.pubReqID = sh.RequestID
	// 后台 goroutine 等新信封或连接关闭；PRT-23：不再 200ms 空转轮询
	// 30s，改 select 监听唤醒信（notifyPush/signalWake）与 done（连接
	// 退出即回收），并给 KeepAlive 兑现一个上限定时器。
	done := cs.done
	go func() {
		ticker := time.NewTicker(cs.publishingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-cs.wake:
				cs.mu.Lock()
				if !cs.pubWaiting || len(cs.pending) == 0 {
					cs.mu.Unlock()
					continue
				}
				env := cs.pending[0]
				cs.pending = cs.pending[1:]
				env.resp.ResponseHeader = opcua.ResponseHeader{
					Timestamp:     opcua.DateTimeFromTime(time.Now()),
					RequestHandle: sh.RequestID,
					ServiceResult: 0,
				}
				cs.pubWaiting = false
				out, oerr := opcua.EncodePublishResponse(env.resp)
				if oerr == nil {
					writeServerFrame(cs.out, opcua.MsgSecureMessage, cs.channelID, cs.tokenID, &cs.nextSeqHdr, sh.RequestID, out)
				}
				cs.mu.Unlock()
				return
			case <-ticker.C:
				// KeepAlive 兑现：悬挂超一个发布周期无信封则回空通知
				cs.mu.Lock()
				if !cs.pubWaiting {
					cs.mu.Unlock()
					return
				}
				cs.pubWaiting = false
				ka := opcua.PublishResponse{
					ResponseHeader: opcua.ResponseHeader{
						Timestamp:     opcua.DateTimeFromTime(time.Now()),
						RequestHandle: sh.RequestID,
						ServiceResult: 0,
					},
				}
				out, oerr := opcua.EncodePublishResponse(ka)
				if oerr == nil {
					writeServerFrame(cs.out, opcua.MsgSecureMessage, cs.channelID, cs.tokenID, &cs.nextSeqHdr, sh.RequestID, out)
				}
				cs.mu.Unlock()
				return
			}
		}
	}()
	return true, nil
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
