// v0.34.0（specs/0007）实源测试：MJPEG over HTTP 直连源、外部进程桥源、
// JPEG 定界器与源工厂分发。零依赖（stdlib httptest/os/exec）。
package video

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// jpegFrame 生成 w x h 的 JPEG 测试帧字节。
func jpegFrame(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x += 4 {
		img.Set(x, 0, color.RGBA{R: uint8(x), G: 80, B: 160, A: 255})
	}
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 60}); err != nil {
		t.Fatalf("jpeg 编码: %v", err)
	}
	return buf.Bytes()
}

// mjpegServer 推送 MJPEG 流：frameCounts 为每次连接的帧数（按请求次序取；
// 耗尽后重复最后一组）。返回服务器与连接计数指针。
func mjpegServer(t *testing.T, w, h int, frameCounts []int) (*httptest.Server, *int) {
	t.Helper()
	conns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		idx := conns
		if idx >= len(frameCounts) {
			idx = len(frameCounts) - 1
		}
		conns++
		mw := multipart.NewWriter(rw)
		rw.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+mw.Boundary())
		rw.WriteHeader(http.StatusOK)
		fl := rw.(http.Flusher)
		fl.Flush()
		for i := 0; i < frameCounts[idx]; i++ {
			part, err := mw.CreatePart(map[string][]string{"Content-Type": {"image/jpeg"}})
			if err != nil {
				return
			}
			_, _ = part.Write(jpegFrame(t, w, h))
			fl.Flush()
		}
		_ = mw.Close()
	}))
	t.Cleanup(srv.Close)
	return srv, &conns
}

// TestV0340MJPEGSourceFrames 正常帧序：3 帧流 → 逐帧产出（宽高/JPEG 有效）。
func TestV0340MJPEGSourceFrames(t *testing.T) {
	srv, _ := mjpegServer(t, 48, 32, []int{3})
	src := NewMJPEGSource(MJPEGConfig{URL: srv.URL, ReconnectMs: 30})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 1; i <= 3; i++ {
		f, err := src.Next(ctx)
		if err != nil {
			t.Fatalf("帧 %d: %v", i, err)
		}
		if f.Seq != uint64(i) || f.Width != 48 || f.Height != 32 {
			t.Fatalf("帧 %d 元数据异常: seq=%d %dx%d", i, f.Seq, f.Width, f.Height)
		}
		if _, _, ok := jpegDim(f.JPEG); !ok {
			t.Fatalf("帧 %d JPEG 无效", i)
		}
	}
	if src.Reconnects() != 0 || src.BadFrames() != 0 {
		t.Fatalf("正常流不应重连/坏帧: rec=%d bad=%d", src.Reconnects(), src.BadFrames())
	}
}

// TestV0340MJPEGReconnect 断流重连：第一次连接 2 帧后断开 → 重连续拉 2 帧。
func TestV0340MJPEGReconnect(t *testing.T) {
	srv, conns := mjpegServer(t, 32, 24, []int{2, 2})
	src := NewMJPEGSource(MJPEGConfig{URL: srv.URL, ReconnectMs: 30})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 1; i <= 4; i++ {
		if _, err := src.Next(ctx); err != nil {
			t.Fatalf("帧 %d: %v", i, err)
		}
	}
	if *conns < 2 {
		t.Fatalf("应发生重连（连接数 %d）", *conns)
	}
	if src.Reconnects() < 1 {
		t.Fatalf("重连计数应为 ≥1: %d", src.Reconnects())
	}
}

// TestV0340MJPEGConfigError 配置性错误显式返回（不重试）：非 multipart
// 与 HTTP 非 200。
func TestV0340MJPEGConfigError(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(rw, "not a stream")
	}))
	t.Cleanup(plain.Close)
	src := NewMJPEGSource(MJPEGConfig{URL: plain.URL, ReconnectMs: 30})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := src.Next(ctx); err == nil || !bytes.Contains([]byte(err.Error()), []byte("multipart")) {
		t.Fatalf("非 multipart 应显式报错: %v", err)
	}

	notFound := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notFound.Close)
	src2 := NewMJPEGSource(MJPEGConfig{URL: notFound.URL, ReconnectMs: 30})
	if _, err := src2.Next(ctx); err == nil || !bytes.Contains([]byte(err.Error()), []byte("404")) {
		t.Fatalf("HTTP 404 应显式报错: %v", err)
	}
}

// TestV0340MJPEGCtxCancel ctx 取消即时退出（服务器挂起不发帧）。
func TestV0340MJPEGCtxCancel(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=xB")
		rw.WriteHeader(http.StatusOK)
		rw.(http.Flusher).Flush()
		<-hold // 挂起：不发任何 part。
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(hold) }) // LIFO：先释放挂起 handler，再关服务器
	src := NewMJPEGSource(MJPEGConfig{URL: srv.URL, ReconnectMs: 30})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := src.Next(ctx)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("ctx 应取消: err=%v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("取消响应过慢: %v", time.Since(start))
	}
}

