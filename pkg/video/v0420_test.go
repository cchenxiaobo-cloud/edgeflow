// v0.42.0（specs/0015）新用例：C6 jpegScanner 元数据段修复验收 + RTSP 源
// 瞬时失败重试语义。冻结带 v0340 测试文件保持零改动（DEV-SPEC：行为演进=
// 新测试文件，v0.42 复核 P1-4）。
package video

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestV0420JPEGScannerMetadataFFD9 覆盖 C6 修复（v0.42.0）：元数据段
// （EXIF 缩略图 / COM）内嵌 FFD9 不再误判为帧尾——完整切帧。
func TestV0420JPEGScannerMetadataFFD9(t *testing.T) {
	a := jpegFrame(t, 20, 16)
	// 构造 APP1 段：marker(FF E1) + len(2) + 段体（内嵌 FFD9 假帧尾）。
	exifBody := []byte{0x45, 0x78, 0x69, 0x66, 0x00, 0x00, 0xFF, 0xD9, 0x01, 0x02} // 含 FFD9
	app1 := []byte{0xFF, 0xE1, byte((len(exifBody) + 2) >> 8), byte((len(exifBody) + 2) & 0xFF)}
	app1 = append(app1, exifBody...)
	// 构造 COM 段（内嵌 FFD9）。
	comBody := []byte{0xFF, 0xD9, 'c', 'o', 'm'}
	com := []byte{0xFF, 0xFE, byte((len(comBody) + 2) >> 8), byte((len(comBody) + 2) & 0xFF)}
	com = append(com, comBody...)
	// 拼装：APP1 + COM + 正常帧（SOI 之后、SOS 之前插入）。
	soi := []byte{0xFF, 0xD8}
	frame := append(append([]byte{}, soi...), app1...)
	frame = append(frame, com...)
	frame = append(frame, a[2:]...) // 去掉原 SOI，保留其余
	// 逐字节喂入（跨块边界防御）。
	var sc jpegScanner
	var got [][]byte
	for _, by := range frame {
		got = append(got, sc.feed([]byte{by})...)
	}
	if len(got) != 1 {
		t.Fatalf("含元数据 FFD9 的帧应完整切出 1 帧: %d", len(got))
	}
	if !bytes.Equal(got[0], frame) {
		t.Fatalf("切帧字节不一致: %d vs %d", len(got[0]), len(frame))
	}
	// 双帧流（元数据段夹在两帧之间）——一次喂入应切出完整 2 帧。
	b := jpegFrame(t, 28, 22)
	stream := append(append([]byte{}, frame...), b...)
	sc2 := jpegScanner{}
	got2 := sc2.feed(stream)
	if len(got2) != 2 || !bytes.Equal(got2[0], frame) || !bytes.Equal(got2[1], b) {
		t.Fatalf("双帧流应完整切出 2 帧: %d", len(got2))
	}
}

// TestV0420RTSPSourceRetryLoop 覆盖断流自愈缺口②修复（v0.42 复核 P1-2）：
// 瞬时不可用（拨号失败）不退化为「单次失败即终止」——Next 循环消费重试
// 哨兵、持续重试，直到 ctx 取消返回 ctx.Err（而非终止性源错误）。
func TestV0420RTSPSourceRetryLoop(t *testing.T) {
	// 127.0.0.1:1 无监听——拨号被立即拒绝（瞬时不可用形态）。
	src := NewRTSPSource(RTSPConfig{URL: "rtsp://127.0.0.1:1/live", Decoder: "/bin/cat", ReconnectMs: 10})
	defer func() { _ = src.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, err := src.Next(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx 截止应返回 DeadlineExceeded（重试不终止）: %v", err)
	}
	if n := src.Reconnects(); n < 3 {
		t.Fatalf("瞬时失败应多次重试: reconnects=%d", n)
	}
}

// TestV0420RTSPSourceURLPrecheck 覆盖构造期 URL 预检（v0.42 复核 P2-1）：
// 非 rtsp scheme 在构造期显式拒绝（不进入运行期重连循环）。
func TestV0420RTSPSourceURLPrecheck(t *testing.T) {
	if _, err := NewSource(SourceConfig{Type: "rtsp", URL: "http://cam/live", Decoder: "/bin/cat"}); err == nil {
		t.Fatal("非 rtsp scheme 应拒绝")
	}
	if _, err := NewSource(SourceConfig{Type: "rtsp", URL: "rtsp://", Decoder: "/bin/cat"}); err == nil {
		t.Fatal("缺 host 应拒绝")
	}
	if _, err := NewSource(SourceConfig{Type: "rtsp", URL: "rtsp://cam/live", Decoder: "/bin/cat"}); err != nil {
		t.Fatalf("合法 rtsp URL 应通过: %v", err)
	}
}
