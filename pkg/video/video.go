// Package video 提供边缘视频流管理与推理对接的核心抽象（v0.31.0，阶段一，
// specs/0004）：帧源（FrameSource）— 背压帧槽（LatestSlot）— 推理服务
// （Inferencer）的最小链路面。
//
// 阶段一边界（spec 0004 分段声明）：
//   - 帧源仅内置合成源（synthetic，确定性可测）；RTSP/GB28181 实源拉流
//     需第三方库或外部进程桥，与零依赖硬约束冲突，归阶段二单独裁定；
//   - 推理对接为 HTTP JSON 契约（帧 JPEG base64 → 检测框数组），GPU
//     运行时集成归阶段二；
//   - 所有类型零第三方依赖（image/jpeg 为 stdlib）。
package video

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Frame 是一帧视频：单调序号 + 毫秒时间戳 + JPEG 编码图像。
type Frame struct {
	Seq    uint64 `json:"frameSeq"`
	TsMs   int64  `json:"tsMs"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	JPEG   []byte `json:"-"`
}

// Detection 是单条推理检测结果；BBox 为 [x, y, w, h]（像素，浮点）。
type Detection struct {
	Label string     `json:"label"`
	Score float64    `json:"score"`
	BBox  [4]float64 `json:"bbox"`
}

// InferenceResult 是一帧的推理结果（HTTP 契约的响应体，亦是留痕/上行的载荷）。
type InferenceResult struct {
	DeviceName string      `json:"deviceName"`
	FrameSeq   uint64      `json:"frameSeq"`
	TsMs       int64       `json:"tsMs"`
	Model      string      `json:"model,omitempty"`
	Detections []Detection `json:"detections"`
}

// FrameSource 是视频帧源抽象：Next 阻塞产出下一帧（帧间间隔由实现决定），
// ctx 取消或源枯竭时返回 error。实现必须并发安全或由单 goroutine 独占使用
// （video mapper 为后者）。
type FrameSource interface {
	Next(ctx context.Context) (*Frame, error)
}

// SyntheticConfig 是合成源的参数（全部有非零默认值，零值可用）。
type SyntheticConfig struct {
	FPS         int `json:"fps"`         // 出帧帧率（>=1，默认 5）
	Width       int `json:"width"`       // 帧宽（>=16，默认 320）
	Height      int `json:"height"`      // 帧高（>=16，默认 240）
	JPEGQuality int `json:"jpegQuality"` // JPEG 质量 1-100（默认 70）
	// BlockSize 是移动热区边长（像素，默认 24）；热区随帧序号沿对角线
	// 折返移动，供推理契约联调与画面变化的确定性验证。
	Block int `json:"block"`
}

func (c *SyntheticConfig) fillDefaults() {
	if c.FPS <= 0 {
		c.FPS = 5
	}
	if c.Width < 16 {
		c.Width = 320
	}
	if c.Height < 16 {
		c.Height = 240
	}
	if c.JPEGQuality <= 0 || c.JPEGQuality > 100 {
		c.JPEGQuality = 70
	}
	if c.Block <= 0 || c.Block >= c.Width || c.Block >= c.Height {
		c.Block = 24
	}
}

// SyntheticFrameSource 是确定性合成帧源（阶段一唯一内置源）：背景灰度
// 渐变条纹 + 随帧序号折返移动的热区方块，JPEG 编码出帧。同参数同序号
// 产出逐字节一致的帧（时间戳除外），供测试与联调。
type SyntheticFrameSource struct {
	cfg   SyntheticConfig
	seq   uint64
	start time.Time
}

// NewSyntheticSource 创建合成源；cfg 零值字段取默认。
func NewSyntheticSource(cfg SyntheticConfig) *SyntheticFrameSource {
	cfg.fillDefaults()
	return &SyntheticFrameSource{cfg: cfg, start: time.Now()}
}

// Config 返回填补默认值后的配置副本。
func (s *SyntheticFrameSource) Config() SyntheticConfig { return s.cfg }

// Next 产出下一帧：按 FPS 节流出帧（首帧立即）；ctx 取消返回 ctx.Err()。
// JPEG 编码失败（实际不可达，内存图像）按错误透传。
func (s *SyntheticFrameSource) Next(ctx context.Context) (*Frame, error) {
	if s.seq == 0 {
		// 首帧立即；后续按帧间隔等待。
	} else {
		interval := time.Second / time.Duration(s.cfg.FPS)
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	s.seq++
	img := s.render(s.seq)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: s.cfg.JPEGQuality}); err != nil {
		return nil, fmt.Errorf("video: 合成帧 JPEG 编码失败: %w", err)
	}
	return &Frame{
		Seq:    s.seq,
		TsMs:   time.Now().UnixMilli(),
		Width:  s.cfg.Width,
		Height: s.cfg.Height,
		JPEG:   buf.Bytes(),
	}, nil
}

// render 渲染第 seq 帧：背景竖向灰度渐变条纹（每 16px 一档，档位随 seq
// 轮转）+ 对角折返热区方块（高对比）。
func (s *SyntheticFrameSource) render(seq uint64) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, s.cfg.Width, s.cfg.Height))
	phase := int(seq) % 16
	for y := 0; y < s.cfg.Height; y++ {
		for x := 0; x < s.cfg.Width; x++ {
			band := uint8(((x/16 + phase) % 16) * 16)
			img.Set(x, y, color.Gray{Y: band})
		}
	}
	// 热区沿对角折返：0..(W-Block) 往返。
	spanX := s.cfg.Width - s.cfg.Block
	spanY := s.cfg.Height - s.cfg.Block
	cycle := 2 * (spanX + spanY)
	pos := int(seq) % cycle
	bx, by := s.fold(spanX, spanY, pos)
	for y := by; y < by+s.cfg.Block; y++ {
		for x := bx; x < bx+s.cfg.Block; x++ {
			img.Set(x, y, color.RGBA{R: 220, G: 40, B: 40, A: 255})
		}
	}
	return img
}

// fold 把线性位置 pos 映射到 (x,y) 对角折返坐标（蛇形路径）。
func (s *SyntheticFrameSource) fold(spanX, spanY, pos int) (int, int) {
	if pos < spanX {
		return pos, 0
	}
	pos -= spanX
	if pos < spanY {
		return spanX, pos
	}
	pos -= spanY
	if pos < spanX {
		return spanX - pos, spanY
	}
	pos -= spanX
	return 0, spanY - pos
}

// Inferencer 是推理服务抽象：对单帧推理返回结果。实现须并发安全或由单
// goroutine 独占使用。
type Inferencer interface {
	Infer(ctx context.Context, f *Frame) (*InferenceResult, error)
}

// HTTPInferencer 通过 HTTP JSON 契约调用推理服务：
//
//	POST {url}  {"deviceName","frameSeq","tsMs","width","height","image"(JPEG base64),"model"?}
//	200 OK      {"deviceName","frameSeq","tsMs","model"?,"detections":[{"label","score","bbox":[x,y,w,h]}]}
//
// 非 200 / 坏 JSON / ctx 超时均显式报错。
type HTTPInferencer struct {
	url        string
	deviceName string
	model      string
	client     *http.Client
}

// NewHTTPInferencer 创建 HTTP 推理客户端；timeout<=0 取 2s 默认。
func NewHTTPInferencer(url, deviceName, model string, timeout time.Duration) *HTTPInferencer {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &HTTPInferencer{
		url:        url,
		deviceName: deviceName,
		model:      model,
		client:     &http.Client{Timeout: timeout},
	}
}

// inferRequest 是推理请求体（image 为帧 JPEG 的 base64）。
type inferRequest struct {
	DeviceName string `json:"deviceName"`
	FrameSeq   uint64 `json:"frameSeq"`
	TsMs       int64  `json:"tsMs"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Image      string `json:"image"`
	Model      string `json:"model,omitempty"`
}

