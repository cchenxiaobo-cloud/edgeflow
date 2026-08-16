package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile 在临时目录写入配置文件并返回其路径。
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudcore.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入临时配置文件失败: %v", err)
	}
	return path
}

// missingFile 返回一个必然不存在的配置文件路径。
func missingFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "not-exist.json")
}

// TestLoadDefault 验证无 flag/env/配置文件时使用默认值 8080。
func TestLoadDefault(t *testing.T) {
	t.Setenv(PortEnvVar, "")

	// 配置文件不存在 + 无 flag/env → 默认值 8080，来源为默认值，且不报错
	cfg, err := Load(missingFile(t), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("端口 = %d，期望默认值 %d", cfg.Port, DefaultPort)
	}
	if cfg.PortSource != SourceDefault {
		t.Errorf("来源 = %q，期望 %q", cfg.PortSource, SourceDefault)
	}

	// 空路径应回落到默认路径（默认路径文件不存在 → 同样使用默认值）
	cfg, err = Load("", 0, false)
	if err != nil {
		t.Fatalf("Load(\"\") 不应报错: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("空路径时端口 = %d，期望默认值 %d", cfg.Port, DefaultPort)
	}
}

// TestLoadConfigFile 验证配置文件中的端口被正确读取。
func TestLoadConfigFile(t *testing.T) {
	t.Setenv(PortEnvVar, "")

	cfg, err := Load(writeFile(t, `{"port": 9090}`), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("端口 = %d，期望 9090", cfg.Port)
	}
	if cfg.PortSource != SourceFile {
		t.Errorf("来源 = %q，期望 %q", cfg.PortSource, SourceFile)
	}
}

// TestLoadEnvOverride 验证环境变量覆盖配置文件与默认值。
func TestLoadEnvOverride(t *testing.T) {
	t.Setenv(PortEnvVar, "10000")

	// 无配置文件：环境变量直接生效
	cfg, err := Load(missingFile(t), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.Port != 10000 {
		t.Errorf("端口 = %d，期望 10000", cfg.Port)
	}
	if cfg.PortSource != SourceEnv {
		t.Errorf("来源 = %q，期望 %q", cfg.PortSource, SourceEnv)
	}

	// 环境变量优先级高于配置文件
	cfg, err = Load(writeFile(t, `{"port": 9090}`), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.Port != 10000 {
		t.Errorf("端口 = %d，期望环境变量值 10000", cfg.Port)
	}
}

// TestLoadFlagOverride 验证 --port 显式指定时优先级最高。
func TestLoadFlagOverride(t *testing.T) {
	t.Setenv(PortEnvVar, "10000")

	// --port 显式指定：环境变量与配置文件均被忽略
	cfg, err := Load(writeFile(t, `{"port": 9090}`), 7070, true)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.Port != 7070 {
		t.Errorf("端口 = %d，期望 --port 值 7070", cfg.Port)
	}
	if cfg.PortSource != SourceFlag {
		t.Errorf("来源 = %q，期望 %q", cfg.PortSource, SourceFlag)
	}
}

// TestLoadFlagIgnoresInvalidEnv 验证显式 --port 时不再读取环境变量，
// 即使环境变量非法也不报错（优先级语义）。
func TestLoadFlagIgnoresInvalidEnv(t *testing.T) {
	t.Setenv(PortEnvVar, "not-a-port")

	cfg, err := Load(missingFile(t), 7070, true)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.Port != 7070 {
		t.Errorf("端口 = %d，期望 7070", cfg.Port)
	}
}

// TestLoadInvalidFlagPort 验证 --port 非法值（范围外）直接报错。
func TestLoadInvalidFlagPort(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	for _, tc := range []struct {
		name string
		port int
	}{
		{"零", 0},
		{"越界上限", 70000},
		{"越界+1", 65536},
		{"负数", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(missingFile(t), tc.port, true)
			if err == nil {
				t.Fatalf("--port %d 应报错", tc.port)
			}
			if !strings.Contains(err.Error(), "1-65535") {
				t.Errorf("错误信息应说明合法范围: %v", err)
			}
		})
	}
}

// TestLoadInvalidEnvPort 验证环境变量非法值（非数字或范围外）直接报错。
func TestLoadInvalidEnvPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
	}{
		{"非数字", "notaport"},
		{"越界上限", "70000"},
		{"零", "0"},
		{"负数", "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(PortEnvVar, tc.env)
			_, err := Load(missingFile(t), 0, false)
			if err == nil {
				t.Fatalf("环境变量 %q 应报错", tc.env)
			}
		})
	}
}

