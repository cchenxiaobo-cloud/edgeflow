package opcua

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
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

	// ---- 泵模式状态（v0.15.0，首次 Subscribe 后启用）----
	mu         sync.Mutex
	waiters    map[uint32]chan []byte        // 严格配对响应等待表（reqID→body）
	pubReq     uint32                        // 在途 Publish 请求 id（同一时刻至多一个；0=无）
	subId      uint32                        // 当前订阅 id（0=无订阅）
	lastPubSeq uint32                        // 最近收到的通知序列号（0=未收过）
	acks       []SubscriptionAcknowledgement // 待随下一个 Publish 捎带的确认
	pubCh      chan PublishResult            // 数据/状态通知出口（缓冲 16）
	pumpOnce   sync.Once
	pubChOnce  sync.Once // PRT-04：pubCh 关闭防双关（pumpLoop 收尾与 stopPump 共用）
	pumping    bool
	pending    map[uint32][]byte // 无主帧兜底缓冲（RequestId→body，防先到后登记竞态）

	// ---- 自动续期（v0.30.0，opt-in OpenSecureChannelOptions.AutoRenewRatio）----
	renewMu        sync.Mutex    // 序列化自动/显式 Renew（同 roundTrip 通道互斥）
	renewDone      chan struct{} // Close 关闭 → 自动续期循环退出
	renewCloseOnce sync.Once
	scLifetime0    time.Duration // 初始令牌寿命（open 时快照，仅启动前写入）
}

// PublishResult 是一条推送到订阅方的通知。
type PublishResult struct {
	SubscriptionId uint32
	SequenceNumber uint32
	KeepAlive      bool                        // 空通知（仅序号推进）
	DataChange     []MonitoredItemNotification // Kind==DataChange 时非空
	StatusChange   StatusCode                  // Kind==StatusChange 时有效
	IsStatusChange bool
}

// Open 连接指定端点并完成全链路握手，返回就绪客户端。
func Open(endpoint string, timeout time.Duration) (*Client, error) {
	return open(endpoint, timeout, OpenSecureChannelOptions{})
}

// OpenWithOptions 是 v0.28.0 引入的安全策略可选 Open：除 OPN 策略协商外
// 与 Open 行为一致。opts 零值 = Open 的逐字等价（SecurityPolicy None）。
// Basic256Sha256 下要求 ClientCert/ClientKey/ServerCert 完整，本地校验
// 失败直接拒绝（不发起网络连接）；未知策略同样拒绝。
func OpenWithOptions(endpoint string, timeout time.Duration, opts OpenSecureChannelOptions) (*Client, error) {
	if _, err := opts.validateSecurityOptions(); err != nil {
		return nil, err
	}
	return open(endpoint, timeout, opts)
}

