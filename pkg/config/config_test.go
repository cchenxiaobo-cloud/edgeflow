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
