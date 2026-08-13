package version

import (
	"strings"
	"testing"
)

// TestGet 验证 Get 能取到通过 -ldflags 注入的值。
func TestGet(t *testing.T) {
	// 测试中临时修改全局变量，结束后恢复
	oldVersion, oldCommit, oldBuild := Version, GitCommit, BuildTime
	defer func() {
		Version, GitCommit, BuildTime = oldVersion, oldCommit, oldBuild
	}()

	Version = "v0.1.0"
	GitCommit = "abc123"
	BuildTime = "2026-08-13T09:00:00+08:00"

	info := Get()
	if info.Version != "v0.1.0" {
		t.Errorf("Version = %q, 期望 %q", info.Version, "v0.1.0")
	}
	if info.GitCommit != "abc123" {
		t.Errorf("GitCommit = %q, 期望 %q", info.GitCommit, "abc123")
	}
	if info.BuildTime != "2026-08-13T09:00:00+08:00" {
		t.Errorf("BuildTime = %q, 期望 %q", info.BuildTime, "2026-08-13T09:00:00+08:00")
	}
	if info.GoVersion == "" {
		t.Error("GoVersion 不应为空")
	}
}

// TestGetDefaults 验证未注入时默认值不为空（保证 --version 始终能输出信息）。
func TestGetDefaults(t *testing.T) {
	info := Get()
	if info.Version == "" {
		t.Error("Version 默认值不应为空")
	}
	if info.GitCommit == "" {
		t.Error("GitCommit 默认值不应为空")
	}
	if info.BuildTime == "" {
		t.Error("BuildTime 默认值不应为空")
	}
}

// TestString 验证 String 输出包含全部字段。
func TestString(t *testing.T) {
	oldVersion, oldCommit, oldBuild := Version, GitCommit, BuildTime
	defer func() {
		Version, GitCommit, BuildTime = oldVersion, oldCommit, oldBuild
	}()

	Version = "v0.1.0"
	GitCommit = "abc123"
	BuildTime = "2026-08-13T09:00:00+08:00"

	s := Get().String()
	for _, want := range []string{"version=v0.1.0", "gitCommit=abc123", "buildTime=2026-08-13T09:00:00+08:00"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() 输出 %q 缺少 %q", s, want)
		}
	}
}