// Infer 执行一次推理调用。
func (h *HTTPInferencer) Infer(ctx context.Context, f *Frame) (*InferenceResult, error) {
	if f == nil || len(f.JPEG) == 0 {
		return nil, fmt.Errorf("video: 推理入参帧为空")
	}
	req := inferRequest{
		DeviceName: h.deviceName,
		FrameSeq:   f.Seq,
		TsMs:       f.TsMs,
		Width:      f.Width,
		Height:     f.Height,
		Image:      base64.StdEncoding.EncodeToString(f.JPEG),
		Model:      h.model,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("video: 推理请求编码失败: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("video: 推理请求构造失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("video: 推理服务请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_sn, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		_ = _sn
		return nil, fmt.Errorf("video: 推理服务返回 %d", resp.StatusCode)
	}
	var out InferenceResult
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("video: 推理响应解码失败: %w", err)
	}
	// 回填元数据：服务端可省略 deviceName/frameSeq/tsMs（以帧为准）。
	if out.DeviceName == "" {
		out.DeviceName = h.deviceName
	}
	if out.FrameSeq == 0 {
		out.FrameSeq = f.Seq
	}
	if out.TsMs == 0 {
		out.TsMs = f.TsMs
	}
	return &out, nil
}

// LatestSlot 是 latest-wins 背压帧槽：生产者 Put 覆盖最新帧，消费者 Take
// 取走尚未消费的最新帧（取后清槽）；覆盖**未消费**帧时累计丢弃数（背压
// 语义：推理慢于出帧时丢旧帧保最新，不排队堆积；已消费帧的后续覆盖不计）。
// 复核 P1-2 修复：Take 清槽，覆盖已消费帧不再误计丢弃。
type LatestSlot struct {
	mu      sync.Mutex
	frame   *Frame
	notify  chan struct{}
	dropped atomic.Uint64
}

// NewLatestSlot 创建空槽。
func NewLatestSlot() *LatestSlot {
	return &LatestSlot{notify: make(chan struct{})}
}

// Put 覆盖槽内最新帧；被覆盖的未消费帧计入丢弃。
func (s *LatestSlot) Put(f *Frame) {
	s.mu.Lock()
	if s.frame != nil {
		s.dropped.Add(1)
	}
	s.frame = f
	old := s.notify
	s.notify = make(chan struct{})
	s.mu.Unlock()
	close(old)
}

// Take 返回 seq 大于 afterSeq 的最新帧（无则等待，ctx 取消返回 false）。
// 消费者以 afterSeq=上次取到的帧序号调用实现"只取新帧"。
func (s *LatestSlot) Take(ctx context.Context, afterSeq uint64) (*Frame, bool) {
	for {
		s.mu.Lock()
		fr := s.frame
		ch := s.notify
		if fr != nil && fr.Seq > afterSeq {
			s.frame = nil // 取走清槽：此后覆盖不计丢弃（复核 P1-2）
			s.mu.Unlock()
			return fr, true
		}
		s.mu.Unlock()
		select {
		case <-ch:
			// 槽已更新，重查。
		case <-ctx.Done():
			return nil, false
		}
	}
}

// Dropped 返回累计丢弃帧数。
func (s *LatestSlot) Dropped() uint64 { return s.dropped.Load() }

// DecodeFrameJPEG 把帧 JPEG 解码为图像（测试/调试辅助；生产链路不解码）。
func DecodeFrameJPEG(f *Frame) (image.Image, error) {
	if f == nil || len(f.JPEG) == 0 {
		return nil, fmt.Errorf("video: 帧为空")
	}
	img, err := jpeg.Decode(bytes.NewReader(f.JPEG))
	if err != nil {
		return nil, fmt.Errorf("video: 帧 JPEG 解码失败: %w", err)
	}
	return img, nil
}

// SanitizeInferenceJSON 截断推理结果 JSON 到 maxBytes（留痕用，防止长结果
// 膨胀台账）；截断处按 UTF-8 rune 边界回退后补 "…"。
func SanitizeInferenceJSON(b string, maxBytes int) string {
	if maxBytes <= 0 || len(b) <= maxBytes {
		return b
	}
	cut := b[:maxBytes]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size != 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