// open 是 Open/OpenWithOptions 的共享实现（v0.28.0 抽取，None 线上行为不变）。
func open(endpoint string, timeout time.Duration, opts OpenSecureChannelOptions) (*Client, error) {
	if timeout <= 0 {
		timeout = DefaultClientTimeout
	}
	conn, err := Dial(endpoint)
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = conn.Close() }
	sc, err := conn.OpenSecureChannelWith(timeout, opts)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("opcua: OpenSecureChannel: %w", err)
	}
	c := &Client{sc: sc, endpoint: endpoint, timeout: timeout}
	// v0.30.0：自动续期——仅 Basic256Sha256 通道启动；None 保持 v0.29.0 行为。
	c.renewDone = make(chan struct{})
	if opts.AutoRenewRatio > 0 && sc.encrypted() {
		c.scLifetime0 = sc.Lifetime()
		go c.autoRenewLoop(opts.AutoRenewRatio)
	}
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
	nonce, err := randomNonce(32)
	if err != nil {
		return fmt.Errorf("opcua: 生成 ClientNonce 失败: %w", err)
	}
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
		ClientNonce:             nonce,
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
	body, err := c.roundTrip(e.buf)
	if err != nil {
		return nil, fmt.Errorf("opcua: send Read: %w", err)
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
	body, err := c.roundTrip(e.buf)
	if err != nil {
		return 0, fmt.Errorf("opcua: send Write: %w", err)
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
// 存在订阅时先停泵、删除订阅（服务端会随删除推送 StatusChange，由丢弃方消费），
// 再关会话。
func (c *Client) Close() error {
	if c.sc == nil {
		return nil
	}
	// v0.30.0：先收口自动续期循环（防止循环在通道关闭后继续发请求）。
	if c.renewDone != nil {
		c.renewCloseOnce.Do(func() { close(c.renewDone) })
	}
	c.mu.Lock()
	pumping := c.pumping
	subId := c.subId
	c.mu.Unlock()
	if pumping {
		c.stopPump()
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
		if subId != 0 {
			// 泵已停；直接同步发送删除并吞响应（尽力而为）
			var de encoder
			delReq := DeleteSubscriptionsRequest{
				RequestHeader: RequestHeader{
					AuthenticationToken: c.authTok,
					Timestamp:           DateTimeFromTime(time.Now()),
					RequestHandle:       c.sc.nextReqID(),
				},
				SubscriptionIds: []uint32{subId},
			}
			if delReq.encodeUA(&de) == nil {
				if reqID, err := c.sc.sendSecure(de.buf); err == nil {
					_, _ = c.sc.recvSecure(reqID, c.timeout)
				}
			}
		}
		if reqID, err := c.sc.sendSecure(e.buf); err == nil {
			_, _ = c.sc.recvSecure(reqID, c.timeout)
		}
	}
	return c.sc.Close()
}

// randomNonce 生成指定长度的随机字节（32 字节客户端 nonce）。
// PRT-06：crypto/rand 失败必须报错中止，禁止降级为时间戳派生
// （可预测 nonce 在启用加密策略时是严重漏洞，且违反“rand 失败即报错”统一策略）。
func randomNonce(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("opcua: crypto/rand 读取失败: %w", err)
	}
	return b, nil
}

// ---------------------------------------------------------------------
// 订阅 API（v0.15.0 高层入口）
// ---------------------------------------------------------------------

// deleteSubBestEffort 尽力删除服务端订阅（PRT-10：Subscribe 失败清理路径
// 专用）。忽略一切错误：清理失败时订阅随 lifetimeCount 自然过期，
// 此处不掩盖原始失败原因；探针可观测。
func (c *Client) deleteSubBestEffort(subId uint32) {
	if subId == 0 {
		return
	}
	var e encoder
	req := DeleteSubscriptionsRequest{
		RequestHeader: RequestHeader{
			AuthenticationToken: c.authTok,
			Timestamp:           DateTimeFromTime(time.Now()),
			RequestHandle:       c.sc.nextReqID(),
		},
		SubscriptionIds: []uint32{subId},
	}
	if err := req.encodeUA(&e); err != nil {
		return
	}
	if body, err := c.roundTrip(e.buf); err == nil {
		var d decoder
		d.b = body
		if resp, derr := decodeDeleteSubscriptionsResponse(&d); derr == nil && !resp.ServiceResult.IsGood() {
			clientProbe("deleteSubBestEffort: 订阅 %d 清除服务失败 %s", subId, resp.ServiceResult)
		}
	}
}

// Subscribe 创建订阅并把 nodes 全部登记为 Reporting 的 DataChange 监测项。
// 返回可直接消费通知的通道（首次调用时启动读泵）。
// samplingInterval<=0 时用服务器修订值；queueSize 传 1（状态量语义：只要最新）。
func (c *Client) Subscribe(nodes []NodeId, publishingIntervalMs float64) (<-chan PublishResult, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("opcua: Subscribe: 节点列表为空")
	}
	c.ensurePump()
	var e encoder
	csReq := CreateSubscriptionRequest{
		RequestHeader: RequestHeader{
			AuthenticationToken: c.authTok,
			Timestamp:           DateTimeFromTime(time.Now()),
			RequestHandle:       c.sc.nextReqID(),
		},
		RequestedPublishingInterval: publishingIntervalMs,
		RequestedLifetimeCount:      60,
		RequestedMaxKeepAliveCount:  20,
		MaxNotificationsPerPublish:  0,
		PublishingEnabled:           true,
	}
	if err := csReq.encodeUA(&e); err != nil {
		return nil, err
	}
	body, err := c.roundTrip(e.buf)
	if err != nil {
		return nil, fmt.Errorf("opcua: CreateSubscription: %w", err)
	}
	var d decoder
	d.b = body
	csResp, err := decodeCreateSubscriptionResponse(&d)
	if err != nil {
		return nil, fmt.Errorf("opcua: decode CreateSubscription response: %w", err)
	}
	if !csResp.ServiceResult.IsGood() {
		return nil, fmt.Errorf("opcua: CreateSubscription 服务失败: %s", csResp.ServiceResult)
	}

	var me encoder
	items := make([]ItemToCreate, 0, len(nodes))
	for i, n := range nodes {
		items = append(items, ItemToCreate{
			NodeId:             n,
			ClientHandle:       uint32(i + 1), // 1-based 句柄（0 保留）
			SamplingIntervalMs: csResp.RevisedPublishingInterval,
			QueueSize:          1,
		})
	}
	miReq := CreateMonitoredItemsRequest{
		RequestHeader: RequestHeader{
			AuthenticationToken: c.authTok,
			Timestamp:           DateTimeFromTime(time.Now()),
			RequestHandle:       c.sc.nextReqID(),
		},
		SubscriptionId:     csResp.SubscriptionId,
		TimestampsToReturn: TimestampsToReturnNeither,
		ItemsToCreate:      items,
	}
	if err := miReq.encodeUA(&me); err != nil {
		// PRT-10：订阅已建，失败路径必须清理，防孤儿订阅直到 lifetime 过期。
		c.deleteSubBestEffort(csResp.SubscriptionId)
		return nil, err
	}
	mBody, err := c.roundTrip(me.buf)
	if err != nil {
		c.deleteSubBestEffort(csResp.SubscriptionId) // PRT-10
		return nil, fmt.Errorf("opcua: CreateMonitoredItems: %w", err)
	}
	var md decoder
	md.b = mBody
	miResp, err := decodeCreateMonitoredItemsResponse(&md)
	if err != nil {
		c.deleteSubBestEffort(csResp.SubscriptionId) // PRT-10
		return nil, fmt.Errorf("opcua: decode CreateMonitoredItems response: %w", err)
	}
	for _, r := range miResp.Results {
		if !r.StatusCode.IsGood() {
			c.deleteSubBestEffort(csResp.SubscriptionId) // PRT-10
			return nil, fmt.Errorf("opcua: 监测项创建失败: %s", r.StatusCode)
		}
	}

	c.mu.Lock()
	c.subId = csResp.SubscriptionId
	c.lastPubSeq = 0
	c.acks = nil
	if c.pubCh == nil {
		c.pubCh = make(chan PublishResult, 16)
	}
	out := c.pubCh
	c.mu.Unlock()

	// 悬挂首条 Publish：通知将由泵分发至 pubCh
	if err := c.sendPublish(); err != nil {
		c.deleteSubBestEffort(csResp.SubscriptionId) // PRT-10
		return nil, err
	}
	return out, nil
}

