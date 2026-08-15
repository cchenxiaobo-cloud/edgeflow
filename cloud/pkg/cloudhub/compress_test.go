package cloudhub

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/protocol"

	"github.com/gorilla/websocket"
)

// readRaw 读取一条消息的原始帧字节（不做解码，供压缩协商断言用）。
func readRaw(t *testing.T, ws *websocket.Conn) []byte {
	t.Helper()
	if err := ws.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	_, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	return data
}

// sendRaw 发送一条消息帧（已编码，压缩/明文均可）。
func sendRaw(t *testing.T, ws *websocket.Conn, data []byte) {
	t.Helper()
	if err := ws.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("设置写超时失败: %v", err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("发送消息失败: %v", err)
	}
}

// registerWithCompression 发送携带压缩声明的 Register 并读取原始应答帧。
// 返回 (应答帧, 应答消息, 应答 payload)。
func registerWithCompression(t *testing.T, ws *websocket.Conn, nodeID, compression string) ([]byte, *protocol.Message, RegisterAckPayload) {
	t.Helper()
	m, err := protocol.NewMessage(protocol.TypeRegister, nodeID, "cloud", RegisterPayload{
		NodeID:          nodeID,
		Arch:            "arm64",
		OS:              "linux",
		EdgecoreVersion: "v0.1.0",
		CPU:             4,
		Memory:          8 << 30,
		Compression:     compression,
	})
	if err != nil {
		t.Fatalf("构造 Register 失败: %v", err)
	}
	sendRaw(t, ws, mustEncode(t, m))
	frame := readRaw(t, ws)
	ack, err := protocol.DecodeCompressed(frame)
	if err != nil {
		t.Fatalf("解析 RegisterAck 失败: %v", err)
	}
	var p RegisterAckPayload
	if err := ack.DecodePayload(&p); err != nil {
		t.Fatalf("解析 RegisterAck payload 失败: %v", err)
	}
	return frame, ack, p
}

// mustEncode 编码消息（失败即测试失败）。
func mustEncode(t *testing.T, m *protocol.Message) []byte {
	t.Helper()
	data, err := protocol.Encode(m)
	if err != nil {
		t.Fatalf("编码消息失败: %v", err)
	}
	return data
}

// bigPayload 构造大负载（> MinCompressBytes，可压缩），用于触发压缩路径。
func bigPayload(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for sb.Len() < protocol.MinCompressBytes*4 {
		sb.WriteString(`{"podName":"demo","namespace":"default","phase":"Running","replicas":3,"image":"nginx:1.27","message":"periodic reconcile heartbeat payload"},`)
	}
	return sb.String()
}

