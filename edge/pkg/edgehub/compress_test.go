package edgehub

import (
	"testing"
	"time"

	"edgeflow/pkg/protocol"
)

// drainRawFrames 清空 mock 已收到的原始帧（测试内做帧格式断言）。
func drainRawFrames(m *mockCloud) [][]byte {
	var frames [][]byte
	for {
		select {
		case f := <-m.rawFrames:
			frames = append(frames, f)
		default:
			return frames
		}
	}
}

// largePayload 构造大负载（> MinCompressBytes 且可压缩），触发压缩路径。
func largePayload() string {
	const chunk = `{"podName":"demo","namespace":"default","phase":"Running","replicas":3,"image":"nginx:1.27","data":"reconcile payload for compression test"},`
	buf := make([]byte, 0, protocol.MinCompressBytes*4)
	for len(buf) < protocol.MinCompressBytes*4 {
		buf = append(buf, chunk...)
	}
	return string(buf)
}

// TestCompressionNegotiatedUplink 验证压缩协商闭环（新边缘 ↔ 新云端）：
//   - Register 恒为明文帧（协商信道）；
//   - RegisterAck 回带 compression=gzip 后，上行大消息为压缩帧且可被解压；
//   - 上行小消息（心跳类）跳过压缩仍为明文。
func TestCompressionNegotiatedUplink(t *testing.T) {
	mock := startMockCloud(t, mockConfig{compressAck: true})
	client := New(Options{
		CloudAddr:         mock.url,
		NodeID:            "compress-node",
		HeartbeatInterval: 200 * time.Millisecond,
		BackoffBase:       50 * time.Millisecond,
	})
	client.Start()
	defer client.Stop()
	reg := mock.waitRegister(t)
	waitConnected(t, client)

	// Register 帧必须声明 gzip 能力（协商起点，WBS 4.4）：
	// 云端据此决定是否回带 compression=gzip；缺失则协商链断裂、压缩永不启用。
	var regPayload RegisterPayload
	if err := reg.DecodePayload(&regPayload); err != nil {
		t.Fatalf("解析 Register 负载失败: %v", err)
	}
	if regPayload.Compression != "gzip" {
		t.Fatalf("Register 应声明 compression=gzip，实际 %q", regPayload.Compression)
	}

	// 大消息上行：应为压缩帧
	big, err := protocol.NewMessage(protocol.TypePodStatus, "compress-node", targetCloud,
		map[string]any{"data": largePayload()})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	if err := client.Send(big); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	got := mock.waitType(t, protocol.TypePodStatus)
	if got.ID != big.ID {
		t.Fatalf("上行消息 ID 不一致: got %q, want %q", got.ID, big.ID)
	}

	// 小消息上行（< MinCompressBytes）：跳过压缩，明文
	small, err := protocol.NewMessage(protocol.TypeHeartbeat, "compress-node", targetCloud,
		HeartbeatPayload{Timestamp: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	if err := client.Send(small); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	mock.waitType(t, protocol.TypeHeartbeat)

	// 帧格式断言：收到的帧按序为 [Register(明文), 大消息(压缩), 小消息(明文)]
	frames := drainRawFrames(mock)
	if len(frames) < 3 {
		t.Fatalf("原始帧数 = %d，期望 ≥3", len(frames))
	}
	if protocol.IsCompressed(frames[0]) {
		t.Fatalf("Register 应恒为明文帧（协商信道），实际压缩帧")
	}
	if !protocol.IsCompressed(frames[1]) {
		t.Fatalf("协商后的大消息应为压缩帧，实际明文")
	}
	if protocol.IsCompressed(frames[2]) {
		t.Fatalf("小消息应跳过压缩（明文），实际压缩帧")
	}
}

// TestCompressionNotAdvertisedUplinkPlaintext 验证旧云端（RegisterAck 不
// 回带 compression）↔ 新边缘：上行恒明文——v1.0 兼容性。
func TestCompressionNotAdvertisedUplinkPlaintext(t *testing.T) {
	mock := startMockCloud(t, mockConfig{}) // 无 compressAck → 旧云端行为
	client := New(Options{
		CloudAddr:         mock.url,
		NodeID:            "plain-node",
		HeartbeatInterval: 200 * time.Millisecond,
		BackoffBase:       50 * time.Millisecond,
	})
	client.Start()
	defer client.Stop()
	mock.waitRegister(t)
	waitConnected(t, client)

	msg, err := protocol.NewMessage(protocol.TypePodStatus, "plain-node", targetCloud,
		map[string]any{"data": largePayload()})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	if err := client.Send(msg); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if got := mock.waitType(t, protocol.TypePodStatus); got.ID != msg.ID {
		t.Fatalf("上行消息 ID 不一致: got %q, want %q", got.ID, msg.ID)
	}

	// 帧格式断言：Register 与上行大消息均为明文（未协商不得压缩）
	frames := drainRawFrames(mock)
	if len(frames) < 2 {
		t.Fatalf("原始帧数 = %d，期望 ≥2", len(frames))
	}
	for i, f := range frames {
		if protocol.IsCompressed(f) {
			t.Fatalf("未协商时第 %d 帧不应压缩", i)
		}
	}
}

// TestCompressionRenegotiateAfterReconnect 验证压缩协商按连接重新进行：
// 重连后 RegisterAck 再次回带 gzip → 上行仍压缩（协商状态不跨连接残留）。
func TestCompressionRenegotiateAfterReconnect(t *testing.T) {
	mock := startMockCloud(t, mockConfig{compressAck: true, closeAfterRegisterCount: 1})
	client := New(Options{
		CloudAddr:         mock.url,
		NodeID:            "recompress-node",
		HeartbeatInterval: 200 * time.Millisecond,
		BackoffBase:       50 * time.Millisecond,
		BackoffMax:        200 * time.Millisecond,
	})
	client.Start()
	defer client.Stop()

	mock.waitRegister(t) // 第一连接注册
	waitConnected(t, client)
	mock.waitRegister(t) // 重连后重新注册
	waitConnected(t, client)

	msg, err := protocol.NewMessage(protocol.TypePodStatus, "recompress-node", targetCloud,
		map[string]any{"data": largePayload()})
	if err != nil {
		t.Fatalf("构造消息失败: %v", err)
	}
	if err := client.Send(msg); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}
	if got := mock.waitType(t, protocol.TypePodStatus); got.ID != msg.ID {
		t.Fatalf("上行消息 ID 不一致: got %q, want %q", got.ID, msg.ID)
	}

	// 重连后的帧序列：[Register(明文), Register(明文), 大消息(压缩)]
	frames := drainRawFrames(mock)
	if len(frames) < 3 {
		t.Fatalf("原始帧数 = %d，期望 ≥3", len(frames))
	}
	for i := 0; i < 2; i++ {
		if protocol.IsCompressed(frames[i]) {
			t.Fatalf("第 %d 个 Register 应恒为明文帧", i+1)
		}
	}
	if !protocol.IsCompressed(frames[2]) {
		t.Fatalf("重连协商后的大消息应为压缩帧，实际明文")
	}
}
