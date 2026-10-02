// RTP/H.264 解复用与重组（v0.42.0，spec 0015 US-2）。
//
// 载荷格式（RFC 6184 最小面）：
//   - 单 NAL 包（NAL header 1 字节，type 1-23）：直通；
//   - STAP-A（type 24）：多个 [size(2)][NAL] 依次分解；
//   - FU-A（type 28）：FU indicator + FU header（S/E/R/type），分片重组为
//     完整 NAL（S 开始重建、E 收尾输出；中间追加）。
//
// 输出：AnnexB 起始码 + NAL（不含原 NAL 头以外的封装）。
package rtspclient

import (
	"encoding/binary"
	"errors"
)

// annexbStart 是 AnnexB 起始码（00 00 00 01）。
var annexbStart = []byte{0x00, 0x00, 0x00, 0x01}

// maxFUANal 是 FU-A 重组单 NAL 的字节上限（P2-8：畸形分片流内存防护，
// 与 jpegScanner maxPending 同口径）。
const maxFUANal = 16 << 20

// fuAssembler 持有跨 RTP 包的 FU-A 分片重组状态（nil = 空闲）。
type fuAssembler struct {
	typ     byte   // 还原后的 NAL type（FU header type）
	fuStart bool   // 是否已收到 S 分片
	nal     []byte // 还原中的 NAL（header + 载荷）
}

// demuxH264 把一个 RTP video 载荷分解为 NAL 列表（无 AnnexB 起始码）。
// st 为跨包 FU-A 状态（调用方持有同一指针）。畸形载荷返回错误（调用方
// 计数丢弃，不断流）。
func demuxH264(payload []byte, st **fuAssembler) ([][]byte, error) {
	if len(payload) < 1 {
		return nil, errors.New("rtspclient: H.264 载荷为空")
	}
	typ := payload[0] & 0x1F
	switch {
	case typ >= 1 && typ <= 23:
		// 单 NAL 包：直通（载荷即完整 NAL）。
		return [][]byte{payload}, nil
	case typ == 24: // STAP-A
		return demuxSTAPA(payload)
	case typ == 28: // FU-A
		return demuxFUA(payload, st)
	case typ == 0, typ == 30, typ == 31:
		return nil, errors.New("rtspclient: H.264 NAL type 保留值")
	case typ >= 29 && typ <= 31:
		return nil, errors.New("rtspclient: FU-B/MTAP 为 TCP 单包分片形态之外（登记边界）")
	default: // 25-27（MTAP）、29+（FU-B）等：登记边界，丢弃
		return nil, errors.New("rtspclient: 不支持的 H.264 载荷形态")
	}
}

// demuxSTAPA 分解 STAP-A：[type24][STAP-A NAL HDR 隐含][size(2)][NAL]...。
// STAP-A 自身的 1 字节 NAL header 是共用头（NRI 位），分解出的各 NAL 保留
// 各自 1 字节头（载荷中的 NAL 含头）——RFC 6184：STAP-A 载荷 = NALU 1 size +
// NALU 1 + NALU 2 size + ...（各 NALU 含自身 NAL header）。
func demuxSTAPA(payload []byte) ([][]byte, error) {
	if len(payload) < 3 {
		return nil, errors.New("rtspclient: STAP-A 载荷过短")
	}
	var out [][]byte
	off := 1 // 跳过 STAP-A type 字节
	for off+2 <= len(payload) {
		size := int(binary.BigEndian.Uint16(payload[off : off+2]))
		off += 2
		if size == 0 || off+size > len(payload) {
			return nil, errors.New("rtspclient: STAP-A NALU 长度非法")
		}
		out = append(out, payload[off:off+size])
		off += size
	}
	if off != len(payload) {
		return nil, errors.New("rtspclient: STAP-A 载荷尾部残留")
	}
	return out, nil
}

// demuxFUA 处理 FU-A 分片：payload = [FU indicator][FU header][frag...]。
// FU indicator: F(1)|NRI(2)|Type(5)=28；FU header: S(1)|E(1)|R(1)|Type(5)。
// S 置位重建 NAL（原 type 取自 FU header，重建 NAL header = indicator 高 3 位
// + FU header type）；E 置位输出完整 NAL。
func demuxFUA(payload []byte, st **fuAssembler) ([][]byte, error) {
	if len(payload) < 2 {
		return nil, errors.New("rtspclient: FU-A 载荷过短")
	}
	indicator := payload[0]
	fuHdr := payload[1]
	sBit := fuHdr>>7&1 == 1
	eBit := fuHdr>>6&1 == 1
	typ := fuHdr & 0x1F
	frag := payload[2:]
	if sBit {
		// 重建 NAL header：F/NRI 取 indicator，type 取 FU header。
		hdr := (indicator & 0xE0) | typ
		*st = &fuAssembler{typ: typ, fuStart: true, nal: []byte{hdr}}
	}
	cur := *st
	if cur == nil || !cur.fuStart {
		return nil, errors.New("rtspclient: FU-A 缺 S 起始分片")
	}
	cur.nal = append(cur.nal, frag...)
	if len(cur.nal) > maxFUANal {
		*st = nil // 丢弃重组状态（畸形流防护）
		return nil, errors.New("rtspclient: FU-A 重组超限（≥16MB）")
	}
	if eBit {
		out := cur.nal
		*st = nil
		return [][]byte{out}, nil
	}
	return nil, nil
}