// bridgeScript 写一个 shell 脚本并返回命令/参数。
func bridgeScript(t *testing.T, body string) (string, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("写脚本: %v", err)
	}
	return "/bin/sh", []string{path}
}

func writeBin(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写字节文件: %v", err)
	}
	return p
}

// TestV0340BridgeFrames 进程桥正常出帧（cat 两个 JPEG 文件）。
func TestV0340BridgeFrames(t *testing.T) {
	dir := t.TempDir()
	f1 := writeBin(t, dir, "f1.jpg", jpegFrame(t, 40, 30))
	f2 := writeBin(t, dir, "f2.jpg", jpegFrame(t, 40, 30))
	cmd, args := bridgeScript(t, "cat "+f1+" "+f2)
	src := NewBridgeSource(BridgeConfig{Command: cmd, Args: args, ReconnectMs: 30})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 1; i <= 2; i++ {
		f, err := src.Next(ctx)
		if err != nil {
			t.Fatalf("帧 %d: %v", i, err)
		}
		if f.Width != 40 || f.Height != 30 {
			t.Fatalf("帧 %d 尺寸异常: %dx%d", i, f.Width, f.Height)
		}
	}
	// 收尾：已取消的 ctx 驱动一次 Next（进程已 EOF → Wait 回收；在途则
	// watcher Kill 回收）。
	cancel()
	_, _ = src.Next(ctx)
}

// TestV0340BridgeCrossChunk 跨读块分帧：帧字节分两次输出（中段 sleep）。
func TestV0340BridgeCrossChunk(t *testing.T) {
	dir := t.TempDir()
	raw := jpegFrame(t, 36, 28)
	half := len(raw) / 2
	p1 := writeBin(t, dir, "p1.bin", raw[:half])
	p2 := writeBin(t, dir, "p2.bin", raw[half:])
	f2 := writeBin(t, dir, "f2.jpg", jpegFrame(t, 36, 28))
	cmd, args := bridgeScript(t, "cat "+p1+"; sleep 0.15; cat "+p2+"; sleep 0.15; cat "+f2)
	src := NewBridgeSource(BridgeConfig{Command: cmd, Args: args, ReconnectMs: 30})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fa, err := src.Next(ctx)
	if err != nil {
		t.Fatalf("帧 1: %v", err)
	}
	if !bytes.Equal(fa.JPEG, raw) {
		t.Fatalf("跨块拼接帧字节不一致: got %d want %d", len(fa.JPEG), len(raw))
	}
	if _, err := src.Next(ctx); err != nil {
		t.Fatalf("帧 2: %v", err)
	}
	if src.BadFrames() != 0 {
		t.Fatalf("不应有坏帧: %d", src.BadFrames())
	}
	cancel()
	_, _ = src.Next(ctx) // 收尾回收进程
}

// TestV0340BridgeCommandMissing 命令不存在：显式报错。
func TestV0340BridgeCommandMissing(t *testing.T) {
	src := NewBridgeSource(BridgeConfig{Command: "/nonexistent/edgeflow-cmd-xyz", ReconnectMs: 30})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := src.Next(ctx)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("启动失败")) {
		t.Fatalf("命令不存在应显式报错: %v", err)
	}
}

// TestV0340BridgeNonZeroExit 进程未产帧即退出：错误含退出状态与 stderr 尾部。
func TestV0340BridgeNonZeroExit(t *testing.T) {
	cmd, args := bridgeScript(t, "echo bridge-boom >&2; exit 3")
	src := NewBridgeSource(BridgeConfig{Command: cmd, Args: args, ReconnectMs: 30})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := src.Next(ctx)
	if err == nil {
		t.Fatal("未产帧退出应报错")
	}
	msg := err.Error()
	if !bytes.Contains([]byte(msg), []byte("未产出帧")) || !bytes.Contains([]byte(msg), []byte("bridge-boom")) {
		t.Fatalf("错误应含退出说明与 stderr 尾部: %s", msg)
	}
}

// TestV0340BridgeReconnect 进程推 1 帧后退出 → 自动重启续拉。
func TestV0340BridgeReconnect(t *testing.T) {
	dir := t.TempDir()
	f1 := writeBin(t, dir, "f1.jpg", jpegFrame(t, 24, 18))
	cmd, args := bridgeScript(t, "cat "+f1) // 每次进程只推 1 帧即退出
	src := NewBridgeSource(BridgeConfig{Command: cmd, Args: args, ReconnectMs: 30})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 1; i <= 2; i++ {
		if _, err := src.Next(ctx); err != nil {
			t.Fatalf("帧 %d: %v", i, err)
		}
	}
	if src.Reconnects() < 1 {
		t.Fatalf("应发生进程重启: %d", src.Reconnects())
	}
}

