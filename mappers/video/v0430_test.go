// v0.43.0（spec 0016 US-2）媒体采集触发单测：检出驱动采集（节流）、
// 片段帧数界长与快照内容、无 sink / 未启用零行为。
package video

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"edgeflow/pkg/mediaup"
	pkgvideo "edgeflow/pkg/video"
)

// clipSink 收集采集输出。
type clipSink struct {
	mu    sync.Mutex
	clips []mediaup.Clip
}

func (s *clipSink) HandleClip(c mediaup.Clip) {
	s.mu.Lock()
	s.clips = append(s.clips, c)
	s.mu.Unlock()
}

func (s *clipSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clips)
}

func (s *clipSink) first() (mediaup.Clip, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clips) == 0 {
		return mediaup.Clip{}, false
	}
	return s.clips[0], true
}

// TestV0430MediaCapture 检出≥1 → 采集（快照=当前帧；片段=最近 N 帧拼接）、
// 节流间隔生效、片段帧数 ≤ segmentFrames。
func TestV0430MediaCapture(t *testing.T) {
	sink := &clipSink{}
	cfg := &Config{
		DeviceName: "cam-cap",
		Source:     SourceConfig{Type: "synthetic"},
		Inference:  InferConfig{URL: "http://stub"},
		Media:      MediaConfig{Enabled: true, SegmentFrames: 4, MinIntervalMs: 200},
	}
	m, err := NewMapper(cfg,
		WithMediaSink(sink),
		WithSource(func(*Config) (pkgvideo.FrameSource, error) { return newStubSource(60, false), nil }),
		WithInferencer(func(*Config) pkgvideo.Inferencer {
			return &stubInfer{result: &pkgvideo.InferenceResult{
				Detections: []pkgvideo.Detection{{Label: "motion", Score: 0.9, BBox: [4]float64{1, 1, 4, 4}}},
			}}
		}),
	)
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })

	// 等待 ≥2 次采集（60 帧 × 20ms ≈ 1.2s；节流 200ms → 上限 ~6 次）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sink.count() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if sink.count() < 2 {
		t.Fatalf("应有 ≥2 次采集（实际 %d）", sink.count())
	}
	// 节流上界：采集数不超过 时长/200ms + 余量（60 帧 20ms 总时长 ~1.2s）。
	if n := sink.count(); n > 10 {
		t.Fatalf("节流未生效：采集 %d 次（60 帧应 ≤10）", n)
	}

	c, ok := sink.first()
	if !ok {
		t.Fatal("无采集输出")
	}
	// 快照 = 当前帧（stub 帧为 4 字节 FFD8 FFD9）。
	if !bytes.Equal(c.Snapshot, []byte{0xFF, 0xD8, 0xFF, 0xD9}) {
		t.Fatalf("快照应等于当前帧: % X", c.Snapshot)
	}
	// 片段 = 最近 N 帧拼接（stub 每帧 4 字节）；帧数 1..4、长度 == 帧数×4。
	if c.FrameCount < 1 || c.FrameCount > 4 {
		t.Fatalf("片段帧数越界: %d", c.FrameCount)
	}
	if len(c.Segment) != c.FrameCount*4 {
		t.Fatalf("片段长度与帧数不一致: %d vs %d", len(c.Segment), c.FrameCount)
	}
	if c.Detections != 1 {
		t.Fatalf("检出数应带出: %d", c.Detections)
	}
	if c.WhenMs == 0 {
		t.Fatal("WhenMs 应填充")
	}
}

// TestV0430MediaZeroBehavior 未注入 sink / media 未启用 → 零行为（不采集、不 panic）。
func TestV0430MediaZeroBehavior(t *testing.T) {
	// 场景一：sink 未注入（media.enabled=true 也无出口）——零行为。
	cfg := &Config{
		DeviceName: "cam-zero",
		Source:     SourceConfig{Type: "synthetic"},
		Inference:  InferConfig{URL: "http://stub"},
		Media:      MediaConfig{Enabled: true, SegmentFrames: 4, MinIntervalMs: 10},
	}
	m, err := NewMapper(cfg,
		WithSource(func(*Config) (pkgvideo.FrameSource, error) { return newStubSource(30, true), nil }),
		WithInferencer(func(*Config) pkgvideo.Inferencer {
			return &stubInfer{result: &pkgvideo.InferenceResult{
				Detections: []pkgvideo.Detection{{Label: "m"}},
			}}
		}),
	)
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = m.Stop()

	// 场景二：sink 注入但 media 未启用——同样零行为（不可直接观测，断言不 panic
	// 且指标面正常运转即可）。
	sink := &clipSink{}
	cfg2 := &Config{
		DeviceName: "cam-zero2",
		Source:     SourceConfig{Type: "synthetic"},
		Inference:  InferConfig{URL: "http://stub"},
	}
	m2, err := NewMapper(cfg2,
		WithMediaSink(sink),
		WithSource(func(*Config) (pkgvideo.FrameSource, error) { return newStubSource(30, true), nil }),
		WithInferencer(func(*Config) pkgvideo.Inferencer {
			return &stubInfer{result: &pkgvideo.InferenceResult{
				Detections: []pkgvideo.Detection{{Label: "m"}},
			}}
		}),
	)
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	if err := m2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = m2.Stop()
	if sink.count() != 0 {
		t.Fatalf("media 未启用不应采集（实际 %d）", sink.count())
	}
}
