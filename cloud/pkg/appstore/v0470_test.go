package appstore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestV0470AppStoreCRUD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apps.json")
	s := New(path)
	if err := s.Load(); err != nil {
		t.Fatalf("Load 空文件: %v", err)
	}
	// Create + 首版本
	a, err := s.Create("My-App", "演示应用", "desc", KindContainer, 1700000000000,
		&Version{Version: "v1.0.0", Image: "registry.io/team/my-app:v1.0.0"})
	if err != nil || a == nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Name != "my-app" {
		t.Fatalf("name 规范化 = %q", a.Name)
	}
	// 重复 409 语义
	if _, err := s.Create("my-app", "", "", KindContainer, 0, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("重复注册 = %v, want ErrExists", err)
	}
	// 非法名/非法 kind
	if _, err := s.Create("Bad_Name", "", "", KindContainer, 0, nil); err == nil {
		t.Fatal("非法名应报错")
	}
	if _, err := s.Create("app2", "", "", "helm", 0, nil); err == nil {
		t.Fatal("非法 kind 应报错")
	}
	// 版本二选一校验
	if _, err := s.AddVersion("my-app", Version{Version: "v1.1.0"}); err == nil {
		t.Fatal("container 版本缺 image 应报错")
	}
	if _, err := s.AddVersion("my-app", Version{Version: "v1.1.0", Image: "reg.io/x:v1.1.0"}); err != nil {
		t.Fatalf("AddVersion: %v", err)
	}
	if _, err := s.AddVersion("my-app", Version{Version: "v1.1.0", Image: "reg.io/x:v1.1.0"}); !errors.Is(err, ErrExists) {
		t.Fatal("重复版本应 ErrExists")
	}
	// script 应用二选一反向
	if _, err := s.Create("scr-app", "", "", KindScript, 0, &Version{Version: "v1", Image: "reg.io/x"}); err == nil {
		t.Fatal("script 版本带 image 应报错")
	}
	if _, err := s.Create("scr-app", "", "", KindScript, 0, &Version{Version: "v1", ScriptRef: "scr-pkg"}); err != nil {
		t.Fatalf("script 合法创建: %v", err)
	}
	// Get/List 副本语义
	got, ok := s.Get("MY-APP")
	if !ok || len(got.Versions) != 2 {
		t.Fatalf("Get 大小写不敏感失败: %+v", got)
	}
	got.Versions = nil
	again, _ := s.Get("my-app")
	if len(again.Versions) != 2 {
		t.Fatal("Get 返回副本（修改不影响存储）")
	}
	if len(s.List()) != 2 {
		t.Fatalf("List = %d", len(s.List()))
	}
	// MarkPkg
	if err := s.MarkPkg("my-app", "v1.0.0", true); err != nil {
		t.Fatalf("MarkPkg: %v", err)
	}
	v, _ := s.GetVersion("my-app", "v1.0.0")
	if !v.HasPkg {
		t.Fatal("MarkPkg 未生效")
	}
	if err := s.MarkPkg("my-app", "nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatal("未知版本 MarkPkg 应 ErrNotFound")
	}
	// 持久化重载
	s2 := New(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("重载: %v", err)
	}
	if s2.Count() != 2 {
		t.Fatalf("重载应用数 = %d", s2.Count())
	}
	if v2, ok := s2.GetVersion("my-app", "v1.0.0"); !ok || !v2.HasPkg {
		t.Fatal("重载后 HasPkg 丢失")
	}
	// Delete
	if err := s.Delete("my-app"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete("my-app"); !errors.Is(err, ErrNotFound) {
		t.Fatal("重复删除应 ErrNotFound")
	}
}

func TestV0470NameValidation(t *testing.T) {
	for _, ok := range []string{"a", "my-app", "app-1", "0app"} {
		if !ValidName(ok) {
			t.Fatalf("合法名误判: %q", ok)
		}
	}
	for _, bad := range []string{"", "-x", "x-", "Bad", "a_b", strings.Repeat("a", 64)} {
		if ValidName(bad) {
			t.Fatalf("非法名漏判: %q", bad)
		}
	}
}
