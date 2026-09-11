// v0.34.0（specs/0007）Mapper 层测试：源错误可见降级（streamOn→0 +
// sourceErrors）、重启恢复、bridge 实源装配链路、配置校验扩展。
package video

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"edgeflow/edge/pkg/mapper"
	pkgvideo "edgeflow/pkg/video"
)

// jpegForTest 生成 w x h 的合法 JPEG 字节（bridge e2e 帧文件）。
func jpegForTest(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(2, 2, color.RGBA{R: 200, G: 30, B: 30, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 60}); err != nil {
		t.Fatalf("jpeg 编码: %v", err)
	}
	return buf.Bytes()
}

// chmodX 给脚本加执行位。
func chmodX(path string) error { return os.Chmod(path, 0o755) }

// errSource 产出 n 帧后返回错误（模拟实源不可用/配置性失败）。
type errSource struct {
	n   int
	err error
}

func (s *errSource) Next(ctx context.Context) (*pkgvideo.Frame, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s.n <= 0 {
		return nil, s.err
	}
	s.n--
	time.Sleep(10 * time.Millisecond)
	return &pkgvideo.Frame{Seq: uint64(100 - s.n), TsMs: time.Now().UnixMilli(),
		Width: 32, Height: 24, JPEG: []byte{0xFF, 0xD8, 0xFF, 0xD9}}, nil
}

// TestV0340MapperSourceErrorDegrade 源错误可见降级 + 重启恢复（v0310 复核
// P2-1 闭环锚）：错误后 streamOn=0 且循环真停；stream=1 重启恢复；
// 正常停止不增 sourceErrors。
func TestV0340MapperSourceErrorDegrade(t *testing.T) {
	inf := &stubInfer{result: &pkgvideo.InferenceResult{}}
	var factoryCalls int
	m, err := NewMapper(testConfig("http://unused"),
		WithSource(func(*Config) (pkgvideo.FrameSource, error) {
			factoryCalls++
			if factoryCalls == 1 {
				return &errSource{n: 2, err: fmt.Errorf("source boom")}, nil
			}
			return newStubSource(5, false), nil
		}),
		WithInferencer(func(*Config) pkgvideo.Inferencer { return inf }))
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 降级断言：sourceErrors≥1 且 streamOn=0。
	waitFor(t, 5*time.Second, func() bool {
		props, _ := m.Collect()
		return props["sourceErrors"] >= 1 && props["streamOn"] == 0
	}, "源错误后应可见降级（sourceErrors≥1, streamOn=0）")
	// 循环真停：推理计数稳定。
	n1 := inf.callCount()
	time.Sleep(120 * time.Millisecond)
	if n2 := inf.callCount(); n2 != n1 {
		t.Fatalf("降级后推理循环仍在跑: %d → %d", n1, n2)
	}
	props, _ := m.Collect()
	if props["sourceErrors"] != 1 {
		t.Fatalf("sourceErrors 应为 1: %v", props)
	}
	// stream=1 重启恢复。
	rep, err := m.HandleCommand(mapper.DeviceCommand{Property: "stream", Value: 1})
	if err != nil || rep.Properties["streamOn"] != 1 {
		t.Fatalf("重启失败: err=%v props=%v", err, rep.Properties)
	}
	waitFor(t, 3*time.Second, func() bool { return inf.callCount() > n1 }, "重启后推理未恢复")
	// 正常停止：不新增 sourceErrors。
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	props, _ = m.Collect()
	if props["streamOn"] != 0 || props["sourceErrors"] != 1 {
		t.Fatalf("正常停止语义: %v", props)
	}
	if factoryCalls != 2 {
		t.Fatalf("源工厂应被调 2 次: %d", factoryCalls)
	}
}

// TestV0340MapperBridgeE2E bridge 实源装配级全链路：配置文件（type=bridge，
// sh 脚本产 2 帧）→ LoadConfig → NewMapper（默认工厂）→ 真 HTTP 推理 stub
// → 指标断言。
func TestV0340MapperBridgeE2E(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"detections":[{"label":"motion","score":0.6,"bbox":[1,1,4,4]}]}`))
	}))
	t.Cleanup(stub.Close)

	dir := t.TempDir()
	f1 := filepath.Join(dir, "f1.jpg")
	f2 := filepath.Join(dir, "f2.jpg")
	for i, p := range []string{f1, f2} {
		if err := writeFile(p, jpegForTest(t, 40+i, 30)); err != nil {
			t.Fatalf("写帧文件: %v", err)
		}
	}
	// 帧间留 300ms 间隔：推理（本地 stub）跟得上，验证不丢帧路径；
	// 结尾 sleep 保持进程存活（流持续语义）。
	script := filepath.Join(dir, "bridge.sh")
	if err := writeFile(script, []byte(fmt.Sprintf("#!/bin/sh\ncat %s; sleep 0.3; cat %s; sleep 30\n", f1, f2))); err != nil {
		t.Fatalf("写脚本: %v", err)
	}
	if err := chmodX(script); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	cfgPath := filepath.Join(dir, "video.json")
	cfgJSON := fmt.Sprintf(`{"deviceName":"cam-bridge","source":{"type":"bridge","command":"/bin/sh","args":[%q],"reconnectMs":50},"inference":{"url":%q,"timeoutMs":2000}}`,
		script, stub.URL)
	if err := writeFile(cfgPath, []byte(cfgJSON)); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	m, err := NewMapper(cfg)
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	ok := false
	for time.Now().Before(deadline) {
		props, _ := m.Collect()
		if props["inferTotal"] >= 2 {
			ok = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		props, _ := m.Collect()
		t.Fatalf("bridge e2e 推理不足 2 帧; props=%v", props)
	}
	props, _ := m.Collect()
	if props["streamOn"] != 1 || props["sourceErrors"] != 0 {
		t.Fatalf("bridge e2e 指标异常: %v", props)
	}
	if props["detectionsLast"] != 1 {
		t.Fatalf("检测数异常: %v", props)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestV0340MapperConfigValidateStage2 配置校验扩展：mjpeg/bridge 必填字段；
// 合法配置通过。
func TestV0340MapperConfigValidateStage2(t *testing.T) {
	if err := (&Config{DeviceName: "c", Source: SourceConfig{Type: "mjpeg"}, Inference: InferConfig{URL: "x"}}).validate(); err == nil {
		t.Fatal("mjpeg 缺 url 应拒绝")
	}
	if err := (&Config{DeviceName: "c", Source: SourceConfig{Type: "bridge"}, Inference: InferConfig{URL: "x"}}).validate(); err == nil {
		t.Fatal("bridge 缺 command 应拒绝")
	}
	if err := (&Config{DeviceName: "c", Source: SourceConfig{Type: "mjpeg", URL: "http://cam/mjpg"}, Inference: InferConfig{URL: "x"}}).validate(); err != nil {
		t.Fatalf("合法 mjpeg 应通过: %v", err)
	}
	if err := (&Config{DeviceName: "c", Source: SourceConfig{Type: "bridge", Command: "ffmpeg"}, Inference: InferConfig{URL: "x"}}).validate(); err != nil {
		t.Fatalf("合法 bridge 应通过: %v", err)
	}
	if err := (&Config{DeviceName: "c", Source: SourceConfig{Type: "rtsp"}, Inference: InferConfig{URL: "x"}}).validate(); err == nil {
		t.Fatal("rtsp 仍应拒绝（原生协议栈未实现）")
	}
}
