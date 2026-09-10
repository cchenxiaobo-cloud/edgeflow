package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestV0310SyntheticSequence 验证合成源出帧：seq 单调、尺寸一致、帧间隔≈FPS、
// 配置默认值填补（US-1）。
func TestV0310SyntheticSequence(t *testing.T) {
	src := NewSyntheticSource(SyntheticConfig{FPS: 50, Width: 64, Height: 48, Block: 8})
	cfg := src.Config()
	if cfg.FPS != 50 || cfg.Width != 64 || cfg.Height != 48 || cfg.JPEGQuality != 70 {
		t.Fatalf("默认值填补错误: %+v", cfg)
	}
	ctx := context.Background()
	start := time.Now()
	var prevSeq uint64
	for i := 0; i < 3; i++ {
		f, err := src.Next(ctx)
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		if f.Seq != prevSeq+1 {
			t.Fatalf("seq 不单调: prev=%d got=%d", prevSeq, f.Seq)
		}
		prevSeq = f.Seq
		if f.Width != 64 || f.Height != 48 {
			t.Fatalf("尺寸不一致: %dx%d", f.Width, f.Height)
		}
		if f.TsMs <= 0 || len(f.JPEG) == 0 {
			t.Fatalf("帧元数据异常: ts=%d jpegLen=%d", f.TsMs, len(f.JPEG))
		}
	}
	// 第 2→3 帧间隔应≈20ms（50fps），放宽到 [5, 200]ms 防抖。
	el := time.Since(start)
	if el < 30*time.Millisecond {
		t.Fatalf("3 帧过快（%v），节流失效", el)
	}
	if el > 500*time.Millisecond {
		t.Fatalf("3 帧过慢（%v）", el)
	}
}

// TestV0310SyntheticDeterministic 同参数同序号帧 JPEG 逐字节一致（时间戳除外）。
func TestV0310SyntheticDeterministic(t *testing.T) {
	mk := func() *Frame {
		s := NewSyntheticSource(SyntheticConfig{FPS: 1000, Width: 48, Height: 32, Block: 6})
		f, err := s.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		return f
	}
	a, b := mk(), mk()
	if string(a.JPEG) != string(b.JPEG) {
		t.Fatalf("同参数同序号帧不一致（len %d vs %d）", len(a.JPEG), len(b.JPEG))
	}
}

// TestV0310JPEGRoundTrip 帧图可被 image.Decode 往返且含热区红色像素（US-1）。
func TestV0310JPEGRoundTrip(t *testing.T) {
	src := NewSyntheticSource(SyntheticConfig{FPS: 1000, Width: 64, Height: 64, Block: 12})
	f, err := src.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	img, err := DecodeFrameJPEG(f)
	if err != nil {
		t.Fatalf("解码往返失败: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 64 || b.Dy() != 64 {
		t.Fatalf("解码尺寸 %v", b)
	}
	red := 0
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r > 40000 && g < 30000 && bl < 30000 {
				red++
			}
		}
	}
	if red == 0 {
		t.Fatalf("帧内未发现热区红色像素")
	}
}

// stubServer 起一个推理 stub：按 handler 处理，收到的请求体存档。
type stubServer struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []inferRequest
	handler func(w http.ResponseWriter, r *http.Request, body inferRequest)
}

