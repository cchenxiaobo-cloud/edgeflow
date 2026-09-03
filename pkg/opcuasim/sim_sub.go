package opcuasim

// 订阅引擎 + Browse 地址空间（v0.15.0）。
//
// 订阅模型（store-and-forward，SecurityPolicy None 匿名会话，单订阅每连接简化模型支持多订阅表）：
//   - 每连接独立会话态 connSession：subscriptionId → subscription
//   - fluctuate() 每步进后 notifyPush 评估全部订阅：
//     任一监测点位值变化 → 数据通知；连续 maxKeepAlive 步无变化 → KeepAlive 空通知
//   - 无悬挂 Publish 时通知入 pending 信封队列（≤32，满丢最旧非数据）
//   - 有悬挂 Publish（连接 goroutine 置 pubWaiting）时由其轮询队列写帧
//   - DeleteSubscriptions / 连接断开 → 全部清理
//
// Browse 最小目录（两级）：Objects(i=85,ns=0) ─Organizes→ "opcua-sim"(ns=2;i=5000) ─→ 6 变量节点

import (
	"net"
	"strconv"
	"sync"
	"time"

	"edgeflow/pkg/opcua"
)

// opcuaObjectNode 是模拟器对象目录节点（Browse 可发现入口，ns=2）。
const opcuaObjectNode = 5000

// objectsFolder 是标准 ObjectsFolder（ns=0;i=85）。
const objectsFolder = 85

// maxPendingEnvelopes 是单连接通知信封队列上限。
const maxPendingEnvelopes = 32

// simSubscription 是一条服务端订阅状态。
type simSubscription struct {
	id                   uint32
	items                []simItem
	publishingIntervalMs float64
	maxKeepAliveCount    uint32
	stepsSinceNotify     uint32
	lifetimeStepsLeft    uint32
	nextSeq              uint32 // 下一个通知序列号（从 1 起）
	lastValues           map[uint32]float64
}

type simItem struct {
	clientHandle uint32
	node         opcua.NodeId
}

// simEnvelope 是待投递的 PublishResponse 信封。
type simEnvelope struct {
	resp   opcua.PublishResponse
	isData bool // 数据通知（队列满时优先保留）
}

// connSession 聚合一个连接的会话态。
type connSession struct {
	mu         sync.Mutex
	subs       map[uint32]*simSubscription
	pending    []*simEnvelope // 无悬挂 publish 时暂存
	pubWaiting bool           // 有悬挂 Publish
	pubReqID   uint32         // 悬挂 Publish 的原始 RequestId（响应帧回填）
	out        net.Conn       // 出站帧通道（悬挂 publish 响应用）
	channelID  uint32
	tokenID    uint32

	nextSeqHdr         uint32        // 出站 SequenceNumber（MSG 序列头）
	nextSubID          uint32        // 下一个 subscriptionId
	lastCreated        uint32        // 最近创建的订阅 id（CreateMonitoredItems 归属）
	wake               chan struct{} // 队列有新信封信号（容量1）
	done               chan struct{} // PRT-23：连接退出信号（悬挂 publish goroutine 监听）
	publishingInterval time.Duration // PRT-23：KeepAlive 兑现周期（随订阅修订）
	closed             bool

	// b256Keys 是 v0.28.1 Basic256Sha256 握手派生密钥（WithIdentity 会话；
	// v0.29.0 MSG 对称覆盖接线用，本段仅持有不使用）。受 mu 保护。
	b256Keys *opcua.DerivedKeys
}

func newConnSession(out net.Conn, channelID, tokenID uint32) *connSession {
	return &connSession{
		subs:               make(map[uint32]*simSubscription),
		out:                out,
		channelID:          channelID,
		tokenID:            tokenID,
		nextSeqHdr:         2, // OPNF 用了 seq=1
		nextSubID:          100,
		wake:               make(chan struct{}, 1),
		done:               make(chan struct{}),
		publishingInterval: 5 * time.Second, // 默认 KeepAlive 兑现周期（订阅创建时修订）
	}
}

// closeSession 关闭会话：置 closed 标志并广播 done（悬挂 publish goroutine 退出）。
func (cs *connSession) closeSession() {
	cs.mu.Lock()
	if cs.closed {
		cs.mu.Unlock()
		return
	}
	cs.closed = true
	close(cs.done)
	cs.mu.Unlock()
}

// signalWake 非阻塞唤醒发布泵。
func (cs *connSession) signalWake() {
	select {
	case cs.wake <- struct{}{}:
	default:
	}
}

