// 演示用合成 H.264 帧源（v0.44.0，spec 0017 F0/F2）：为 FLV 分发端点提供
// 确定性 AnnexB 流（SPS/PPS 带外 + IDR/P 帧交替 + 25fps 时戳）。
//
// ⚠️ 演示/测试面（与 pkg/rtspclient/sim.go 同类）：非产品交付物——生产帧源
// （边缘推流/云端 RTSP 拉流）登记 KI §45 后续。
package flvremux

import (
	"context"
	"sync/atomic"
	"time"
)

// 合成参数（与 rtspclient 模拟器同形态，确定性）。
var (
	demoSPS = []byte{0x67, 0x42, 0xC0, 0x1E, 0xAB, 0x40, 0xA0, 0xFD, 0xFF, 0xE8}
	demoPPS = []byte{0x68, 0xCB, 0x83, 0xCB, 0x20}
)

// SyntheticH264Source 是合成 H.264 帧源（AnnexB；25fps；IDR 每 GOP 帧）。
type SyntheticH264Source struct {
	GOP int // IDR 间隔（帧数，默认 25）

	seq    atomic.Uint64
	frame0 time.Time
}

// NewSyntheticH264Source 创建合成源。
func NewSyntheticH264Source(gop int) *SyntheticH264Source {
	if gop <= 0 {
		gop = 25
	}
	return &SyntheticH264Source{GOP: gop, frame0: time.Now()}
}

// Params 返回 SPS/PPS（始终就绪）。
func (s *SyntheticH264Source) Params() ([]byte, []byte, error) {
	return demoSPS, demoPPS, nil
}

// Next 阻塞产出下一帧 AnnexB（25fps 节奏；ctx 取消返回错误）。
// 帧：偶数序 IDR（含 SPS/PPS 前置）+ 裸 IDR；奇数序 P 帧。
func (s *SyntheticH264Source) Next(ctx context.Context) ([]byte, uint64, error) {
	seq := s.seq.Add(1)
	if seq > 1 {
		// 25fps 节流（首帧立即）。
		target := s.frame0.Add(time.Duration(seq-1) * 40 * time.Millisecond)
		if d := time.Until(target); d > 0 {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-t.C:
			}
		}
	}
	tsMs := uint64(time.Since(s.frame0).Milliseconds())
	isIDR := seq%uint64(s.GOP) == 1
	var out []byte
	if isIDR {
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, demoSPS...)
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, demoPPS...)
		out = append(out, 0x00, 0x00, 0x00, 0x01)
	}
	nalType := byte(1) // P 帧（非 IDR）
	if isIDR {
		nalType = 5
	}
	nal := make([]byte, 32)
	nal[0] = nalType
	for i := 1; i < len(nal); i++ {
		nal[i] = byte(seq*7 + uint64(i))
	}
	out = append(out, 0x00, 0x00, 0x00, 0x01)
	out = append(out, nal...)
	return out, tsMs, nil
}
