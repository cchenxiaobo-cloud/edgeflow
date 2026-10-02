// v0.44.0（spec 0017 US-1）FLV 封装器字节级单测：header/tag 结构、AnnexB→AVCC
// 两形态起始码、sequence header 结构、onMetaData、畸形防御。
package flvremux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// sps/pps 与 pkg/rtspclient 模拟器同形态（测试确定性）。
var (
	testSPS = []byte{0x67, 0x42, 0xC0, 0x1E, 0xAB, 0x40, 0xA0, 0xFD, 0xFF, 0xE8}
	testPPS = []byte{0x68, 0xCB, 0x83, 0xCB, 0x20}
)

// frame 构造一帧 AnnexB（IDR 或非 IDR，双 NALU 形态）。
func frame(idr bool) []byte {
	nalType := byte(1) // 非 IDR
	if idr {
		nalType = 5
	}
	n1 := append([]byte{0x65 | (nalType & 0), nalType}, 0x01, 0x02, 0x03) // 占位重构见下
	_ = n1
	nal1 := []byte{nalType, 0x01, 0x02, 0x03}
	nal2 := []byte{0x01, 0x09, 0x0A}
	out := []byte{0x00, 0x00, 0x00, 0x01}
	out = append(out, nal1...)
	out = append(out, 0x00, 0x00, 0x01) // 3 字节起始码（第二形态）
	out = append(out, nal2...)
	return out
}

func TestFLVHeaderAndSeqHeader(t *testing.T) {
	r := New()
	if !bytes.HasPrefix(r.Full(), []byte("FLV\x01\x01")) {
		t.Fatalf("FLV header 异常: % X", r.Full()[:8])
	}
	if binary.BigEndian.Uint32(r.Full()[5:9]) != 9 {
		t.Fatalf("HeaderSize 应为 9")
	}
	// 缺 SPS/PPS → 拒绝。
	if err := r.SequenceHeader(nil, testPPS); !errors.Is(err, ErrNoParamSet) {
		t.Fatalf("缺 SPS 应 ErrNoParamSet: %v", err)
	}
	if err := r.WriteAnnexB(frame(true), 0); err == nil {
		t.Fatal("序列头未发前 WriteAnnexB 应拒绝")
	}
	if err := r.SequenceHeader(testSPS, testPPS); err != nil {
		t.Fatalf("SequenceHeader: %v", err)
	}

	// 解析序列头 tag：偏移 13 = header(9)+prev0(4)。
	b := r.Full()
	off := 13
	if b[off] != 18 && b[off] != 9 {
		t.Fatalf("首个 tag 应为 video(9): %d", b[off])
	}
	dataSize := int(b[off+1])<<16 | int(b[off+2])<<8 | int(b[off+3])
	if dataSize <= 0 || off+11+dataSize+4 > len(b) {
		t.Fatalf("tag 长度异常: %d", dataSize)
	}
	body := b[off+11 : off+11+dataSize]
	if body[0] != 0x17 || body[1] != 0x00 {
		t.Fatalf("序列头 tag 应 0x17/0x00: % X", body[:3])
	}
	// AVCDecoderConfigurationRecord 结构断言。
	rec := body[5:]
	if rec[0] != 0x01 || rec[4] != 0xFF || rec[5] != 0xE1 {
		t.Fatalf("配置记录头异常: % X", rec[:6])
	}
	spsLen := int(rec[6])<<8 | int(rec[7])
	if !bytes.Equal(rec[8:8+spsLen], testSPS) {
		t.Fatalf("SPS 不符: % X", rec[8:8+spsLen])
	}
	if rec[8+spsLen] != 0x01 {
		t.Fatalf("numPPS 应 1")
	}
	ppsLen := int(rec[9+spsLen])<<8 | int(rec[10+spsLen])
	if !bytes.Equal(rec[11+spsLen:11+spsLen+ppsLen], testPPS) {
		t.Fatalf("PPS 不符")
	}

	// MetaData：script tag（type 18）+ @setDataFrame/onMetaData。
	// 遍历口径：tag header 11 字节（type@0，DataSize@1..3）+ data + prev(4)。
	r.MetaData(320, 240)
	foundScript := false
	bb := r.Full()
	for off2 := 13; off2+11 <= len(bb); {
		dataSz := int(bb[off2+1])<<16 | int(bb[off2+2])<<8 | int(bb[off2+3])
		if off2+11+dataSize+4 > len(bb) {
			break
		}
		if bb[off2] == 18 {
			foundScript = true
			if !bytes.Contains(bb[off2+11:off2+11+dataSz], []byte("onMetaData")) {
				t.Fatal("script tag 应含 onMetaData")
			}
			break
		}
		off2 += 11 + dataSz + 4
	}
	if !foundScript {
		t.Fatal("onMetaData script tag 缺失")
	}
}

