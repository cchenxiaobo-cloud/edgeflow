// 镜像探活单测（v0.9.0，R-1）：mirror 解析 + registry v2 HEAD 探活 +
// Docker Hub token 换取流程（httptest 模拟）。
package modelrelease

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestParseMirror 验证 mirror 三段解析（registry/repo/tag）。
func TestParseMirror(t *testing.T) {
	cases := []struct {
		in       string
		registry string
		repo     string
		tag      string
		wantErr  bool
	}{
		{"mnist", "", "mnist", "latest", false},
		{"mnist:v1.0.0", "", "mnist", "v1.0.0", false},
		{"reg.example.com/mnist:v1.0.0", "reg.example.com", "mnist", "v1.0.0", false},
		{"localhost:5000/mnist:v1", "localhost:5000", "mnist", "v1", false},
		{"10.0.0.1:5000/team/model:1.2", "10.0.0.1:5000", "team/model", "1.2", false},
		{"", "", "", "", true},
		{"/repo", "", "", "", true},
	}
	for _, c := range cases {
		got, err := parseMirror(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q 应报错", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 解析失败: %v", c.in, err)
			continue
		}
		if got.Registry != c.registry || got.Repo != c.repo || got.Tag != c.tag {
			t.Errorf("%q → %+v，期望 registry=%q repo=%q tag=%q", c.in, got, c.registry, c.repo, c.tag)
		}
	}
}

// TestCheckMirrorPrivate 验证私有 registry：HEAD manifests 200=存在；
// 404=不存在；401 且配 token 时携带 Authorization 重试。
func TestCheckMirrorPrivate(t *testing.T) {
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/team/model/manifests/v1" {
			if r.Header.Get("Authorization") != "" {
				authHeader = r.Header.Get("Authorization")
			}
			w.Header().Set("Docker-Content-Digest", "sha256:1111111111111111111111111111111111111111111111111111111111111111")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v2/team/missing/manifests/latest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:1111111111111111111111111111111111111111111111111111111111111111")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 存在（200 + Docker-Content-Digest 头 → 返回 digest）
	opts := MirrorCheckOptions{Timeout: 3 * time.Second, HTTPClient: srv.Client()}
	if _, err := CheckMirror(context.Background(), srv.URL+"/team/model:v1", opts); err != nil {
		t.Errorf("存在应返回 nil: %v", err)
	}
	// 不存在
	if _, err := CheckMirror(context.Background(), srv.URL+"/team/missing:latest", opts); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("不存在应报 404: %v", err)
	}
	// 带 token（私有 registry 探活应携带 Authorization）
	opts.Token = "sekret"
	if d, err := CheckMirror(context.Background(), srv.URL+"/team/model:v1", opts); err != nil {
		t.Errorf("带 token 探活失败: %v", err)
	} else if d == "" {
		t.Error("探活成功但 digest 为空（应返回 Docker-Content-Digest）")
	}
	if authHeader != "Bearer sekret" {
		t.Errorf("Authorization 头 = %q，期望 Bearer sekret", authHeader)
	}
}

// TestCheckMirrorDockerHubFlow 验证 Docker Hub token 流程（合并
// v2 API 与 token 端点在同一个 httptest 服务器上，避免不同 server
// 间 client 互联问题）。
func TestCheckMirrorDockerHubFlow(t *testing.T) {
	var sawToken bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			// 返回 realm 指向本 server 的 /token 端点（用 r.Host 构造）
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/token",service="s"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/token":
			fmt.Fprintf(w, `{"token":"hub-token-123"}`)
		case r.URL.Path == "/v2/mnist/manifests/v1":
			if r.Header.Get("Authorization") == "Bearer hub-token-123" {
				sawToken = true
			}
			w.Header().Set("Docker-Content-Digest", "sha256:2222222222222222222222222222222222222222222222222222222222222222")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	// 走完整 CheckMirror（HubEndpoint 注入本地端点，不连真实 Docker Hub）
	d, err := CheckMirror(context.Background(), "mnist:v1", MirrorCheckOptions{
		Timeout:     3 * time.Second,
		HubEndpoint: srv.URL,
		HTTPClient:  srv.Client(),
	})
	if err != nil {
		t.Fatalf("CheckMirror 失败: %v", err)
	}
	if d == "" {
		t.Error("Docker Hub 流程 digest 为空")
	}
	if !sawToken {
		t.Error("sawToken=false：HEAD 应携带换取到的 Bearer token")
	}
}

// TestCheckMirrorTimeout 验证超时生效（慢 registry → 错误而非挂起）。
func TestCheckMirrorTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	opts := MirrorCheckOptions{Timeout: 50 * time.Millisecond, HTTPClient: srv.Client()}
	start := time.Now()
	_, err := CheckMirror(context.Background(), srv.URL+"/repo:v1", opts)
	if err == nil {
		t.Error("超时应返回错误")
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Errorf("超时未生效，耗时 %v", time.Since(start))
	}
}

// TestParseMirrorCheckMode 验证模式解析。
func TestParseMirrorCheckMode(t *testing.T) {
	cases := []struct {
		in   string
		want MirrorCheckMode
		err  bool
	}{
		{"", MirrorCheckOff, false},
		{"off", MirrorCheckOff, false},
		{"warn", MirrorCheckWarn, false},
		{"fail", MirrorCheckFail, false},
		{"WARN", MirrorCheckWarn, false},
		{"bogus", MirrorCheckOff, true},
	}
	for _, c := range cases {
		got, err := ParseMirrorCheckMode(c.in)
		if c.err {
			if err == nil {
				t.Errorf("%q 应报错", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q → %v err=%v，期望 %v", c.in, got, err, c.want)
		}
	}
}


// TestCheckMirrorReturnsDigest 验证 200 + Docker-Content-Digest 头 → 返回该值。
func TestCheckMirrorReturnsDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "  sha256:abc123  ")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d, err := CheckMirror(context.Background(), srv.URL+"/team/model:v1", MirrorCheckOptions{
		Timeout: 3 * time.Second, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if d != "sha256:abc123" {
		t.Errorf("digest = %q，期望 sha256:abc123（应 TrimSpace）", d)
	}
}

// TestCheckMirrorMissingDigestHeader 验证 200 但缺 Docker-Content-Digest
// 头 → ("", nil)（v0.9.0 语义保持，digest 校验静默降级）。
func TestCheckMirrorMissingDigestHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d, err := CheckMirror(context.Background(), srv.URL+"/team/model:v1", MirrorCheckOptions{
		Timeout: 3 * time.Second, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if d != "" {
		t.Errorf("缺头应返回空 digest，实际 %q", d)
	}
}