// TestLoadFileParseError 验证配置文件存在但解析失败时返回错误。
func TestLoadFileParseError(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"残缺 JSON", `{"port": 9090`},
		{"端口为字符串", `{"port": "8080"}`},
		{"端口为小数", `{"port": 8080.5}`},
		{"纯文本", `port=8080`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeFile(t, tc.content), 0, false)
			if err == nil {
				t.Fatalf("文件内容 %q 应解析失败", tc.content)
			}
			if !strings.Contains(err.Error(), "解析配置文件") {
				t.Errorf("错误信息应指向解析失败: %v", err)
			}
		})
	}
}

// TestLoadFileInvalidPort 验证配置文件存在但端口非法时返回错误。
func TestLoadFileInvalidPort(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"端口越界", `{"port": 70000}`},
		{"端口为零", `{"port": 0}`},
		{"未声明端口", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeFile(t, tc.content), 0, false)
			if err == nil {
				t.Fatalf("文件 %s 应报错", tc.content)
			}
		})
	}
}

// TestLoadFileUnreadable 验证文件存在但不可读取（如路径指向目录）时返回错误。
func TestLoadFileUnreadable(t *testing.T) {
	t.Setenv(PortEnvVar, "")

	_, err := Load(t.TempDir(), 0, false)
	if err == nil {
		t.Fatal("目录路径应报错")
	}
	if !strings.Contains(err.Error(), "读取配置文件") {
		t.Errorf("错误信息应指向读取失败: %v", err)
	}
}

// ---- WBS 2.7：CloudHub 端口（hubPort）解析与热重载入口 ----

