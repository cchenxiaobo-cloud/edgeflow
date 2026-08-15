// edgecore 配置解析测试（WBS 2.7）：文件/env/默认值优先级与校验。
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearEdgeCoreEnv 清空全部 edgecore 相关环境变量，隔离测试。
func clearEdgeCoreEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		EdgeCoreCloudAddrEnv, EdgeCoreNodeIDEnv,
		EdgeCoreReportIntervalEnv, EdgeCoreDeviceReportIntervalEnv,
		EdgeCoreReconcileIntervalEnv,
	} {
		t.Setenv(k, "")
	}
}

// writeEdgeCoreFile 在临时目录写入 edgecore 配置文件并返回其路径。
func writeEdgeCoreFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "edgecore.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入临时配置文件失败: %v", err)
	}
	return path
}

// TestLoadEdgeCoreDefaults 验证无文件/env 时使用全部默认值。
func TestLoadEdgeCoreDefaults(t *testing.T) {
	clearEdgeCoreEnv(t)

	cfg, err := LoadEdgeCore(missingFile(t))
	if err != nil {
		t.Fatalf("LoadEdgeCore 不应报错: %v", err)
	}
	if cfg.CloudAddr != EdgeCoreDefaultCloudAddr {
		t.Errorf("CloudAddr = %q，期望 %q", cfg.CloudAddr, EdgeCoreDefaultCloudAddr)
	}
	if cfg.NodeID == "" || !strings.HasPrefix(cfg.NodeID, "edge-") {
		t.Errorf("NodeID 应回落为 edge-<主机名> 形式，实际 %q", cfg.NodeID)
	}
	if cfg.PodReportInterval != EdgeCoreDefaultReportInterval {
		t.Errorf("PodReportInterval = %v，期望 %v", cfg.PodReportInterval, EdgeCoreDefaultReportInterval)
	}
	if cfg.DeviceReportInterval != EdgeCoreDefaultReportInterval {
		t.Errorf("DeviceReportInterval = %v，期望 %v", cfg.DeviceReportInterval, EdgeCoreDefaultReportInterval)
	}
	if cfg.ReconcileInterval != EdgeCoreDefaultReconcileInterval {
		t.Errorf("ReconcileInterval = %v，期望 %v", cfg.ReconcileInterval, EdgeCoreDefaultReconcileInterval)
	}
}

// TestLoadEdgeCoreNodeIDEnv 验证 nodeID 环境变量优先（默认值链：env > 主机名）。
func TestLoadEdgeCoreNodeIDEnv(t *testing.T) {
	clearEdgeCoreEnv(t)
	t.Setenv(EdgeCoreNodeIDEnv, "edge-test-1")

	cfg, err := LoadEdgeCore(missingFile(t))
	if err != nil {
		t.Fatalf("LoadEdgeCore 不应报错: %v", err)
	}
	if cfg.NodeID != "edge-test-1" {
		t.Errorf("NodeID = %q，期望 env 值 edge-test-1", cfg.NodeID)
	}
}

// TestLoadEdgeCoreFile 验证配置文件全部字段生效。
func TestLoadEdgeCoreFile(t *testing.T) {
	clearEdgeCoreEnv(t)

	cfg, err := LoadEdgeCore(writeEdgeCoreFile(t, `{
		"cloudAddr": "ws://10.0.0.1:10000",
		"nodeID": "edge-file-1",
		"podReportInterval": "15s",
		"deviceReportInterval": "45s",
		"reconcileInterval": "3s"
	}`))
	if err != nil {
		t.Fatalf("LoadEdgeCore 不应报错: %v", err)
	}
	if cfg.CloudAddr != "ws://10.0.0.1:10000" {
		t.Errorf("CloudAddr = %q", cfg.CloudAddr)
	}
	if cfg.NodeID != "edge-file-1" {
		t.Errorf("NodeID = %q", cfg.NodeID)
	}
	if cfg.PodReportInterval != 15*time.Second {
		t.Errorf("PodReportInterval = %v，期望 15s", cfg.PodReportInterval)
	}
	if cfg.DeviceReportInterval != 45*time.Second {
		t.Errorf("DeviceReportInterval = %v，期望 45s", cfg.DeviceReportInterval)
	}
	if cfg.ReconcileInterval != 3*time.Second {
		t.Errorf("ReconcileInterval = %v，期望 3s", cfg.ReconcileInterval)
	}
}