func newStub(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body inferRequest)) *stubServer {
	t.Helper()
	s := &stubServer{handler: handler}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body inferRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		handler(w, r, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// TestV0310HTTPInferencerOK 成功往返：请求字段保真、响应解码、元数据回填（US-2）。
func TestV0310HTTPInferencerOK(t *testing.T) {
	src := NewSyntheticSource(SyntheticConfig{FPS: 1000, Width: 32, Height: 24, Block: 6})
	f, _ := src.Next(context.Background())
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request, body inferRequest) {
		if body.DeviceName != "cam-01" || body.FrameSeq != f.Seq || body.Width != 32 || body.Height != 24 {
			t.Errorf("请求字段不保真: %+v", body)
		}
		if len(body.Image) == 0 {
			t.Errorf("image base64 为空")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(InferenceResult{
			Detections: []Detection{{Label: "person", Score: 0.9, BBox: [4]float64{1, 2, 3, 4}}},
		})
	})
	inf := NewHTTPInferencer(stub.URL, "cam-01", "det-v1", time.Second)
	res, err := inf.Infer(context.Background(), f)
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if res.DeviceName != "cam-01" || res.FrameSeq != f.Seq || res.TsMs != f.TsMs {
		t.Fatalf("响应元数据回填失败: %+v", res)
	}
	if len(res.Detections) != 1 || res.Detections[0].Label != "person" || res.Detections[0].Score != 0.9 {
		t.Fatalf("检测结果不保真: %+v", res.Detections)
	}
}

// TestV0310HTTPInferencerErrors 5xx / 坏 JSON / 空帧 显式报错（US-2）。
func TestV0310HTTPInferencerErrors(t *testing.T) {
	src := NewSyntheticSource(SyntheticConfig{})
	f, _ := src.Next(context.Background())

	stub500 := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ inferRequest) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	inf := NewHTTPInferencer(stub500.URL, "cam-01", "", time.Second)
	if _, err := inf.Infer(context.Background(), f); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("5xx 未报错: %v", err)
	}

	stubBad := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ inferRequest) {
		_, _ = w.Write([]byte("not-json"))
	})
	inf2 := NewHTTPInferencer(stubBad.URL, "cam-01", "", time.Second)
	if _, err := inf2.Infer(context.Background(), f); err == nil {
		t.Fatalf("坏 JSON 未报错")
	}

	inf3 := NewHTTPInferencer("http://127.0.0.1:1/nope", "cam-01", "", 300*time.Millisecond)
	if _, err := inf3.Infer(context.Background(), f); err == nil {
		t.Fatalf("连接拒绝未报错")
	}

	if _, err := inf3.Infer(context.Background(), &Frame{}); err == nil {
		t.Fatalf("空帧未报错")
	}
}

// TestV0310LatestSlotDrop 背压槽：覆盖丢帧计数、Take 只取新帧（US-3）。
func TestV0310LatestSlotDrop(t *testing.T) {
	slot := NewLatestSlot()
	mk := func(seq uint64) *Frame { return &Frame{Seq: seq} }
	slot.Put(mk(1))
	slot.Put(mk(2)) // 覆盖 seq=1 → dropped=1
	if got := slot.Dropped(); got != 1 {
		t.Fatalf("丢弃计数=%d 期望 1", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f, ok := slot.Take(ctx, 0)
	if !ok || f.Seq != 2 {
		t.Fatalf("Take 应得最新帧 2: ok=%v seq=%d", ok, f.Seq)
	}
	// afterSeq=2 且无新帧 → 等待直至超时。
	ctx2, cancel2 := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel2()
	if _, ok := slot.Take(ctx2, 2); ok {
		t.Fatalf("afterSeq=2 不应有帧返回")
	}
	// 新帧到达 → 立即返回。
	go func() {
		time.Sleep(10 * time.Millisecond)
		slot.Put(mk(3))
	}()
	ctx3, cancel3 := context.WithTimeout(context.Background(), time.Second)
	defer cancel3()
	if f, ok := slot.Take(ctx3, 2); !ok || f.Seq != 3 {
		t.Fatalf("等待新帧失败: ok=%v", ok)
	}
}

// TestV0310SanitizeInferenceJSON 截断按 UTF-8 边界（US-4 支撑）。
func TestV0310SanitizeInferenceJSON(t *testing.T) {
	long := `{"label":"目标` + strings.Repeat("x", 600) + `"}`
	got := SanitizeInferenceJSON(long, 100)
	if len(got) > 103 {
		t.Fatalf("截断超限: %d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("截断未补省略号: %q", got)
	}
	if SanitizeInferenceJSON("short", 100) != "short" {
		t.Fatalf("短串不应截断")
	}
	if SanitizeInferenceJSON("", 10) != "" {
		t.Fatalf("空串")
	}
	_ = errors.New // 保持 import 稳定
	_ = fmt.Sprintf
}
