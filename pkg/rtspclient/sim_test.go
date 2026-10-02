// 模拟 RTSP 服务端的测试壳（服务端本体在 sim.go——导出面供 mapper 级测试
// 与 e2e 复用）。
package rtspclient

import (
	"testing"
	"time"
)

// startSimServer 测试壳：启动模拟服务端并注册清理。
func startSimServer(t *testing.T, cfg SimConfig) string {
	t.Helper()
	s, addr, err := NewSimServer(cfg)
	if err != nil {
		t.Fatalf("模拟服务端启动失败: %v", err)
	}
	t.Cleanup(s.Close)
	return addr
}

// 占位引用（保持测试内构造态一致）。
var _ = time.Second