func TestWriteAnnexBAndAVCC(t *testing.T) {
	r := New()
	if err := r.SequenceHeader(testSPS, testPPS); err != nil {
		t.Fatal(err)
	}
	before := len(r.Full())
	f := frame(true) // 双 NALU（4 字节 + 3 字节起始码）
	if err := r.WriteAnnexB(f, 1234); err != nil {
		t.Fatalf("WriteAnnexB: %v", err)
	}
	b := r.Full()
	off := before
	if b[off] != 9 {
		t.Fatalf("tag type 应 9: %d", b[off])
	}
	// 时戳：1234ms → 低 24 位 = 1234，扩展 = 0。
	ts := int(b[off+4])<<16 | int(b[off+5])<<8 | int(b[off+6])
	if ts != 1234 || b[off+7] != 0 {
		t.Fatalf("时戳异常: %d/%d", ts, b[off+7])
	}
	dataSize := int(b[off+1])<<16 | int(b[off+2])<<8 | int(b[off+3])
	body := b[off+11 : off+11+dataSize]
	if body[0] != 0x17 || body[1] != 0x01 {
		t.Fatalf("IDR 帧应 0x17/0x01: % X", body[:2])
	}
	// AVCC 重转回 AnnexB 等价（NALU 序列一致）。
	avcc := body[5:] // 跳过 FrameType/AVCPacketType/CompositionTime
	nals := splitAVCC(avcc)
	if len(nals) != 2 {
		t.Fatalf("AVCC 应 2 NALU: %d", len(nals))
	}
	if !bytes.Equal(nals[0], []byte{5, 0x01, 0x02, 0x03}) || !bytes.Equal(nals[1], []byte{1, 0x09, 0x0A}) {
		t.Fatalf("NALU 内容不符: % X / % X", nals[0], nals[1])
	}
	// 非关键帧帧型位。
	before = len(r.Full())
	if err := r.WriteAnnexB(frame(false), 1300); err != nil {
		t.Fatal(err)
	}
	b = r.Full()
	off = before
	if b[off+11] != 0x27 {
		t.Fatalf("非 IDR 帧应 0x27: % X", b[off+11])
	}
}

func TestSplitAnnexBNals(t *testing.T) {
	// 4 字节起始码开头 + 3 字节起始码分隔 + 尾部 NALU。
	b := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xAA, 0x00, 0x00, 0x01, 0x41, 0xBB, 0xCC}
	nals := splitAnnexBNals(b)
	if len(nals) != 2 {
		t.Fatalf("应 2 NALU: %d", len(nals))
	}
	if !bytes.Equal(nals[0], []byte{0x65, 0xAA}) || !bytes.Equal(nals[1], []byte{0x41, 0xBB, 0xCC}) {
		t.Fatalf("NALU 切分异常: % X / % X", nals[0], nals[1])
	}
	// 空 NALU（连续起始码）容忍。
	if nals := splitAnnexBNals([]byte{0, 0, 0, 1}); len(nals) != 0 {
		t.Fatalf("空流应 0 NALU: %d", len(nals))
	}
	// 空输入。
	if nals := splitAnnexBNals(nil); len(nals) != 0 {
		t.Fatalf("nil 应 0 NALU")
	}
}

func TestWriteAnnexBEmptyFails(t *testing.T) {
	r := New()
	if err := r.SequenceHeader(testSPS, testPPS); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteAnnexB([]byte{0x00, 0x00, 0x00, 0x01}, 0); err == nil {
		t.Fatal("空 NALU 流应拒绝")
	}
}

// splitAVCC 按 4 字节长度前缀切 NALU（测试用）。
func splitAVCC(b []byte) [][]byte {
	var out [][]byte
	for len(b) >= 4 {
		n := int(binary.BigEndian.Uint32(b[:4]))
		if 4+n > len(b) {
			break
		}
		out = append(out, b[4:4+n])
		b = b[4+n:]
	}
	return out
}