// TestLoadHubPortDefault 验证未配置 hubPort 时使用默认值 10000（来源=默认值）。
func TestLoadHubPortDefault(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	t.Setenv(HubPortEnvVar, "")

	cfg, err := Load(writeFile(t, `{"port": 8080}`), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.HubPort != DefaultHubPort {
		t.Errorf("HubPort = %d，期望默认值 %d", cfg.HubPort, DefaultHubPort)
	}
	if cfg.HubPortSource != SourceDefault {
		t.Errorf("HubPort 来源 = %q，期望 %q", cfg.HubPortSource, SourceDefault)
	}
}

// TestLoadHubPortFromFile 验证配置文件中的 hubPort 被正确读取。
func TestLoadHubPortFromFile(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	t.Setenv(HubPortEnvVar, "")

	cfg, err := Load(writeFile(t, `{"port": 8080, "hubPort": 10001}`), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.HubPort != 10001 {
		t.Errorf("HubPort = %d，期望 10001", cfg.HubPort)
	}
	if cfg.HubPortSource != SourceFile {
		t.Errorf("HubPort 来源 = %q，期望 %q", cfg.HubPortSource, SourceFile)
	}
}

// TestLoadHubPortFileZero 验证 hubPort 显式 0 = 随机端口（与 env 语义一致，测试用）。
func TestLoadHubPortFileZero(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	t.Setenv(HubPortEnvVar, "")

	cfg, err := Load(writeFile(t, `{"port": 8080, "hubPort": 0}`), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.HubPort != 0 {
		t.Errorf("HubPort = %d，期望 0（随机端口）", cfg.HubPort)
	}
}

// TestLoadHubPortEnvOverride 验证环境变量 EDGEFLOW_CLOUDCORE_HUB_PORT
// 覆盖配置文件与默认值。
func TestLoadHubPortEnvOverride(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	t.Setenv(HubPortEnvVar, "10002")

	cfg, err := Load(writeFile(t, `{"port": 8080, "hubPort": 10001}`), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.HubPort != 10002 {
		t.Errorf("HubPort = %d，期望 env 值 10002", cfg.HubPort)
	}
	if cfg.HubPortSource != SourceHubEnv {
		t.Errorf("HubPort 来源 = %q，期望 %q", cfg.HubPortSource, SourceHubEnv)
	}
}

// TestLoadHubPortFlagRetainsEnv 验证 --port 显式指定时 hubPort 仍由 env 解析
// （与旧行为一致：--port 只锁定 HTTP 端口，CloudHub 端口独立配置）。
func TestLoadHubPortFlagRetainsEnv(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	t.Setenv(HubPortEnvVar, "10003")

	cfg, err := Load(missingFile(t), 7070, true)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.HubPort != 10003 {
		t.Errorf("HubPort = %d，期望 env 值 10003", cfg.HubPort)
	}
}

// TestLoadHubPortEnvInvalid 验证 hubPort 环境变量非法时返回错误（fail-fast）。
func TestLoadHubPortEnvInvalid(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	for _, tc := range []struct {
		name string
		env  string
	}{
		{"非数字", "notaport"},
		{"越界上限", "70000"},
		{"负数", "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(HubPortEnvVar, tc.env)
			_, err := Load(missingFile(t), 0, false)
			if err == nil {
				t.Fatalf("环境变量 %q 应报错", tc.env)
			}
			if !strings.Contains(err.Error(), HubPortEnvVar) {
				t.Errorf("错误信息应包含环境变量名 %s: %v", HubPortEnvVar, err)
			}
		})
	}
}

// TestLoadHubPortFileInvalid 验证配置文件 hubPort 越界时返回错误。
func TestLoadHubPortFileInvalid(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	t.Setenv(HubPortEnvVar, "")
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"越界上限", `{"port": 8080, "hubPort": 70000}`},
		{"负数", `{"port": 8080, "hubPort": -1}`},
		{"类型错误", `{"port": 8080, "hubPort": "10000"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeFile(t, tc.content), 0, false)
			if err == nil {
				t.Fatalf("文件 %s 应报错", tc.content)
			}
		})
	}
}

// TestLoadReloadMissingFile 验证重载语义：配置文件不存在时返回错误
// （保持旧配置），与启动语义（静默回落默认值）区分。
func TestLoadReloadMissingFile(t *testing.T) {
	t.Setenv(PortEnvVar, "")
	t.Setenv(HubPortEnvVar, "")

	_, err := LoadReload(missingFile(t), 0, false)
	if err == nil {
		t.Fatal("LoadReload(文件不存在) 应报错")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误信息应说明文件缺失: %v", err)
	}

	// 启动语义不受影响：Load 文件不存在仍静默回落默认值
	if _, err := Load(missingFile(t), 0, false); err != nil {
		t.Fatalf("Load(文件不存在) 不应报错: %v", err)
	}
}

// TestLoadReloadEnvPriority 验证重载入口同样完整执行优先级链（env 覆盖保留）。
func TestLoadReloadEnvPriority(t *testing.T) {
	t.Setenv(PortEnvVar, "10010")
	t.Setenv(HubPortEnvVar, "")

	cfg, err := LoadReload(writeFile(t, `{"port": 9090}`), 0, false)
	if err != nil {
		t.Fatalf("LoadReload 不应报错: %v", err)
	}
	if cfg.Port != 10010 || cfg.PortSource != SourceEnv {
		t.Errorf("重载时 env 覆盖应保留: port=%d source=%q", cfg.Port, cfg.PortSource)
	}
}

// TestLoadCompressDefault 验证压缩开关缺省为开启（true）：
// 无配置文件、文件未声明 compress 字段两种情况下默认值一致。
func TestLoadCompressDefault(t *testing.T) {
	t.Setenv(PortEnvVar, "")

	// 无配置文件 → 默认开启
	cfg, err := Load(missingFile(t), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if !cfg.Compress {
		t.Errorf("无配置文件时 Compress 应默认 true，实际 %v", cfg.Compress)
	}

	// 文件存在但未声明 compress → 默认开启（指针 nil 回落默认值）
	cfg, err = Load(writeFile(t, `{"port": 8080}`), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if !cfg.Compress {
		t.Errorf("未声明 compress 时 Compress 应默认 true，实际 %v", cfg.Compress)
	}
}

// TestLoadCompressFromFile 验证配置文件显式声明 compress 被正确读取。
func TestLoadCompressFromFile(t *testing.T) {
	t.Setenv(PortEnvVar, "")

	// 显式 true
	cfg, err := Load(writeFile(t, `{"port": 8080, "compress": true}`), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if !cfg.Compress {
		t.Errorf("compress=true 应生效，实际 %v", cfg.Compress)
	}

	// 显式 false（关闭压缩）
	cfg, err = Load(writeFile(t, `{"port": 8080, "compress": false}`), 0, false)
	if err != nil {
		t.Fatalf("Load 不应报错: %v", err)
	}
	if cfg.Compress {
		t.Errorf("compress=false 应生效，实际 %v", cfg.Compress)
	}
}

// TestLoadReloadCompress 验证重载入口同样解析 compress（与 Load 语义一致）。
func TestLoadReloadCompress(t *testing.T) {
	t.Setenv(PortEnvVar, "")

	cfg, err := LoadReload(writeFile(t, `{"port": 9090, "compress": false}`), 0, false)
	if err != nil {
		t.Fatalf("LoadReload 不应报错: %v", err)
	}
	if cfg.Compress {
		t.Errorf("重载时 compress=false 应生效，实际 %v", cfg.Compress)
	}
}