// sendPublish 发送一条悬挂 Publish（登记在途 reqID，等待服务端积压通知/KeepAlive）。
func (c *Client) sendPublish() error {
	c.mu.Lock()
	acks := c.acks
	c.acks = nil
	c.mu.Unlock()
	var e encoder
	req := PublishRequest{
		RequestHeader: RequestHeader{
			AuthenticationToken: c.authTok,
			Timestamp:           DateTimeFromTime(time.Now()),
			RequestHandle:       c.sc.nextReqID(),
		},
		SubscriptionAcks: acks,
	}
	if err := req.encodeUA(&e); err != nil {
		return err
	}
	reqID, err := c.sc.sendSecure(e.buf)
	if err != nil {
		return fmt.Errorf("opcua: send Publish: %w", err)
	}
	c.mu.Lock()
	c.pubReq = reqID
	c.mu.Unlock()
	return nil
}

// PubAck 补发悬挂 Publish（接收方每取走一条通知后调用一次，维持 publish 窗口）。
// 内部吞掉已累计的 ack 列表；无订阅时为 no-op。
func (c *Client) PubAck() error {
	c.mu.Lock()
	has := c.subId != 0 && c.pumping
	c.mu.Unlock()
	if !has {
		return nil
	}
	return c.sendPublish()
}