// enqueue 通知入队（满时丢最旧非数据信封）。返回当前队列长度。
func (cs *connSession) enqueue(env *simEnvelope) int {
	if len(cs.pending) >= maxPendingEnvelopes {
		for i, e := range cs.pending {
			if !e.isData {
				cs.pending = append(cs.pending[:i], cs.pending[i+1:]...)
				break
			}
		}
	}
	if len(cs.pending) < maxPendingEnvelopes {
		cs.pending = append(cs.pending, env)
	}
	return len(cs.pending)
}

// tryDispatch 若有悬挂 publish 则取队首信封立即写帧。调用方持 cs.mu。
func (cs *connSession) tryDispatchLocked(reqID uint32) {
	_ = reqID
	if !cs.pubWaiting || len(cs.pending) == 0 {
		return
	}
	env := cs.pending[0]
	cs.pending = cs.pending[1:]
	env.resp.RequestHandle = cs.pubReqID
	env.resp.Timestamp = opcua.DateTimeFromTime(time.Now())
	body, err := opcua.EncodePublishResponse(env.resp)
	if err != nil {
		return
	}
	writeServerFrame(cs.out, opcua.MsgSecureMessage, cs.channelID, cs.tokenID, &cs.nextSeqHdr, cs.pubReqID, body)
	cs.pubWaiting = false
}

// writeServerFrame 写一条完整 MSG 帧（8 字节头+对称头+序列头+body）。
// 仅在持有 cs.mu 时调用。
func writeServerFrame(c net.Conn, msgType string, channelID, tokenID uint32, nextSeqHdr *uint32, reqID uint32, body []byte) {
	sym, err := opcua.EncodeSymmetricSecurityHeader(opcua.SymmetricSecurityHeader{TokenID: tokenID})
	if err != nil {
		return
	}
	seqH, err := opcua.EncodeSequenceHeader(opcua.SequenceHeader{SequenceNumber: *nextSeqHdr, RequestID: reqID})
	if err != nil {
		return
	}
	*nextSeqHdr++
	payload := append(append(append([]byte{}, sym...), seqH...), body...)
	hdr, herr := opcua.EncodeHeader(opcua.MessageHeader{
		MessageType: msgType, ChunkType: opcua.ChunkFinal,
		MessageSize: uint32(opcua.HeaderSize + len(payload)), ChannelId: channelID,
	})
	if herr != nil {
		return
	}
	frame := append(append([]byte{}, hdr...), payload...)
	_ = writeFrameTimeout(c, frame, 5*time.Second)
}

// writeFrameTimeout 带超时写整帧（opcuasim 内部：构造 MSG 帧头+body）。
func writeFrameTimeout(c net.Conn, msgBody []byte, timeout time.Duration) error {
	if dl := c.SetWriteDeadline(time.Now().Add(timeout)); dl != nil {
		_ = c.SetWriteDeadline(time.Time{})
	}
	defer func() { _ = c.SetWriteDeadline(time.Time{}) }()
	_, err := c.Write(msgBody)
	return err
}

// notifyPush 由 fluctuate 步进调用：评估所有连接会话。
func (s *Simulator) notifyPush() {
	s.sessMu.Lock()
	sessions := make([]*connSession, 0, len(s.sessions))
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessMu.Unlock()

	now := time.Now()
	for _, cs := range sessions {
		cs.mu.Lock()
		if cs.closed {
			cs.mu.Unlock()
			continue
		}
		for _, sub := range cs.subs {
			sub.lifetimeStepsLeft--
			if sub.lifetimeStepsLeft <= 0 {
				continue
			}
			changed := false
			for _, it := range sub.items {
				dv := s.readNode(it.node)
				fv, ok := dataValueToFloat(dv)
				if !ok {
					continue
				}
				if last, seen := sub.lastValues[it.clientHandle]; !seen || fv != last {
					sub.lastValues[it.clientHandle] = fv
					changed = true
				}
			}
			if changed {
				sub.stepsSinceNotify = 0
				sub.nextSeq++
				msg := s.buildDataChange(sub, now)
				resp := opcua.PublishResponse{
					SubscriptionId:      sub.id,
					NotificationMessage: msg,
				}
				cs.enqueue(&simEnvelope{resp: resp, isData: true})
			} else {
				sub.stepsSinceNotify++
				if sub.maxKeepAliveCount > 0 && sub.stepsSinceNotify >= sub.maxKeepAliveCount {
					sub.stepsSinceNotify = 0
					sub.nextSeq++
					cs.enqueue(&simEnvelope{
						resp:   opcua.PublishResponse{SubscriptionId: sub.id, NotificationMessage: keepAliveMessage(sub.nextSeq, now)},
						isData: false,
					})
				}
			}
		}
		for id, sub := range cs.subs {
			if sub.lifetimeStepsLeft <= 0 {
				delete(cs.subs, id)
			}
		}
		cs.tryDispatchLocked(cs.pubReqID)
		cs.mu.Unlock()
		cs.signalWake()
	}
}

