// Package flvremux 实现 FLV 透传封装器（v0.44.0，spec 0017 US-1）：把
// H.264 AnnexB 裸流（含带外 SPS/PPS）封装为 FLV 字节流——纯封装无转码
// （AnnexB→AVCC + AVCDecoderConfigurationRecord + onMetaData）。
//
// 格式要点（Adobe FLV Format Spec 10.1 最小面）：
//   - Header："FLV" + version(1)=1 + flags(1，video=0x01) + HeaderSize(4)=9；
//   - 每个 Tag 前 4 字节 PreviousTagSize（首条为 0）；
//   - Tag：type(1)（9=video, 18=script）+ DataSize(3) + Timestamp(3, 毫秒低位)
//   - TimestampExtended(1, 毫秒高 8 位) + StreamID(3, 0) + Data；
//   - Video Tag Body：FrameType(4bit)|CodecID(4bit=7 AVC) + AVCPacketType(8bit:
//     0=sequence header, 1=NALU) + CompositionTime(3, 本版 0) + 载荷；
//   - AVC sequence header = AVCDecoderConfigurationRecord（SPS/PPS 打包，
//     lengthSizeMinusOne=3 → AVCC 4 字节长度前缀）；
//   - Script Tag（onMetaData）：AMF0 "@setDataFrame" + "onMetaData" + ECMA 数组
//     最小字段（width/height/videocodecid/encoder）。
//
// 零依赖（仅标准库）。单 goroutine 独占使用（Remuxer 有内部状态——序列头
// 只发一次）。
package flvremux

import (
	"encoding/binary"
	"errors"
	"math"
)

// FLV 头部常量。
var flvHeader = []byte{'F', 'L', 'V', 0x01, 0x01, 0x00, 0x00, 0x00, 0x09} // video-only

// ErrNoParamSet 无 SPS/PPS（序列头无法构造）。
var ErrNoParamSet = errors.New("flvremux: 缺 SPS/PPS（序列头无法构造）")

// Remuxer 把 AnnexB H.264 帧序列封装为 FLV。
// 用法：New() → SequenceHeader(sps, pps) → MetaData(...) → 逐帧 WriteAnnexB；
// Full 取全部、Delta 取自上次取用以来的增量（增量拉流）。单 goroutine 独占。
type Remuxer struct {
	buf       []byte // 已封装字节
	consumed  int    // Delta 消费游标
	seqHeader bool   // 序列头是否已发
}

// New 创建封装器并写入 FLV header + PreviousTagSize0。
func New() *Remuxer {
	r := &Remuxer{buf: make([]byte, 0, 4096)}
	r.buf = append(r.buf, flvHeader...)
	r.buf = append(r.buf, 0x00, 0x00, 0x00, 0x00) // PreviousTagSize0
	return r
}

// Full 返回当前全部封装字节。
func (r *Remuxer) Full() []byte { return r.buf }

// Delta 返回自上次取用以来的增量字节（增量拉流；与 Full 择一使用）。
func (r *Remuxer) Delta() []byte {
	out := r.buf[r.consumed:]
	r.consumed = len(r.buf)
	return out
}

// Sync 把增量游标推进到当前末尾（Full() 取走全部后调用，使后续 Delta 只含
// 新增字节——否则 Delta 会重放 Full 覆盖范围）。
func (r *Remuxer) Sync() { r.consumed = len(r.buf) }

// writeTag 追加一个 FLV tag（type/timestampMs/data）+ PreviousTagSize。
func (r *Remuxer) writeTag(tagType byte, timestampMs uint32, data []byte) {
	n := len(data)
	head := []byte{
		tagType,
		byte(n >> 16), byte(n >> 8), byte(n),
		byte(timestampMs >> 16), byte(timestampMs >> 8), byte(timestampMs), // 低 24 位
		byte(timestampMs >> 24), // 扩展高 8 位
		0x00, 0x00, 0x00,        // StreamID=0
	}
	r.buf = append(r.buf, head...)
	r.buf = append(r.buf, data...)
	prev := make([]byte, 4)
	binary.BigEndian.PutUint32(prev, uint32(n+11))
	r.buf = append(r.buf, prev...)
}

// videoTagBody 构造 video tag data（AVC）。
func videoTagBody(packetType byte, compositionMs uint32, payload []byte) []byte {
	out := make([]byte, 0, len(payload)+5)
	out = append(out, 0x17) // FrameType=1(key)|CodecID=7(AVC)——本版关键帧口径统一 0x17
	out = append(out, packetType)
	// CompositionTime：3 字节有符号（本版 0）。
	out = append(out, byte(compositionMs>>16), byte(compositionMs>>8), byte(compositionMs))
	out = append(out, payload...)
	return out
}

// SequenceHeader 构造并发送 AVC sequence header（SPS/PPS → 配置记录）。
func (r *Remuxer) SequenceHeader(sps, pps []byte) error {
	if len(sps) == 0 || len(pps) == 0 {
		return ErrNoParamSet
	}
	rec := avcDecoderConfigurationRecord(sps, pps)
	r.writeTag(9, 0, videoTagBody(0, 0, rec))
	r.seqHeader = true
	return nil
}