// DeleteSubscription 显式删除订阅并保持泵可用（供 gap 重建路径复用 Client）。
func (c *Client) DeleteSubscription() error {
	c.mu.Lock()
	subId := c.subId
	c.subId = 0
	c.acks = nil
	c.lastPubSeq = 0
	c.mu.Unlock()
	if subId == 0 {
		return nil
	}
	var e encoder
	req := DeleteSubscriptionsRequest{
		RequestHeader: RequestHeader{
			AuthenticationToken: c.authTok,
			Timestamp:           DateTimeFromTime(time.Now()),
			RequestHandle:       c.sc.nextReqID(),
		},
		SubscriptionIds: []uint32{subId},
	}
	if err := req.encodeUA(&e); err != nil {
		return err
	}
	body, err := c.roundTrip(e.buf)
	if err != nil {
		return fmt.Errorf("opcua: DeleteSubscriptions: %w", err)
	}
	var d decoder
	d.b = body
	resp, err := decodeDeleteSubscriptionsResponse(&d)
	if err != nil {
		return fmt.Errorf("opcua: decode DeleteSubscriptions response: %w", err)
	}
	if !resp.ServiceResult.IsGood() {
		return fmt.Errorf("opcua: DeleteSubscriptions 服务失败: %s", resp.ServiceResult)
	}
	return nil
}

// ---------------------------------------------------------------------
// 泵模式（v0.15.0）：唯一读 goroutine + 请求分发器。未启用时行为与
// v0.14.0 完全一致（recvSecure 严格配对）。
// ---------------------------------------------------------------------

// Renew 请求安全令牌续期（v0.29.0，仅 Basic256Sha256 加密通道）：在现有
// 加密通道内发送 ClientNonce(32B) ‖ OpenSecureChannelRequest{RENEW}
// （44B 形状指纹网关，见 spec 0002），服务端以旧出站组密封响应（新
// TokenID + 新 ServerNonce）；本端校验后用新 nonce 对派生新密钥组并原子
// 换组（旧组保留作在途帧回退）。复用 roundTrip 泵机制，订阅活跃时安全。
func (c *Client) Renew(timeout time.Duration) error {
	c.renewMu.Lock()
	defer c.renewMu.Unlock()
	_, err := c.renewLocked(timeout)
	return err
}

// renewLocked 是 Renew 的共享态安全实现（v0.30.0 重构）：调用方须持
// renewMu；不改写共享 c.timeout（原实现无锁改写与并发 roundTrip 的
// 读取构成数据竞争，-race 下由自动续期测试暴露），超时经参数传入
// roundTripTimeout；返回服务端修订寿命（毫秒）供自动续期循环重算。
func (c *Client) renewLocked(timeout time.Duration) (float64, error) {
	ks := c.sc.curKeys()
	if ks.keys == nil {
		return 0, errors.New("opcua: Renew 仅支持 Basic256Sha256 加密通道")
	}
	nonce := make([]byte, B256NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return 0, fmt.Errorf("opcua: 生成续期 ClientNonce 失败: %w", err)
	}
	var oe encoder
	if err := (OpenSecureChannelRequest{
		ClientProtocolVersion: 0,
		RequestType:           SecurityTokenRequestTypeRenew,
		RequestedLifetime:     DefaultRequestedLifetime,
	}).encodeUA(&oe); err != nil {
		return 0, err
	}
	body := append(append([]byte{}, nonce...), oe.buf...)
	respWire, err := c.roundTripTimeout(body, timeout)
	if err != nil {
		return 0, fmt.Errorf("opcua: Renew 往返失败: %w", err)
	}
	var d decoder
	d.b = respWire
	resp, err := decodeOpenSecureChannelResponse(&d)
	if err != nil {
		return 0, fmt.Errorf("opcua: Renew 响应解码失败: %w", err)
	}
	if !resp.ServiceResult.IsGood() {
		return 0, fmt.Errorf("opcua: Renew 服务失败: %s", resp.ServiceResult)
	}
	if resp.SecurityToken.RevisedLifetime <= 0 {
		return 0, fmt.Errorf("opcua: Renew 响应 RevisedLifetime=%v 非法（须 > 0）", resp.SecurityToken.RevisedLifetime)
	}
	if resp.SecurityToken.TokenID == 0 {
		return 0, errors.New("opcua: Renew 响应 TokenID 非法")
	}
	if len(resp.ServerNonce) != B256NonceLen {
		return 0, fmt.Errorf("opcua: Renew 响应 ServerNonce 长度 %d 非 %d", len(resp.ServerNonce), B256NonceLen)
	}
	newKeys := DeriveKeys(nonce, resp.ServerNonce, c.sc.opts.ClientCert.Raw, c.sc.opts.ServerCert.Raw)
	c.sc.swapKeys(newKeys, resp.SecurityToken.TokenID)
	return resp.SecurityToken.RevisedLifetime, nil
}

