package edged

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"edgeflow/edge/pkg/metamanager"
)

func TestV0460ImageAuthPasswordEnv(t *testing.T) {
	t.Setenv(EnvImageAuth, `{"priv.registry.io":"s3cret","broken.io":""}`)
	// 重置 once 缓存（测试注入后强制重解析）
	imageAuthOnce = sync.Once{}
	pw, ok := imageAuthPassword("priv.registry.io")
	if !ok || pw != "s3cret" {
		t.Fatalf("imageAuthPassword = %q %v", pw, ok)
	}
	if _, ok := imageAuthPassword("broken.io"); ok {
		t.Fatal("空密码条目应视为未配置")
	}
	if _, ok := imageAuthPassword("unknown.io"); ok {
		t.Fatal("未配置 registry 应返回 false")
	}
}

func TestV0460ImageAuthPasswordBadJSON(t *testing.T) {
	t.Setenv(EnvImageAuth, `{not-json`)
	imageAuthOnce = sync.Once{}
	if _, ok := imageAuthPassword("x.io"); ok {
		t.Fatal("坏 JSON 应返回 false")
	}
}

func TestV0460PodImageAuthDecode(t *testing.T) {
	// podsync 消息带 imageAuth（authRef 形态）→ Pod JSON 解码保留
	raw := `{"name":"edgeflow-model-m1","namespace":"edgeflow","image":"priv.registry.io/team/m1:v1","replicas":1,"imageAuth":{"registry":"priv.registry.io","username":"ops"}}`
	var p metamanager.Pod
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.ImageAuth.Registry != "priv.registry.io" || p.ImageAuth.Username != "ops" {
		t.Fatalf("imageAuth = %+v", p.ImageAuth)
	}
	// 旧消息（无 imageAuth）→ 零值，兼容
	var old metamanager.Pod
	if err := json.Unmarshal([]byte(`{"name":"p","namespace":"edgeflow","image":"nginx","replicas":1}`), &old); err != nil {
		t.Fatalf("decode old: %v", err)
	}
	if old.ImageAuth != nil {
		t.Fatal("旧消息 imageAuth 应为 nil")
	}
	// 序列化回环：空 imageAuth 不输出（omitempty）
	b, _ := json.Marshal(metamanager.Pod{Name: "p", Namespace: "default", Image: "nginx", Replicas: 1})
	if strings.Contains(string(b), "imageAuth") {
		t.Fatalf("空 imageAuth 不应序列化: %s", b)
	}
}

func TestV0460LoginMissingUsername(t *testing.T) {
	d := &DockerRuntime{}
	if err := d.login("reg.io", "", "pw"); err == nil || !strings.Contains(err.Error(), "缺 username") {
		t.Fatalf("login 空 username 应报错: %v", err)
	}
}