// buildDataChange 构造数据变更 NotificationMessage。
func (s *Simulator) buildDataChange(sub *simSubscription, now time.Time) opcua.NotificationMessage {
	msg := opcua.NotificationMessage{SequenceNumber: sub.nextSeq, PublishTime: opcua.DateTimeFromTime(now)}
	dvs := make([]opcua.MonitoredItemNotification, 0, len(sub.items))
	for _, it := range sub.items {
		dv := s.readNode(it.node)
		dvs = append(dvs, opcua.MonitoredItemNotification{ClientHandle: it.clientHandle, Value: dv})
	}
	msg.NotificationData = append(msg.NotificationData, opcua.NotificationData{
		Kind:       opcua.NotificationDataChange,
		DataChange: dvs,
	})
	return msg
}

// keepAliveMessage 构造空通知（KeepAlive：仅推进序列号）。
func keepAliveMessage(seq uint32, now time.Time) opcua.NotificationMessage {
	return opcua.NotificationMessage{SequenceNumber: seq, PublishTime: opcua.DateTimeFromTime(now)}
}

// dataValueToFloat 提取 DataValue 数值（与 mapper 的 variantToFloat 同口径的简化版）。
func dataValueToFloat(dv opcua.DataValue) (float64, bool) {
	if dv.Value == nil || dv.Status != nil && !dv.Status.IsGood() {
		return 0, false
	}
	v := dv.Value
	if v == nil {
		return 0, false
	}
	switch t := any(v.Value).(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int64:
		return float64(t), true
	case int32:
		return float64(t), true
	case int16:
		return float64(t), true
	case int8:
		return float64(t), true
	case uint64:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint16:
		return float64(t), true
	case byte:
		return float64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	return 0, false
}

// ---------------------------------------------------------------------
// 服务分派处理器（v0.15.0）：CreateSubscription / CreateMonitoredItems /
// DeleteSubscriptions / Browse。试解失败一律返回 (false, nil) 交还调用方。
// ---------------------------------------------------------------------

// handleCreateSubscription 创建订阅（单连接多订阅表）。修订值：
// publishingInterval=模拟器步进周期；maxKeepAlive=20 步；lifetime=60 步。
func (s *Simulator) handleCreateSubscription(cs *connSession, sh opcua.SequenceHeader, rest []byte, writeResp func([]byte) bool) (bool, error) {
	if _, err := opcua.DecodeCreateSubscriptionRequest(rest); err != nil {
		return false, nil
	}
	s.mu.Lock()
	interval := s.step.Seconds() * 1000
	s.mu.Unlock()
	cs.mu.Lock()
	subID := cs.nextSubID
	cs.nextSubID++
	cs.subs[subID] = &simSubscription{
		id:                   subID,
		publishingIntervalMs: interval,
		maxKeepAliveCount:    20,
		lifetimeStepsLeft:    60,
		lastValues:           make(map[uint32]float64),
	}
	// 记录最近创建的订阅，供 CreateMonitoredItems 归属（单请求创建后紧随）
	cs.lastCreated = subID
	// PRT-23：KeepAlive 兑现周期随订阅修订值刷新（下限 200ms 防零值/过小）
	if iv := time.Duration(interval) * time.Millisecond; iv >= 200*time.Millisecond {
		cs.publishingInterval = iv
	}
	cs.mu.Unlock()

	resp, err := opcua.EncodeCreateSubscriptionResponse(opcua.CreateSubscriptionResponse{
		ResponseHeader:            opcua.ResponseHeader{Timestamp: opcua.DateTimeFromTime(time.Now()), RequestHandle: sh.RequestID},
		SubscriptionId:            subID,
		RevisedPublishingInterval: interval,
		RevisedLifetimeCount:      60,
		RevisedMaxKeepAliveCount:  20,
	})
	if err != nil {
		return true, err
	}
	return writeResp(resp), nil
}

// handleCreateMonitoredItems 登记监测项（挂到最近创建的订阅上）。
func (s *Simulator) handleCreateMonitoredItems(cs *connSession, sh opcua.SequenceHeader, rest []byte, writeResp func([]byte) bool) (bool, error) {
	req, err := opcua.DecodeCreateMonitoredItemsRequest(rest)
	if err != nil || len(req.ItemsToCreate) == 0 {
		return false, nil
	}
	cs.mu.Lock()
	sub, ok := cs.subs[req.SubscriptionId]
	if !ok {
		sub, ok = cs.subs[cs.lastCreated]
	}
	if ok {
		for _, it := range req.ItemsToCreate {
			item := simItem{clientHandle: it.ClientHandle, node: it.NodeId}
			sub.items = append(sub.items, item)
			// 播种当前值为基线：下一步起的变化才会通知
			if dv := s.readNode(item.node); dv.Value != nil {
				if fv, ok2 := dataValueToFloat(dv); ok2 {
					sub.lastValues[item.clientHandle] = fv
				}
			}
		}
		sub.lifetimeStepsLeft = 60
	}
	cs.mu.Unlock()

	resp := opcua.CreateMonitoredItemsResponse{
		ResponseHeader: opcua.ResponseHeader{Timestamp: opcua.DateTimeFromTime(time.Now()), RequestHandle: sh.RequestID},
	}
	for range req.ItemsToCreate {
		resp.Results = append(resp.Results, opcua.MonitoredItemCreateResultDetail{
			MonitoredItemId: 0,
			StatusCode:      0,
		})
	}
	out, err := opcua.EncodeCreateMonitoredItemsResponse(resp)
	if err != nil {
		return true, err
	}
	return writeResp(out), nil
}

// handleDeleteSubscriptions 删除订阅并回结果。
func (s *Simulator) handleDeleteSubscriptions(cs *connSession, sh opcua.SequenceHeader, rest []byte, writeResp func([]byte) bool) (bool, error) {
	req, err := opcua.DecodeDeleteSubscriptionsRequest(rest)
	if err != nil {
		return false, nil
	}
	cs.mu.Lock()
	for _, id := range req.SubscriptionIds {
		delete(cs.subs, id)
	}
	cs.mu.Unlock()
	resp := opcua.DeleteSubscriptionsResponse{
		ResponseHeader: opcua.ResponseHeader{Timestamp: opcua.DateTimeFromTime(time.Now()), RequestHandle: sh.RequestID},
	}
	for range req.SubscriptionIds {
		resp.Results = append(resp.Results, 0)
	}
	out, err := opcua.EncodeDeleteSubscriptionsResponse(resp)
	if err != nil {
		return true, err
	}
	return writeResp(out), nil
}

// handleBrowse 两级最小目录：
//
//	Objects(i=85,ns=0) → opcua-sim(ns=2;i=5000) → 6 变量节点
func (s *Simulator) handleBrowse(sh opcua.SequenceHeader, rest []byte, writeResp func([]byte) bool) (bool, error) {
	req, err := opcua.DecodeBrowseRequest(rest)
	if err != nil || len(req.NodesToBrowse) == 0 {
		return false, nil
	}
	desc := req.NodesToBrowse[0]
	var refs []opcua.ReferenceDescription
	appendVar := func(id uint32, name string) {
		refs = append(refs, opcua.ReferenceDescription{
			ReferenceTypeId: opcua.NewNodeID(0, 35),
			IsForward:       true,
			NodeId:          opcua.ExpandedNodeId{NodeId: opcua.NewNodeID(2, id)},
			BrowseName:      opcua.QualifiedName{Namespace: 2, Name: name},
			DisplayName:     opcua.NewLocalizedText(name),
			NodeClass:       opcua.NodeClassVariable,
		})
	}
	target := desc.NodeId
	switch {
	case target.Type == opcua.NodeIDNumeric && target.Numeric == objectsFolder:
		refs = append(refs, opcua.ReferenceDescription{
			ReferenceTypeId: opcua.NewNodeID(0, 35),
			IsForward:       true,
			NodeId:          opcua.ExpandedNodeId{NodeId: opcua.NewNodeID(2, opcuaObjectNode)},
			BrowseName:      opcua.QualifiedName{Namespace: 2, Name: "opcua-sim"},
			DisplayName:     opcua.NewLocalizedText("opcua-sim"),
			NodeClass:       opcua.NodeClassObject,
		})
	case target.Type == opcua.NodeIDNumeric && target.Numeric == opcuaObjectNode:
		appendVar(NodeTemperature, "temperature")
		appendVar(NodeHumidity, "humidity")
		appendVar(NodePressure, "pressure")
		appendVar(NodeRunning, "running")
		appendVar(NodeLabel, "label")
		appendVar(NodeSetpoint, "setpoint")
	default:
		refs = nil // 未知节点：空引用数组 + Good
	}

	resp := opcua.BrowseResponse{
		ResponseHeader: opcua.ResponseHeader{Timestamp: opcua.DateTimeFromTime(time.Now()), RequestHandle: sh.RequestID},
	}
	resp.Results = append(resp.Results, opcua.BrowseResult{
		Status:     0,
		References: refs,
	})
	out, err := opcua.EncodeBrowseResponse(resp)
	if err != nil {
		return true, err
	}
	return writeResp(out), nil
}