// autoRenewLoop 在令牌寿命的 ratio 比例点自动触发 Renew（v0.30.0）。
// 续期失败按剩余寿命 1/10（下限 1s）退避重试，不中断订阅；Close 关闭
// renewDone 后循环退出。None 通道不启动本循环。
func (c *Client) autoRenewLoop(ratio float64) {
	cur := c.scLifetime0
	if cur <= 0 {
		return
	}
	retrying := false
	for {
		delay := time.Duration(float64(cur) * ratio)
		if retrying {
			delay = cur / 10
			if delay < time.Second {
				delay = time.Second
			}
		} else if delay < 50*time.Millisecond {
			delay = 50 * time.Millisecond
		}
		t := time.NewTimer(delay)
		select {
		case <-c.renewDone:
			t.Stop()
			return
		case <-t.C:
		}
		c.renewMu.Lock()
		life, err := c.renewLocked(0)
		c.renewMu.Unlock()
		if err != nil {
			if c.sc == nil || !c.sc.encrypted() {
				return // 通道已不可加密续期：退出（使用方负责重连）
			}
			retrying = true
			continue
		}
		retrying = false
		if life > 0 {
			cur = time.Duration(life) * time.Millisecond
		}
	}
}

// TokenID 返回当前生效的安全令牌 id（v0.29.0：Renew 换钥后为新令牌）。
func (c *Client) TokenID() uint32 { return c.sc.TokenID() }

// ProbeKeyMaterial 返回当前对称密钥组与通道/令牌 id（v0.29.0 传输级篡改
// 用例与诊断探针专用；业务路径禁止使用，密钥不落盘不打印）。
func (c *Client) ProbeKeyMaterial() (*DerivedKeys, uint32, uint32) {
	ks := c.sc.curKeys()
	return ks.keys, c.sc.channelId, ks.tokenID
}

// ProbeWriteRaw 写原始帧字节（测试探针：密封后篡改重放用）。
func (c *Client) ProbeWriteRaw(frame []byte) error {
	return c.sc.conn.WriteFrameRaw(frame)
}

// roundTrip 是严格配对调用的统一入口：发送、登记 waiter、收响应。
// 泵未启动时直接走 recvSecure；启动后经 waiters 表由 pump 分发。
func (c *Client) roundTrip(body []byte) ([]byte, error) {
	return c.roundTripTimeout(body, 0)
}

// roundTripTimeout 是 roundTrip 的超时参数化变体（v0.30.0）：timeout<=0
// 时读共享 c.timeout（open 后不再被写，读安全）；Renew 经此传入显式
// 超时而不改写共享态。
func (c *Client) roundTripTimeout(body []byte, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = c.timeout
	}
	reqID, err := c.sc.sendSecure(body)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	pumping := c.pumping
	var ch chan []byte
	if pumping {
		// 竞态兜底：响应可能先于本处登记到达并被泵暂存
		if b, ok := c.pending[reqID]; ok {
			delete(c.pending, reqID)
			c.mu.Unlock()
			return b, nil
		}
		ch = make(chan []byte, 1)
		if c.waiters == nil {
			c.waiters = make(map[uint32]chan []byte)
		}
		c.waiters[reqID] = ch
	}
	c.mu.Unlock()
	if !pumping {
		return c.sc.recvSecure(reqID, timeout)
	}
	select {
	case b := <-ch:
		return b, nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.waiters, reqID)
		// PRT-17：超时放弃的 waiter 对应 pending 条目顺带清理，
		// 防兜底表条目滞留至滚动覆盖或 Close。
		delete(c.pending, reqID)
		c.mu.Unlock()
		return nil, fmt.Errorf("opcua: 响应超时（reqID=%d，泵模式）", reqID)
	}
}

// ensurePump 启动唯一读 goroutine（幂等）。
func (c *Client) ensurePump() {
	c.pumpOnce.Do(func() {
		c.mu.Lock()
		c.pumping = true
		c.mu.Unlock()
		go c.pumpLoop()
	})
}