// MetaData 构造并发送 onMetaData script tag（AMF0 最小字段）。
func (r *Remuxer) MetaData(width, height int) {
	var data []byte
	data = appendAMFString(data, "@setDataFrame")
	data = appendAMFString(data, "onMetaData")
	// ECMA 数组：count(4) + [key(2+len) + value]×N + 结束 00 00 09。
	items := []struct {
		key string
		val []byte
	}{
		{"width", amfNumber(float64(width))},
		{"height", amfNumber(float64(height))},
		{"videocodecid", amfNumber(7)},
		{"encoder", amfString("EdgeFlow/0.44")},
	}
	body := make([]byte, 0, 64)
	var cnt [4]byte
	binary.BigEndian.PutUint32(cnt[:], uint32(len(items)))
	body = append(body, cnt[:]...)
	for _, it := range items {
		body = appendAMFString(body, it.key)
		body = append(body, it.val...)
	}
	body = append(body, 0x00, 0x00, 0x09) // array end
	data = append(data, body...)
	r.writeTag(18, 0, data)
}

// WriteAnnexB 把一帧 AnnexB H.264（可含多 NALU）封装为 AVC NALU tag。
// 前置：SequenceHeader 已调用（否则错误）。
func (r *Remuxer) WriteAnnexB(annexb []byte, timestampMs uint32) error {
	if !r.seqHeader {
		return ErrNoParamSet
	}
	avcc, err := annexBToAVCC(annexb)
	if err != nil {
		return err
	}
	keyframe := containsIDR(annexb)
	body := videoTagBody(1, 0, avcc)
	if keyframe {
		body[0] = 0x17 // 关键帧
	} else {
		body[0] = 0x27 // 帧间
	}
	r.writeTag(9, timestampMs, body)
	return nil
}

// avcDecoderConfigurationRecord 打包 SPS/PPS（单 SPS/单 PPS 最小面）。
func avcDecoderConfigurationRecord(sps, pps []byte) []byte {
	out := make([]byte, 0, len(sps)+len(pps)+16)
	out = append(out, 0x01)   // configurationVersion
	out = append(out, sps[1]) // AVCProfileIndication
	out = append(out, sps[2]) // profile_compatibility
	out = append(out, sps[3]) // AVCLevelIndication
	out = append(out, 0xFF)   // reserved(6)=111111 + lengthSizeMinusOne(2)=11
	out = append(out, 0xE1)   // reserved(3)=111 + numOfSPS(5)=1
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(sps)))
	out = append(out, l[:]...)
	out = append(out, sps...)
	out = append(out, 0x01) // numOfPPS
	binary.BigEndian.PutUint16(l[:], uint16(len(pps)))
	out = append(out, l[:]...)
	out = append(out, pps...)
	return out
}

// annexBToAVCC 把 AnnexB（00 00 01 / 00 00 00 01 起始码分隔）转为 AVCC
// （每 NALU 前 4 字节大端长度）。
func annexBToAVCC(annexb []byte) ([]byte, error) {
	nals := splitAnnexBNals(annexb)
	if len(nals) == 0 {
		return nil, errors.New("flvremux: AnnexB 无 NALU")
	}
	out := make([]byte, 0, len(annexb)+len(nals)*4)
	var l [4]byte
	for _, nal := range nals {
		binary.BigEndian.PutUint32(l[:], uint32(len(nal)))
		out = append(out, l[:]...)
		out = append(out, nal...)
	}
	return out, nil
}

// splitAnnexBNals 按 3/4 字节起始码切分 NALU（容忍 NALU 内 0x00 填充——
// 起始码判定严格匹配 00 00 01 序列）。
func splitAnnexBNals(b []byte) [][]byte {
	var nals [][]byte
	i := 0
	start := -1
	for i+2 < len(b) {
		if b[i] == 0x00 && b[i+1] == 0x00 && b[i+2] == 0x01 {
			// 找到 3 字节起始码：前面若还有 0x00 则属 4 字节起始码的一部分。
			scStart := i
			if scStart > 0 && b[scStart-1] == 0x00 {
				scStart--
			}
			if start >= 0 && scStart > start {
				nals = append(nals, b[start:scStart])
			}
			start = i + 3
			i += 3
			continue
		}
		i++
	}
	if start >= 0 && start < len(b) {
		nals = append(nals, b[start:])
	}
	return nals
}

// containsIDR 判定 AnnexB 是否含 IDR NALU（type 5）。
func containsIDR(annexb []byte) bool {
	for _, nal := range splitAnnexBNals(annexb) {
		if len(nal) > 0 && nal[0]&0x1F == 5 {
			return true
		}
	}
	return false
}

// ---- AMF0 最小编码 ----

// appendAMFString 追加 AMF0 字符串（0x02 + len(2) + data）。
func appendAMFString(dst []byte, s string) []byte {
	dst = append(dst, 0x02)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(s)))
	dst = append(dst, l[:]...)
	return append(dst, s...)
}

// amfNumber 编码 AMF0 number（0x00 + float64 大端）。
func amfNumber(v float64) []byte {
	out := make([]byte, 9)
	out[0] = 0x00
	binary.BigEndian.PutUint64(out[1:], float64bits(v))
	return out
}

// amfString 编码 AMF0 字符串值。
func amfString(s string) []byte { return appendAMFString(nil, s) }

// float64bits 是 math.Float64bits 的局部别名（可读性）。
func float64bits(v float64) uint64 { return math.Float64bits(v) }
