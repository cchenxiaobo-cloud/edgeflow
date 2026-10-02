// v0.42.0（specs/0015）Mapper 层 RTSP 源全链测试：模拟 RTSP 服务端 →
// RTSPSource（自研客户端拉流）→ 解码脚本（stdin 排空 + MJPEG stdout）→
// 推理 stub → 指标断言；含断流自愈（服务端 ResetAfterDur 强制断开）。
//
// 复核修订（v0.42 P1-3）：排空脚本用 `cat <&0`（显式 fd 复制，非交互 sh
// 下 `cat >/dev/null &` 的 stdin 已被重定向到 /dev/null、不会排空）；断言
// 具体化——framesTotal/inferTotal 门槛 + reconnects 增长 + 恢复后帧流继续
// 增长（防「管线早停仍全绿」）。
package video

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	rtsp "edgeflow/pkg/rtspclient"
)

// TestV0420MapperConfigValidateRTSP 配置校验：rtsp 源型 url+decoder 必填；
// 合法配置通过（v0.34 冻结用例语义迁移——行为演进=新测试文件，v0.42 P1-4）。
func TestV0420MapperConfigValidateRTSP(t *testing.T) {
	if err := (&Config{DeviceName: "c", Source: SourceConfig{Type: "rtsp"}, Inference: InferConfig{URL: "x"}}).validate(); err == nil {
		t.Fatal("rtsp 缺 url/decoder 应拒绝")
	}
	if err := (&Config{DeviceName: "c", Source: SourceConfig{Type: "rtsp", URL: "rtsp://cam/live"}, Inference: InferConfig{URL: "x"}}).validate(); err == nil {
		t.Fatal("rtsp 缺 decoder 应拒绝")
	}
	if err := (&Config{DeviceName: "c", Source: SourceConfig{Type: "rtsp", URL: "rtsp://cam/live", Decoder: "ffmpeg"}, Inference: InferConfig{URL: "x"}}).validate(); err != nil {
		t.Fatalf("合法 rtsp 应通过: %v", err)
	}
}

// TestV0420MapperRTSPE2E 覆盖 rtsp 源型全链：配置文件（type=rtsp）→
// LoadConfig → NewMapper（默认工厂）→ RTSP 模拟服务端拉流 → 解码脚本出帧
// → 真 HTTP 推理 stub → 指标断言；断流自愈（服务端中途强制断开）。
func TestV0420MapperRTSPE2E(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"detections":[{"label":"motion","score":0.7,"bbox":[1,1,4,4]}]}`))
	}))
	t.Cleanup(stub.Close)

	// RTSP 模拟服务端：50ms/包推流；8s 后强制断流（自愈验证）。
	sim, rtspAddr, err := rtsp.NewSimServer(rtsp.SimConfig{StreamGap: 50 * time.Millisecond, ResetAfterDur: 8 * time.Second})
	if err != nil {
		t.Fatalf("RTSP 模拟服务端启动失败: %v", err)
	}
	t.Cleanup(sim.Close)

	// 解码脚本：后台排空 stdin（`<&0` 显式 fd 复制——非交互 sh 下裸
	// `cat >/dev/null &` 不排空），前台按 ~2fps 输出 MJPEG 帧。
	dir := t.TempDir()
	f1 := filepath.Join(dir, "f1.jpg")
	f2 := filepath.Join(dir, "f2.jpg")
	if err := os.WriteFile(f1, jpegForTest(t, 40, 30), 0o644); err != nil {
		t.Fatalf("写 f1: %v", err)
	}
	if err := os.WriteFile(f2, jpegForTest(t, 48, 36), 0o644); err != nil {
		t.Fatalf("写 f2: %v", err)
	}
	decoder := filepath.Join(dir, "decoder.sh")
	decoderBody := fmt.Sprintf("#!/bin/sh\ncat <&0 >/dev/null &\ncat %s\nsleep 0.3\ncat %s\nsleep 0.5\nwhile true; do cat %s; sleep 0.5; done\n", f1, f2, f1)
	if err := os.WriteFile(decoder, []byte(decoderBody), 0o755); err != nil {
		t.Fatalf("写解码脚本: %v", err)
	}

	cfgPath := filepath.Join(dir, "video.json")
	cfgJSON := fmt.Sprintf(`{"deviceName":"cam-rtsp","source":{"type":"rtsp","url":"rtsp://%s/live","decoder":"/bin/sh %s","reconnectMs":50},"inference":{"url":%q,"timeoutMs":2000}}`,
		rtspAddr, decoder, stub.URL)
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
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
	if err := m.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })

	// waitFor：有界等待指标条件成立；超时显式失败（附当前指标）。
	waitFor := func(desc string, timeout time.Duration, cond func(map[string]float64) bool) map[string]float64 {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if props, _ := m.Collect(); cond(props) {
				return props
			}
			time.Sleep(150 * time.Millisecond)
		}
		props, _ := m.Collect()
		t.Fatalf("%s 超时未达成: %v", desc, props)
		return nil
	}

	// 阶段一：持续出帧 + 推理发生（帧数/推理数门槛——防管线早停假绿）。
	p1 := waitFor("出帧与推理（framesTotal≥6, inferTotal≥3, streamOn=1）", 25*time.Second, func(props map[string]float64) bool {
		return props["streamOn"] == 1 && props["framesTotal"] >= 6 && props["inferTotal"] >= 3
	})
	if p1["fps"] <= 0 {
		t.Fatalf("帧率应为正: %v", p1)
	}
	if p1["inferFailTotal"] != 0 {
		t.Fatalf("推理不应失败: %v", p1)
	}
	if _, ok := p1["reconnects"]; !ok {
		t.Fatal("reconnects 指标应经 Collect 暴露")
	}

	// 阶段二：断流自愈——8s 时服务端断开：源检测 → 重连（reconnects 增长）
	// → 帧流恢复（framesTotal 继续增长）。
	baseRec := p1["reconnects"]
	p2 := waitFor("断流后重连（reconnects 增长）", 30*time.Second, func(props map[string]float64) bool {
		return props["reconnects"] > baseRec
	})
	p3 := waitFor("重连后帧流恢复（framesTotal 继续增长）", 30*time.Second, func(props map[string]float64) bool {
		return props["framesTotal"] > p2["framesTotal"]+2
	})
	if p3["streamOn"] != 1 || p3["inferFailTotal"] != 0 {
		t.Fatalf("自愈后指标异常: %v", p3)
	}
	t.Logf("自愈验证: rec %.0f→%.0f, frames %.0f→%.0f, fps=%.2f",
		baseRec, p3["reconnects"], p1["framesTotal"], p3["framesTotal"], p3["fps"])
}
