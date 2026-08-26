//go:build windows

package certs

import (
	"bytes"
	"testing"

	"edgeflow/pkg/log"
)

// captureStderrFd Windows 实现：经 pkg/log.SetOutput 捕获 logger 输出
// （Windows 无 fd 级 dup 语义）。断言内容（WARN/降级/无锁/限频）与
// Unix 版一致；包内测试无 t.Parallel（串行），全局注入安全。
func captureStderrFd(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Output()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}