// TestCompressionNegotiation 验证压缩协商闭环（新边缘 ↔ 新云端）：
//   - Register 声明 gzip → RegisterAck 明文回带 compression=gzip（协商信道恒明文）
//   - 下发大消息 → 压缩帧（IsCompressed），小消息 → 明文（跳过规则）
//   - 上行压缩消息 → 云端解压并正常分发
func TestCompressionNegotiation(t *testing.T) {
	srv, url := newTestServer(t)
	ws := dial(t, url)

	// 注册：声明 gzip 能力
	frame, ack, p := registerWithCompression(t, ws, "edge-c1", "gzip")
	if protocol.IsCompressed(frame) {
		t.Fatalf("RegisterAck 应恒为明文（协商信道），实际收到压缩帧")
	}
	if !p.Accepted {
		t.Fatalf("注册被拒绝: %s", p.Message)
	}
	if p.Compression != "gzip" {
		t.Fatalf("RegisterAck 应回带 compression=gzip，实际 %q", p.Compression)
	}
	if ack.Source != "cloud" || ack.Target != "edge-c1" {
		t.Fatalf("RegisterAck 信封 = %s→%s，期望 cloud→edge-c1", ack.Source, ack.Target)
	}

	// 下发大消息 → 压缩帧
	down, err := protocol.NewMessage(protocol.TypePodSync, "cloud", "edge-c1", map[string]string{"data": bigPayload(t)})
	if err != nil {
		t.Fatalf("构造下发消息失败: %v", err)
	}
	if err := srv.SendToNode("edge-c1", down); err != nil {
		t.Fatalf("SendToNode 失败: %v", err)
	}
	got := readRaw(t, ws)
	if !protocol.IsCompressed(got) {
		t.Fatalf("已协商连接的下发消息应为压缩帧，实际明文 %d 字节", len(got))
	}
	gotMsg, err := protocol.DecodeCompressed(got)
	if err != nil {
		t.Fatalf("解压下发消息失败: %v", err)
	}
	if gotMsg.ID != down.ID || gotMsg.Type != protocol.TypePodSync {
		t.Fatalf("下发消息往返不一致: id=%q type=%q", gotMsg.ID, gotMsg.Type)
	}

	// 下发小消息 → 明文（跳过规则，对旧路径透明）
	small, err := protocol.NewMessage(protocol.TypeHeartbeatAck, "cloud", "edge-c1", HeartbeatAckPayload{NodeStatus: "Ready"})
	if err != nil {
		t.Fatalf("构造小消息失败: %v", err)
	}
	if err := srv.SendToNode("edge-c1", small); err != nil {
		t.Fatalf("SendToNode 失败: %v", err)
	}
	gotSmall := readRaw(t, ws)
	if protocol.IsCompressed(gotSmall) {
		t.Fatalf("小消息应跳过压缩（明文），实际压缩帧")
	}
	if _, err := protocol.Decode(gotSmall); err != nil {
		t.Fatalf("明文小消息解码失败: %v", err)
	}

	// 上行压缩消息 → 云端解压分发（PodStatus 大负载）
	reported := make(chan PodStatusPayload, 1)
	srv.SetPodStatusHandler(func(nodeID string, ps PodStatusPayload) {
		if nodeID != "edge-c1" {
			t.Errorf("PodStatus 回调 nodeID = %q，期望 edge-c1", nodeID)
		}
		reported <- ps
	})
	up, err := protocol.NewMessage(protocol.TypePodStatus, "edge-c1", "cloud", PodStatusPayload{
		NodeID:  "edge-c1",
		PodName: "demo",
		Phase:   "Running",
		Message: bigPayload(t),
	})
	if err != nil {
		t.Fatalf("构造上行消息失败: %v", err)
	}
	upFrame, err := protocol.EncodeCompressed(up)
	if err != nil {
		t.Fatalf("EncodeCompressed 失败: %v", err)
	}
	if !protocol.IsCompressed(upFrame) {
		t.Fatalf("测试前提：大负载应产出压缩帧")
	}
	sendRaw(t, ws, upFrame)
	select {
	case ps := <-reported:
		if ps.PodName != "demo" || ps.Phase != "Running" {
			t.Fatalf("上行 PodStatus 内容不一致: %+v", ps)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("等待 PodStatus 回调超时（压缩上行未被解压分发）")
	}
}

// TestCompressionOldEdgePlaintext 验证旧边缘（不声明能力）↔ 新云端：
// 注册应答不回带压缩字段，下发恒为明文——v1.0 兼容性。
func TestCompressionOldEdgePlaintext(t *testing.T) {
	srv, url := newTestServer(t)
	ws := dial(t, url)

	frame, _, p := registerWithCompression(t, ws, "edge-old", "")
	if protocol.IsCompressed(frame) {
		t.Fatalf("RegisterAck 应恒为明文，实际压缩帧")
	}
	if !p.Accepted {
		t.Fatalf("注册被拒绝: %s", p.Message)
	}
	if p.Compression != "" {
		t.Fatalf("旧边缘不应收到压缩确认，实际 %q", p.Compression)
	}

	// 下发大消息 → 明文（未协商，不得压缩）
	down, err := protocol.NewMessage(protocol.TypePodSync, "cloud", "edge-old", map[string]string{"data": bigPayload(t)})
	if err != nil {
		t.Fatalf("构造下发消息失败: %v", err)
	}
	if err := srv.SendToNode("edge-old", down); err != nil {
		t.Fatalf("SendToNode 失败: %v", err)
	}
	got := readRaw(t, ws)
	if protocol.IsCompressed(got) {
		t.Fatalf("未协商连接的下发消息必须明文，实际压缩帧")
	}
	if _, err := protocol.Decode(got); err != nil {
		t.Fatalf("明文下发消息解码失败: %v", err)
	}
}

// TestCompressionDisabled 验证云端开关关闭（WithCompress(false)）：
// 即使边缘声明 gzip，也不回带能力、不下发压缩帧（单开关控双向）。
// 上行压缩帧仍可被云端解压（接收端始终兼容两种格式，降级不破坏）。
func TestCompressionDisabled(t *testing.T) {
	srv, url := newTestServer(t, WithCompress(false))
	ws := dial(t, url)

	frame, _, p := registerWithCompression(t, ws, "edge-off", "gzip")
	if protocol.IsCompressed(frame) {
		t.Fatalf("RegisterAck 应恒为明文，实际压缩帧")
	}
	if p.Compression != "" {
		t.Fatalf("压缩关闭时 RegisterAck 不应回带能力，实际 %q", p.Compression)
	}

	// 下发大消息 → 明文
	down, err := protocol.NewMessage(protocol.TypePodSync, "cloud", "edge-off", map[string]string{"data": bigPayload(t)})
	if err != nil {
		t.Fatalf("构造下发消息失败: %v", err)
	}
	if err := srv.SendToNode("edge-off", down); err != nil {
		t.Fatalf("SendToNode 失败: %v", err)
	}
	if got := readRaw(t, ws); protocol.IsCompressed(got) {
		t.Fatalf("压缩关闭时下发必须明文，实际压缩帧")
	}

	// 上行压缩帧仍被接受（降级兼容：新边缘配置回落后不破坏通道）
	reported := make(chan struct{}, 1)
	srv.SetPodStatusHandler(func(_ string, _ PodStatusPayload) { reported <- struct{}{} })
	up, err := protocol.NewMessage(protocol.TypePodStatus, "edge-off", "cloud", PodStatusPayload{
		NodeID:  "edge-off",
		PodName: "demo",
		Phase:   "Running",
		Message: bigPayload(t),
	})
	if err != nil {
		t.Fatalf("构造上行消息失败: %v", err)
	}
	upFrame, err := protocol.EncodeCompressed(up)
	if err != nil {
		t.Fatalf("EncodeCompressed 失败: %v", err)
	}
	sendRaw(t, ws, upFrame)
	select {
	case <-reported:
	case <-time.After(2 * time.Second):
		t.Fatalf("压缩上行未被解压分发（降级路径失败）")
	}
}

// TestCompressionBroadcastPerConn 验证广播按连接协商状态分别编码：
// 新边缘（压缩）与旧边缘（明文）在同一次广播中各得其所。
func TestCompressionBroadcastPerConn(t *testing.T) {
	srv, url := newTestServer(t)
	wsNew := dial(t, url)
	wsOld := dial(t, url)

	if _, _, p := registerWithCompression(t, wsNew, "edge-b1", "gzip"); !p.Accepted {
		t.Fatalf("新边缘注册失败: %s", p.Message)
	}
	if _, _, p := registerWithCompression(t, wsOld, "edge-b2", ""); !p.Accepted {
		t.Fatalf("旧边缘注册失败: %s", p.Message)
	}

	msg, err := protocol.NewMessage(protocol.TypePodSync, "cloud", TargetBroadcast, map[string]string{"data": bigPayload(t)})
	if err != nil {
		t.Fatalf("构造广播消息失败: %v", err)
	}
	if n := srv.Broadcast(msg); n != 2 {
		t.Fatalf("广播送达数 = %d，期望 2", n)
	}
	frameNew := readRaw(t, wsNew)
	frameOld := readRaw(t, wsOld)
	if !protocol.IsCompressed(frameNew) {
		t.Fatalf("已协商连接应收到压缩广播帧，实际明文")
	}
	if protocol.IsCompressed(frameOld) {
		t.Fatalf("未协商连接应收到明文广播帧，实际压缩")
	}
	// 两帧内容一致（同一消息的两种编码）
	mNew, err := protocol.DecodeCompressed(frameNew)
	if err != nil {
		t.Fatalf("解压广播帧失败: %v", err)
	}
	mOld, err := protocol.Decode(frameOld)
	if err != nil {
		t.Fatalf("解码明文广播帧失败: %v", err)
	}
	if mNew.ID != mOld.ID || !bytes.Equal(mNew.Payload, mOld.Payload) {
		t.Fatalf("广播两帧内容不一致")
	}
}

// TestCompressionRegisterAckCorrelation 验证协商后的 RegisterAck 仍满足
// 既有契约（Source/Target/CorrelationID 正确）——压缩改动不破坏注册语义。
func TestCompressionRegisterAckCorrelation(t *testing.T) {
	srv, url := newTestServer(t)
	ws := dial(t, url)

	m, err := protocol.NewMessage(protocol.TypeRegister, "edge-c2", "cloud", RegisterPayload{
		NodeID: "edge-c2", Compression: "gzip",
	})
	if err != nil {
		t.Fatalf("构造 Register 失败: %v", err)
	}
	sendRaw(t, ws, mustEncode(t, m))
	ack, err := protocol.DecodeCompressed(readRaw(t, ws))
	if err != nil {
		t.Fatalf("解析 RegisterAck 失败: %v", err)
	}
	if ack.CorrelationID != m.ID {
		t.Fatalf("RegisterAck CorrelationID = %q，期望 %q（协商不影响注册关联）", ack.CorrelationID, m.ID)
	}
	if srv.NodeCount() != 1 {
		t.Fatalf("注册表节点数 = %d，期望 1", srv.NodeCount())
	}
}

// TestCompressionInvalidMessageAck 验证压缩帧解码失败（消息不合法）时，
// rejectInvalid 能从压缩帧中解压提取 source 并回发 invalid_message Ack
// （压缩不破坏既有错误反馈路径；Ack 按协商结果压缩下发）。
func TestCompressionInvalidMessageAck(t *testing.T) {
	_, url := newTestServer(t)
	ws := dial(t, url)

	if _, _, p := registerWithCompression(t, ws, "edge-bad", "gzip"); !p.Accepted {
		t.Fatalf("注册失败: %s", p.Message)
	}

	// 压缩帧内的信封缺少 version 字段（校验失败，但 source 可提取）
	bad := &protocol.Message{ID: "bad-1", Type: protocol.TypeHeartbeat, Source: "edge-bad", Target: "cloud"}
	if len(mustEncode(t, bad)) < protocol.MinCompressBytes {
		// 撑大到可压缩，确保产出压缩帧（测试压缩路径的错误处理）
		bad.Payload = []byte(`"` + strings.Repeat("padding for compressibility ", 40) + `"`)
	}
	frame, err := protocol.EncodeCompressed(bad)
	if err != nil {
		t.Fatalf("EncodeCompressed 失败: %v", err)
	}
	if !protocol.IsCompressed(frame) {
		t.Fatalf("测试前提：应产出压缩帧")
	}
	sendRaw(t, ws, frame)

	ackFrame := readRaw(t, ws)
	ack, err := protocol.DecodeCompressed(ackFrame)
	if err != nil {
		t.Fatalf("解析错误反馈 Ack 失败: %v", err)
	}
	if ack.Type != protocol.TypeAck {
		t.Fatalf("期望 Ack，收到 %s", ack.Type)
	}
	if ack.Target != "edge-bad" {
		t.Fatalf("Ack Target = %q，期望 edge-bad（从压缩帧提取 source）", ack.Target)
	}
	var p AckPayload
	if err := ack.DecodePayload(&p); err != nil {
		t.Fatalf("解析 Ack payload 失败: %v", err)
	}
	if p.Code != CodeInvalidMessage {
		t.Fatalf("Ack code = %q，期望 %q", p.Code, CodeInvalidMessage)
	}
}