// TestV0340JPEGScannerSplit 定界器：单字节喂入 + 前缀垃圾 → 完整切帧。
func TestV0340JPEGScannerSplit(t *testing.T) {
	a := jpegFrame(t, 20, 16)
	b := jpegFrame(t, 28, 22)
	stream := append([]byte("JUNKJUNK"), a...)
	stream = append(stream, b...)
	var sc jpegScanner
	var got [][]byte
	for _, by := range stream {
		got = append(got, sc.feed([]byte{by})...)
	}
	if len(got) != 2 {
		t.Fatalf("应切出 2 帧: %d", len(got))
	}
	if !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) {
		t.Fatalf("切帧字节不一致: %d/%d vs %d/%d", len(got[0]), len(got[1]), len(a), len(b))
	}
}

// TestV0340NewSourceDispatch 源工厂分发与未知类型拒绝。
func TestV0340NewSourceDispatch(t *testing.T) {
	if src, err := NewSource(SourceConfig{Type: "synthetic"}); err != nil {
		t.Fatalf("synthetic: %v", err)
	} else if _, ok := src.(*SyntheticFrameSource); !ok {
		t.Fatalf("synthetic 类型错误: %T", src)
	}
	if _, err := NewSource(SourceConfig{Type: "mjpeg"}); err == nil {
		t.Fatal("mjpeg 缺 url 应拒绝")
	}
	if _, err := NewSource(SourceConfig{Type: "bridge"}); err == nil {
		t.Fatal("bridge 缺 command 应拒绝")
	}
	if src, err := NewSource(SourceConfig{Type: "mjpeg", URL: "http://x/mjpg"}); err != nil {
		t.Fatalf("mjpeg: %v", err)
	} else if _, ok := src.(*MJPEGSource); !ok {
		t.Fatalf("mjpeg 类型错误: %T", src)
	}
	if src, err := NewSource(SourceConfig{Type: "bridge", Command: "ffmpeg"}); err != nil {
		t.Fatalf("bridge: %v", err)
	} else if _, ok := src.(*BridgeSource); !ok {
		t.Fatalf("bridge 类型错误: %T", src)
	}
	if _, err := NewSource(SourceConfig{Type: "rtsp"}); err == nil {
		t.Fatal("rtsp 仍应拒绝（原生协议栈未实现）")
	}
}

// TestV0340BridgeCloseReaps 复现/锚定 v0.34.0 复核 P1 修复：
// 出帧循环可在「帧交付后直接退出」路径终止而不再调用 Next——Close 必须
// Kill 并 Wait 回收进程（不得等待孙进程/写端自然结束），且幂等、释放后
// 可重建新进程。
func TestV0340BridgeCloseReaps(t *testing.T) {
	dir := t.TempDir()
	f1 := writeBin(t, dir, "f1.jpg", jpegFrame(t, 32, 24))
	cmd, args := bridgeScript(t, "cat "+f1+"; sleep 30")
	src := NewBridgeSource(BridgeConfig{Command: cmd, Args: args, ReconnectMs: 30})
	if _, err := src.Next(context.Background()); err != nil {
		t.Fatalf("首帧: %v", err)
	}
	// 交付间隙退出：不再 Next，直接 Close——若未 Kill+Wait 将等 sleep 30 自然结束。
	start := time.Now()
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close 未及时回收（%v，疑似未 Kill 或等待孙进程写端）", d)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("二次 Close（幂等）: %v", err)
	}
	// 释放后可重建：句柄已清理，Next 应启动新进程并成功出帧。
	f2, err := src.Next(context.Background())
	if err != nil || f2 == nil || f2.Width != 32 || f2.Height != 24 {
		t.Fatalf("Close 后重建失败: frame=%+v err=%v", f2, err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("收尾 Close: %v", err)
	}
}

// TestV0340MJPEGBadURLRejected 锚定 v0.34.0 复核 P2-3：scheme/host 类
// 永久性错误在构造期显式拒绝（不进入运行期无限重连）。
func TestV0340MJPEGBadURLRejected(t *testing.T) {
	for _, bad := range []string{"ftp://x/y", "://bad", "http://", "/no/scheme", "ws://host/p"} {
		if _, err := NewSource(SourceConfig{Type: "mjpeg", URL: bad}); err == nil {
			t.Fatalf("非法 URL %q 未被拒绝", bad)
		}
	}
	if _, err := NewSource(SourceConfig{Type: "mjpeg", URL: "https://cam.local:8443/v.mjpg"}); err != nil {
		t.Fatalf("合法 URL 被误拒: %v", err)
	}
}