// stopPump 停止泵并清理等待者（Close 专用；通知出口关闭由 pumpLoop 收尾）。
func (c *Client) stopPump() {
	c.mu.Lock()
	if !c.pumping {
		c.mu.Unlock()
		return
	}
	c.pumping = false
	for id, ch := range c.waiters {
		close(ch) // 等待者收到 nil body 后报错返回
		delete(c.waiters, id)
	}
	c.pending = nil
	pubCh := c.pubCh
	c.pubCh = nil
	c.mu.Unlock()
	if pubCh != nil {
		c.pubChOnce.Do(func() { close(pubCh) }) // sync.Once 防双关（PRT-04）
	}
}

// pumpLoop 唯一读循环：帧 → 解头 → 分发（waiter/在途 Publish/丢弃）。
// v0.29.0：B256 通道先解封（openIncoming）再解析序列头；None 走原路径。
func (c *Client) pumpLoop() {
	defer func() {
		c.mu.Lock()
		c.pumping = false
		for id, ch := range c.waiters {
			close(ch)
			delete(c.waiters, id)
		}
		pubCh := c.pubCh
		c.pubCh = nil // 置空出口：下次 Subscribe 重建新通道，不复用已关闭通道
		c.mu.Unlock()
		// PRT-04：一切退出路径（连接级故障/stopPump）都关闭订阅出口，
		// 订阅方 range 可感知退出；sync.Once 防与 stopPump 双关。
		if pubCh != nil {
			c.pubChOnce.Do(func() { close(pubCh) })
		}
	}()
	for {
		c.mu.Lock()
		running := c.pumping
		c.mu.Unlock()
		if !running {
			return
		}
		msgType, body, err := c.readFrame()
		if err != nil {
			return // 连接级故障：waiters 由 defer 关闭报错
		}
		_ = msgType
		var ridge uint32
		var rbody []byte
		var ok bool
		if c.sc.encrypted() {
			plain, derr := c.sc.openIncoming(msgType, body)
			if derr != nil {
				return
			}
			var dd decoder
			dd.b = plain
			sh, serr := decodeSequenceHeader(&dd)
			if serr != nil {
				continue
			}
			ridge, rbody, ok = sh.RequestID, dd.b[dd.off:], true
		} else {
			ridge, rbody, ok = parseSecureFrame(body)
		}
		if !ok {
			continue
		}
		c.mu.Lock()
		ch, waiting := c.waiters[ridge]
		if waiting {
			delete(c.waiters, ridge)
			c.mu.Unlock()
			ch <- rbody // 缓冲 1，必不阻塞
			continue
		}
		isPub := c.pubReq != 0 && c.pubReq == ridge
		clientProbe("frame reqID=%d bodyLen=%d isPub=%v", ridge, len(rbody), isPub)
		if !isPub {
			// 认领者尚未登记（send→登记窗口）：暂存待 roundTrip 自取
			if c.pending == nil {
				c.pending = make(map[uint32][]byte)
			}
			if len(c.pending) < 64 {
				c.pending[ridge] = append([]byte{}, rbody...)
			} else {
				// PRT-07：兑底表满丢弃无主帧必须可观测（跳帧不再静默）。
				clientProbe("pump: pending 表满，丢弃无主帧 reqID=%d", ridge)
			}
			c.mu.Unlock()
			continue
		}
		c.pubReq = 0
		c.mu.Unlock()
		c.consumePublishFrame(rbody)
	}
}