// TestLoadEdgeCorePartialFile 验证部分字段的文件：缺省字段回落默认值。
func TestLoadEdgeCorePartialFile(t *testing.T) {
	clearEdgeCoreEnv(t)

	cfg, err := LoadEdgeCore(writeEdgeCoreFile(t, `{"cloudAddr": "ws://10.0.0.2:10000"}`))
	if err != nil {
		t.Fatalf("LoadEdgeCore 不应报错: %v", err)
	}
	if cfg.CloudAddr != "ws://10.0.0.2:10000" {
		t.Errorf("CloudAddr = %q", cfg.CloudAddr)
	}
	if cfg.PodReportInterval != EdgeCoreDefaultReportInterval {
		t.Errorf("缺省字段应回落默认值: %v", cfg.PodReportInterval)
	}
	if cfg.NodeID == "" || !strings.HasPrefix(cfg.NodeID, "edge-") {
		t.Errorf("缺省 nodeID 应回落 edge-<主机名>: %q", cfg.NodeID)
	}
}

// TestLoadEdgeCoreEnvOverride 验证环境变量覆盖配置文件（env > file）。
func TestLoadEdgeCoreEnvOverride(t *testing.T) {
	clearEdgeCoreEnv(t)
	t.Setenv(EdgeCoreCloudAddrEnv, "ws://10.0.0.9:10000")
	t.Setenv(EdgeCoreNodeIDEnv, "edge-env-1")
	t.Setenv(EdgeCoreReportIntervalEnv, "7s")

	cfg, err := LoadEdgeCore(writeEdgeCoreFile(t, `{
		"cloudAddr": "ws://10.0.0.1:10000",
		"nodeID": "edge-file-1",
		"podReportInterval": "15s"
	}`))
	if err != nil {
		t.Fatalf("LoadEdgeCore 不应报错: %v", err)
	}
	if cfg.CloudAddr != "ws://10.0.0.9:10000" {
		t.Errorf("CloudAddr = %q，期望 env 覆盖", cfg.CloudAddr)
	}
	if cfg.NodeID != "edge-env-1" {
		t.Errorf("NodeID = %q，期望 env 覆盖", cfg.NodeID)
	}
	if cfg.PodReportInterval != 7*time.Second {
		t.Errorf("PodReportInterval = %v，期望 env 值 7s", cfg.PodReportInterval)
	}
	// 未覆盖的字段仍来自文件
	if cfg.DeviceReportInterval != EdgeCoreDefaultReportInterval {
		t.Errorf("DeviceReportInterval = %v，期望默认值", cfg.DeviceReportInterval)
	}
}

// TestLoadEdgeCoreEnvInvalidInterval 验证非法环境变量回落当前值并告警（不报错）。
func TestLoadEdgeCoreEnvInvalidInterval(t *testing.T) {
	clearEdgeCoreEnv(t)
	t.Setenv(EdgeCoreReportIntervalEnv, "bogus")

	cfg, err := LoadEdgeCore(writeEdgeCoreFile(t, `{"podReportInterval": "20s"}`))
	if err != nil {
		t.Fatalf("LoadEdgeCore 不应报错: %v", err)
	}
	if cfg.PodReportInterval != 20*time.Second {
		t.Errorf("非法 env 应回落文件值 20s，实际 %v", cfg.PodReportInterval)
	}
}

// TestLoadEdgeCoreFileInvalid 验证配置文件非法字段返回错误（fail-fast）。
func TestLoadEdgeCoreFileInvalid(t *testing.T) {
	clearEdgeCoreEnv(t)
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"残缺 JSON", `{"cloudAddr": "ws://x`},
		{"周期格式非法", `{"podReportInterval": "30"}`},
		{"周期越界下限", `{"podReportInterval": "500ms"}`},
		{"周期越界上限", `{"deviceReportInterval": "1h"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadEdgeCore(writeEdgeCoreFile(t, tc.content))
			if err == nil {
				t.Fatalf("文件 %s 应报错", tc.content)
			}
		})
	}
}

// TestLoadEdgeCoreReloadMissingFile 验证重载语义：文件缺失返回错误（保持旧配置）。
func TestLoadEdgeCoreReloadMissingFile(t *testing.T) {
	clearEdgeCoreEnv(t)

	_, err := LoadEdgeCoreReload(missingFile(t))
	if err == nil {
		t.Fatal("LoadEdgeCoreReload(文件不存在) 应报错")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误信息应说明文件缺失: %v", err)
	}
}

// TestLoadEdgeCoreReadError 验证文件不可读（路径指向目录）时返回错误。
func TestLoadEdgeCoreReadError(t *testing.T) {
	clearEdgeCoreEnv(t)

	_, err := LoadEdgeCore(t.TempDir())
	if err == nil {
		t.Fatal("目录路径应报错")
	}
	if !strings.Contains(err.Error(), "读取配置文件") {
		t.Errorf("错误信息应指向读取失败: %v", err)
	}
}
