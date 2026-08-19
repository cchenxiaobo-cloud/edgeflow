// edgecore 配置解析测试（WBS 2.7）：文件/env/默认值优先级与校验。
package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"edgeflow/pkg/log"
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

// TestEdgeCoreIntervalFromEnvLogging 验证周期配置启动日志明示生效值
// （KNOWN-ISSUES §1 ④ 修复）：合法 env 输出 Info（来源+请求值+生效值）、
// 越界 env 输出 Warn（请求值+合法区间+回落值）并回落默认，返回值两态
// 与解析语义一致；未设置时输出 Info 并保持当前值。
// 日志用 bytes.Buffer 捕获（log.SetOutput 注入），不依赖真实输出。
func TestEdgeCoreIntervalFromEnvLogging(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(nil) // 恢复 stderr（与 pkg/log 既有测试同约定）

	key := EdgeCoreReportIntervalEnv
	const def = 30 * time.Second

	// 合法值：返回解析值，Info 明示来源/请求值/生效值
	clearEdgeCoreEnv(t)
	t.Setenv(key, "2m")
	if got := edgeCoreIntervalFromEnv(key, def); got != 2*time.Minute {
		t.Errorf("合法 env 返回值 = %v，期望 2m", got)
	}

	// 越界值（300ms < 1s）：回落默认，Warn 说明请求值与合法区间
	t.Setenv(key, "300ms")
	if got := edgeCoreIntervalFromEnv(key, def); got != def {
		t.Errorf("越界 env 返回值 = %v，期望回落 %v", got, def)
	}

	// 未设置：保持默认，Info 明示来源
	clearEdgeCoreEnv(t)
	if got := edgeCoreIntervalFromEnv(key, def); got != def {
		t.Errorf("未设置 env 返回值 = %v，期望 %v", got, def)
	}

	out := buf.String()
	for _, want := range []string{
		key,                 // 日志含环境变量名
		"生效值 2m0s",          // 合法态：生效值
		"请求值 2m",            // 合法态：请求值
		"来源：环境变量",           // 合法态：配置来源
		"WARN",              // 越界态：警告级别
		"请求 300ms",          // 越界态：请求值
		"超出合法区间 [1s,10m0s]", // 越界态：合法区间
		"回落 30s",            // 越界态：回落值
		"来源：默认/配置文件",        // 未设置态：配置来源
	} {
		if !strings.Contains(out, want) {
			t.Errorf("日志缺少 %q：\n%s", want, out)
		}
	}
}