// consumePublishFrame 解码 PublishResponse 并投递到 pubCh（泵专用）。
func (c *Client) consumePublishFrame(body []byte) {
	var d decoder
	d.b = body
	resp, err := decodePublishResponse(&d)
	if err != nil {
		clientProbe("consume 解码失败: %v", err)
		return
	}
	c.mu.Lock()
	// PRT-07：发布序号防重放——seq==0（非法，序号从 1 起）与
	// seq<=lastPubSeq（重复/回退：重放或服务器异常）直接丢弃，
	// 不投递、不生成 ack；lastPubSeq 不回退。
	// v0.29.0 修正：seq==0 且空通知 = 本仓 KeepAlive 兑现（PRT-23）。
	// 原静默丢弃会断裂发布长轮询（窗口靠消费方 PubAck 驱动，丢弃后无人
	// 重挂 → 订阅流永久静默，Renew 时序下必然触发）；改为不投递、不推进
	// lastPubSeq，但自动重挂发布窗口维持循环。
	seq := resp.NotificationMessage.SequenceNumber
	if seq == 0 {
		isKA := len(resp.NotificationMessage.NotificationData) == 0
		pumping, hasSub := c.pumping, c.subId != 0
		c.mu.Unlock()
		if isKA && pumping && hasSub {
			_ = c.sendPublish() // 重挂发布窗口（sendPublish 自持锁）
		} else {
			clientProbe("consume 丢弃非法序号 seq=0")
		}
		return
	}
	if c.lastPubSeq != 0 && seq <= c.lastPubSeq {
		c.mu.Unlock()
		clientProbe("consume 丢弃重复/回退序号 seq=%d last=%d", seq, c.lastPubSeq)
		return
	}
	pr := PublishResult{
		SubscriptionId: resp.SubscriptionId,
		SequenceNumber: seq,
	}
	gap := pr.SequenceNumber > c.lastPubSeq+1 && c.lastPubSeq != 0
	c.lastPubSeq = seq
	for _, nd := range resp.NotificationMessage.NotificationData {
		switch nd.Kind {
		case NotificationDataChange:
			pr.DataChange = append(pr.DataChange, nd.DataChange...)
		case NotificationStatusChange:
			pr.IsStatusChange = true
			pr.StatusChange = nd.StatusChange
		}
	}
	if len(resp.NotificationMessage.NotificationData) == 0 {
		pr.KeepAlive = true // 空通知：仅序号推进
	}
	c.acks = append(c.acks, SubscriptionAcknowledgement{SubscriptionId: resp.SubscriptionId, SequenceNumber: seq})
	out := c.pubCh
	c.mu.Unlock()
	_ = gap // gap 处理由订阅方（重建订阅）决定；此处仅透传
	if out == nil {
		return
	}
	select {
	case out <- pr:
	default: // 队列满：丢最旧保新
		select {
		case <-out:
		default:
		}
		select {
		case out <- pr:
		default:
		}
	}
}

// readFrame 读一条 MSG 帧（泵专用：不设 deadline 不限 wantReqID）。
func (c *Client) readFrame() (string, []byte, error) {
	msgType, body, err := c.sc.conn.ReadMessage()
	if err != nil {
		// PRT-16：泵路径同样容忍 Abort：跳过该帧继续泵循环，
		// 不再因对端弃报整条连接退出。返回零值帧让 pumpLoop 走
		// parseSecureFrame 失败→continue 的既有惯性路径。
		if errors.Is(err, ErrPeerAbort) {
			clientProbe("pump: peer abort frame, skip")
			return "", nil, nil
		}
		return "", nil, err
	}
	if msgType == MsgError {
		return "", nil, fmt.Errorf("opcua: server service error frame")
	}
	return msgType, body, nil
}

// parseSecureFrame 从 MSG 帧体剥出 RequestId 与服务 body。
func parseSecureFrame(body []byte) (uint32, []byte, bool) {
	var d decoder
	d.b = body
	if _, err := decodeSymmetricSecurityHeader(&d); err != nil {
		return 0, nil, false
	}
	sh, err := decodeSequenceHeader(&d)
	if err != nil {
		return 0, nil, false
	}
	return sh.RequestID, d.b[d.off:], true
}

func statusPtr(s uint32) *StatusCode {
	st := StatusCode(s)
	return &st
}

// SubscribeProbe 调试辅助：仅创建订阅+监测项+悬挂 Publish，不暴露通知消费语义。
// 与 Subscribe 相同链路但返回通道同时允许测试观察 pubReq 状态。非稳定 API。
func (c *Client) SubscribeProbe() (<-chan PublishResult, error) {
	return c.Subscribe([]NodeId{NewNodeID(2, 1001)}, 0)
}

// clientProbe 开发期探针（发布前移除；环境变量 OPCUA_CLIENT_PROBE=1 时输出）。
func clientProbe(format string, args ...any) {
	if os.Getenv("OPCUA_CLIENT_PROBE") == "1" {
		fmt.Printf("[client-probe] "+format+"\n", args...)
	}
}

// PubReqForProbe/LastPubSeqForProbe/AcksForProbe 调试探针（非稳定 API）。
func (c *Client) PubReqForProbe() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pubReq
}

func (c *Client) LastPubSeqForProbe() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPubSeq
}

func (c *Client) AcksForProbe() []SubscriptionAcknowledgement {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SubscriptionAcknowledgement{}, c.acks...)
}
