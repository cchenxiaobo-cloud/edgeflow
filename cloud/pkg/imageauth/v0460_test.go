package imageauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func osReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

func TestV0460ImageAuthStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "imageauth.json")
	aead, err := NewKey("key-material-1")
	if err != nil || aead == nil {
		t.Fatalf("NewKey: %v", err)
	}
	s, err := Load(path, aead)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Set + 规范化
	if err := s.Set("MyRegistry.io:5000/", "ops", "pass-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// 文件里不出现明文
	data := mustRead(t, path)
	if strings.Contains(string(data), "pass-123") {
		t.Fatal("凭证文件泄露明文密码")
	}
	// 列表脱敏
	views := s.List()
	if len(views) != 1 || views[0].Password != HashPrefix(views[0].Password) || len(views[0].Password) > 12 {
		t.Fatalf("列表脱敏异常: %+v", views)
	}
	if !views[0].Deployable {
		t.Fatal("配置了 key 的条目应 deployable")
	}
	if views[0].Registry != "myregistry.io:5000" {
		t.Fatalf("registry 规范化失败: %q", views[0].Registry)
	}
	// Lookup 解密回原文（含端口镜像引用）
	reg, user, pass, err := s.Lookup("myregistry.io:5000/team/app:v1")
	if err != nil || reg != "myregistry.io:5000" || user != "ops" || pass != "pass-123" {
		t.Fatalf("Lookup = %q %q %q err=%v", reg, user, pass, err)
	}
	// 官方短名（无 registry 段）不匹配
	if s.Match("nginx:1.25") {
		t.Fatal("短名镜像不应匹配凭证")
	}
	// Verify
	if !s.VerifyPassword("myregistry.io:5000", "pass-123") {
		t.Fatal("密码校验应通过")
	}
	if s.VerifyPassword("myregistry.io:5000", "wrong") {
		t.Fatal("错误密码不应通过")
	}
	// 覆盖写 + 重载
	if err := s.Set("myregistry.io:5000", "ops2", "pass-456"); err != nil {
		t.Fatalf("覆盖 Set: %v", err)
	}
	s2, err := Load(path, aead)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if _, _, p, err := s2.Lookup("myregistry.io:5000/x/y:1"); err != nil || p != "pass-456" {
		t.Fatalf("重载后 Lookup: pass=%q err=%v", p, err)
	}
	if len(s2.List()) != 1 {
		t.Fatalf("覆盖写应维持单条: %d", len(s2.List()))
	}
	// 非法 registry
	if err := s.Set("bad_registry", "u", "p"); err == nil {
		t.Fatal("非法 registry 应报错")
	}
	// Delete
	if !s2.Delete("myregistry.io:5000") || s2.Delete("myregistry.io:5000") {
		t.Fatal("Delete 语义异常")
	}
}

func TestV0460ImageAuthNoKey(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(filepath.Join(dir, "ia.json"), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Set("reg.io", "u", "p"); err != nil {
		t.Fatalf("Set（无 key）: %v", err)
	}
	v := s.List()[0]
	if v.Deployable {
		t.Fatal("无 key 条目不应 deployable")
	}
	// Lookup → 明确错误（下发不可用）
	if _, _, _, err := s.Lookup("reg.io/a:1"); err == nil || !strings.Contains(err.Error(), "下发不可用") {
		t.Fatalf("无 key Lookup 应报下发不可用: %v", err)
	}
}

func TestV0460RegistryEdge(t *testing.T) {
	if !ValidRegistry("localhost:5000") || !ValidRegistry("my-reg.io") {
		t.Fatal("合法地址误判")
	}
	if ValidRegistry("-bad.io") || ValidRegistry("") {
		t.Fatal("非法地址漏判")
	}
	if got := NormalizeRegistry("HTTPS://Reg.IO:5000/"); got != "reg.io:5000" {
		t.Fatalf("Normalize = %q", got)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return data
}
