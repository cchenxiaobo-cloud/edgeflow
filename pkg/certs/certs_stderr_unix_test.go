//go:build !windows

package certs

import (
	"bytes"
	"io"
	"os"
	"syscall"
	"testing"
)

// captureStderrFd 把 fd 2（stderr）重定向到内存 buffer 后执行 fn，
// 返回 fn 期间写入 stderr 的输出。pkg/log 的 logger 在包初始化时
// 已持有 os.Stderr（fd 2 的包装），重定向 fd 2 本身即可截获其输出。
// 本包测试无 t.Parallel（测试串行执行），重定向期间无并发写 stderr。
// （v0.11.0，L20b+：Unix 专用实现——Windows 见 certs_stderr_windows_test.go）
func captureStderrFd(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	saved, err := syscall.Dup(2)
	if err != nil {
		t.Fatalf("备份 fd 2 失败: %v", err)
	}
	if err := syscall.Dup2(int(w.Fd()), 2); err != nil {
		_ = syscall.Close(saved)
		t.Fatalf("重定向 fd 2 失败: %v", err)
	}
	fn()
	// 恢复 fd 2（先于读取，避免管道写端未关闭导致读取阻塞）
	_ = syscall.Dup2(saved, 2)
	_ = syscall.Close(saved)
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}
