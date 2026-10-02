// v0420_test.go：RTSP 客户端与 H.264 重组单测（spec 0015 US-1/US-2）。
package rtspclient

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSignalFlowWithSim 覆盖信令全链（模拟服务端）：OPTIONS 协商 →
// DESCRIBE（SDP 控制路径解析）→ SETUP（interleaved channel）→ PLAY →
// Teardown；断言会话/能力/轨地址。
func TestSignalFlowWithSim(t *testing.T) {
	addr := startSimServer(t, SimConfig{})
	c, err := Dial(Config{URL: "rtsp://" + addr + "/live", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Options(); err != nil {
		t.Fatalf("OPTIONS 失败: %v", err)
	}
	srv, pub := c.Capabilities()
	if srv == "" || pub == "" {
		t.Fatalf("能力协商未记录: %q / %q", srv, pub)
	}
	if err := c.Describe(); err != nil {
		t.Fatalf("DESCRIBE 失败: %v", err)
	}
	if c.control != "rtsp://"+addr+"/live/trackID=0" {
		t.Fatalf("控制路径解析不符: %q", c.control)
	}
	if err := c.Setup(); err != nil {
		t.Fatalf("SETUP 失败: %v", err)
	}
	if c.Session() == "" {
		t.Fatalf("会话未建立")
	}
	if err := c.Play(); err != nil {
		t.Fatalf("PLAY 失败: %v", err)
	}
	if err := c.Teardown(); err != nil {
		t.Fatalf("TEARDOWN 失败: %v", err)
	}
}

// TestBasicAuthChallenge 覆盖 401 挑战 → Basic 认证重试（凭证正确）与
// 凭证缺失报错。
func TestBasicAuthChallenge(t *testing.T) {
	// 凭证正确：URL 内嵌，全链通过。
	addr := startSimServer(t, SimConfig{RequireAuth: true, User: "cam", Pass: "secret"})
	c, err := Dial(Config{URL: "rtsp://cam:secret@" + addr + "/live", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Options(); err != nil {
		t.Fatalf("OPTIONS 失败: %v", err)
	}
	if err := c.Describe(); err != nil {
		t.Fatalf("带凭证 DESCRIBE 应通过 401 挑战: %v", err)
	}

	// 凭证缺失：DESCRIBE 返回 ErrUnauthorized。
	c2, err := Dial(Config{URL: "rtsp://" + addr + "/live", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = c2.Close() }()
	_ = c2.Options()
	if err := c2.Describe(); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("缺凭证应 ErrUnauthorized: %v", err)
	}
}

// TestReadAnnexReassembly 覆盖 interleaved 媒体流 → AnnexB 重组：
// STAP-A（SPS/PPS）与 FU-A 分片 IDR 全部还原为 [起始码][NAL] 序列。
func TestReadAnnexReassembly(t *testing.T) {
	addr := startSimServer(t, SimConfig{StreamGap: 10 * time.Millisecond})
	c, err := Dial(Config{URL: "rtsp://" + addr + "/live", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Play(); err != nil {
		t.Fatalf("PLAY 失败: %v", err)
	}
	// PLAY 应答读取过程中已含部分媒体帧；再读至积累 ≥2 个完整 IDR NAL。
	deadline := time.Now().Add(5 * time.Second)
	var gotSPS, gotPPS, gotIDR int
	for time.Now().Before(deadline) {
		out, rerr := c.ReadAnnex()
		if rerr != nil {
			t.Fatalf("ReadAnnex 出错: %v", rerr)
		}
		nals := splitAnnex(out)
		for _, nal := range nals {
			switch nal[0] & 0x1F {
			case 7:
				gotSPS++
			case 8:
				gotPPS++
			case 5:
				gotIDR++
			}
		}
		if gotIDR >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gotSPS == 0 || gotPPS == 0 || gotIDR < 2 {
		t.Fatalf("AnnexB 重组不足: sps=%d pps=%d idr=%d", gotSPS, gotPPS, gotIDR)
	}
	if c.DroppedRTP() != 0 {
		t.Fatalf("正常流不应有 RTP 丢弃: %d", c.DroppedRTP())
	}
}

// splitAnnex 按 AnnexB 起始码切 NAL（测试用）。
func splitAnnex(b []byte) [][]byte {
	var out [][]byte
	start := []byte{0, 0, 0, 1}
	for {
		i := bytes.Index(b, start)
		if i < 0 {
			break
		}
		rest := b[i+4:]
		j := bytes.Index(rest, start)
		if j < 0 {
			if len(rest) > 0 {
				out = append(out, rest)
			}
			return out
		}
		out = append(out, rest[:j])
		b = rest[j:]
	}
	return out
}

// TestDemuxH264 单元覆盖三种载荷形态与错误路径。
func TestDemuxH264(t *testing.T) {
	// 单 NAL（type 7 SPS）。
	nals, err := demuxH264([]byte{0x67, 0x01, 0x02}, &nothing)
	if err != nil || len(nals) != 1 || nals[0][0]&0x1F != 7 {
		t.Fatalf("单 NAL 直通失败: %v %v", nals, err)
	}
	// STAP-A 两个 NAL。
	stap := []byte{0x78, 0x00, 0x02, 0x67, 0x01, 0x00, 0x02, 0x68, 0x02}
	nals, err = demuxH264(stap, &nothing)
	if err != nil || len(nals) != 2 {
		t.Fatalf("STAP-A 分解失败: %v %v", nals, err)
	}
	// FU-A 三片重组：NAL type 5。
	idr := []byte{0x65, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE}
	frags := fuaPackets(idr, 2)
	if len(frags) != 3 {
		t.Fatalf("分片数 = %d, want 3", len(frags))
	}
	var assembled [][]byte
	for _, f := range frags {
		ns, err := demuxH264(f, &nothing)
		if err != nil {
			t.Fatalf("FU-A 片解析失败: %v", err)
		}
		assembled = append(assembled, ns...)
	}
	if len(assembled) != 1 || !bytes.Equal(assembled[0], idr) {
		t.Fatalf("FU-A 重组不符: % X vs % X", assembled, idr)
	}
	// 缺 S 起始分片 → 错误。
	orphan := []byte{0x7C, 0x05, 0x01, 0x02}
	if _, err := demuxH264(orphan, &nothing); err == nil {
		t.Fatalf("缺 S 分片应报错")
	}
	// 空/畸形载荷 → 错误。
	if _, err := demuxH264(nil, &nothing); err == nil {
		t.Fatalf("空载荷应报错")
	}
	// 保留 type 0 → 错误。
	if _, err := demuxH264([]byte{0x00, 0x01}, &nothing); err == nil {
		t.Fatalf("保留 type 应报错")
	}
}

// nothing 是 demuxH264 的空分片状态槽。
var nothing *fuAssembler

// TestParseSDPControl 覆盖 SDP 控制路径三种形态。
func TestParseSDPControl(t *testing.T) {
	sdp := "v=0\r\nm=video 0 RTP/AVP 96\r\na=control:trackID=0\r\n"
	base := "rtsp://h/live"
	if got := parseSDPControl(sdp, base); got != base+"/trackID=0" {
		t.Fatalf("相对控制路径拼接不符: %q", got)
	}
	if got := parseSDPControl(sdp, "rtsp://h:554/live"); got != "rtsp://h:554/live/trackID=0" {
		t.Fatalf("带端口拼接不符: %q", got)
	}
	abs := "v=0\r\na=control:rtsp://h/abs\r\n"
	if got := parseSDPControl(abs, base); got != "rtsp://h/abs" {
		t.Fatalf("绝对控制路径应直返: %q", got)
	}
}

// TestParseRTSPURL 覆盖 URL 形态（凭证/缺端口/缺路径）。
func TestParseRTSPURL(t *testing.T) {
	u, p, hp, path := parseRTSPURL("rtsp://cam:s@host:8554/live")
	if u != "cam" || p != "s" || hp != "host:8554" || path != "/live" {
		t.Fatalf("解析不符: %q %q %q %q", u, p, hp, path)
	}
	u, p, hp, path = parseRTSPURL("rtsp://host")
	if hp != "host" || path != "/" {
		t.Fatalf("缺端口/路径默认不符: %q %q", hp, path)
	}
}

// TestParseInterleavedChannel 覆盖 Transport 头解析。
func TestParseInterleavedChannel(t *testing.T) {
	if got := parseInterleavedChannel("RTP/AVP/TCP;interleaved=0-1"); got != 0 {
		t.Fatalf("channel = %d, want 0", got)
	}
	if got := parseInterleavedChannel("RTP/AVP/TCP;interleaved=2-3"); got != 2 {
		t.Fatalf("channel = %d, want 2", got)
	}
	if got := parseInterleavedChannel("RTP/AVP/UDP"); got != 0 {
		t.Fatalf("无 interleaved 应默认 0: %d", got)
	}
}

// TestRTPHeaderParse 覆盖 RTP 头解析与扩展头/CSRC 跳过（经 dispatch 路径）。
func TestRTPHeaderParse(t *testing.T) {
	c := &Client{}
	// 带 X 扩展头 + 2 个 CSRC 的包（载荷 STAP-A 一个 NAL）。
	// 首字节 0x92 = V(2)|P=0|X=1|CC=2；CSRC 列表在 SSRC 之后、扩展头之前
	// （RFC 3550——v0.42 复核 P2-2：此前后字节序构造使 CSRC/扩展未被真实覆盖）。
	pkt := make([]byte, 0, 64)
	pkt = append(pkt, 0x92, 96)               // V=2 X=1 CC=2, PT=96
	pkt = append(pkt, 0x00, 0x07)             // seq=7
	pkt = append(pkt, 0x00, 0x00, 0x00, 0x01) // ts
	pkt = append(pkt, 0xDE, 0xAD, 0xBE, 0xEF) // ssrc
	pkt = append(pkt, 0x11, 0x22, 0x33, 0x44) // CSRC×2
	pkt = append(pkt, 0x55, 0x66, 0x77, 0x88)
	pkt = append(pkt, 0xBD, 0x00, 0x00, 0x01) // 扩展头（profile + 1 word）
	pkt = append(pkt, 0x00, 0x00, 0x00, 0x00) // 扩展体 1 word
	stap := stapA([]byte{0x67, 0x01})
	pkt = append(pkt, stap...)
	c.dispatchRTP(pkt)
	out, rerr := c.ReadAnnex()
	if rerr != nil {
		t.Fatalf("ReadAnnex 出错: %v", rerr)
	}
	// 输出内容断言：应为 [00 00 00 01][0x67 0x01]（跳过 CSRC/扩展后的净载荷）。
	want := append(append([]byte{}, annexbStart...), 0x67, 0x01)
	if !bytes.Equal(out, want) {
		t.Fatalf("输出 NAL 不符（CSRC/扩展跳过口径）: % X", out)
	}
	// SEQ 跳跃（乱序观测）→ 丢弃计数增长。
	droppedBefore := c.DroppedRTP()
	c.dispatchRTP(buildRTP(96, 99, 0, stapA([]byte{0x67, 0x01})))
	if c.DroppedRTP() != droppedBefore+1 {
		t.Fatalf("SEQ 跳跃应计数: %d → %d", droppedBefore, c.DroppedRTP())
	}
	// 非 RTP 版本包 → 丢弃计数。
	c.dispatchRTP([]byte{0x00, 0x01, 0x02, 0x03})
	if c.DroppedRTP() <= droppedBefore+1 {
		t.Fatalf("畸形 RTP 应计数丢弃")
	}
	_ = binary.BigEndian
}

// TestReadAnnexConnError 覆盖断流自愈缺口①修复（v0.42 复核 P1-1）：服务端
// 强制断开（ResetAfterDur）后 ReadAnnex 返回连接错误——不再把 EOF 当读
// 超时静默吞掉（旧行为下调用方永远无法感知断流）。
func TestReadAnnexConnError(t *testing.T) {
	addr := startSimServer(t, SimConfig{StreamGap: 10 * time.Millisecond, ResetAfterDur: 400 * time.Millisecond})
	c, err := Dial(Config{URL: "rtsp://" + addr + "/live", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Play(); err != nil {
		t.Fatalf("PLAY 失败: %v", err)
	}
	deadline := time.Now().Add(6 * time.Second)
	var lastErr error
	var gotFrames bool
	for time.Now().Before(deadline) {
		out, rerr := c.ReadAnnex()
		if len(out) > 0 {
			gotFrames = true
		}
		if rerr != nil {
			lastErr = rerr
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gotFrames {
		t.Fatal("断开前应有帧输出（前置条件）")
	}
	if lastErr == nil {
		t.Fatal("服务端断开后 ReadAnnex 应返回连接错误")
	}
	if !strings.Contains(lastErr.Error(), "媒体连接断开") {
		t.Fatalf("错误应标识媒体连接断开: %v", lastErr)
	}
}
