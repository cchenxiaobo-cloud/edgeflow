package modelrelease

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// v0.23.0（CLD-11 输入卫生回归）：
//  1. 探活入口预检拒绝注入面（短形式控制字符/URL 结构字符；带 '/' 形态走
//     modelrepo.ValidateMirror 同源校验）；
//  2. manifestURL 路径段经 url.PathEscape 编码后拼接（请求路径精确、不逃逸）；
//  3. Docker Hub token 换取 query 经 url.Values 编码——WWW-Authenticate 的
//     service 注入 "&scope=..." 不再覆盖真实 scope 参数。
func TestCheckMirrorPrecheckRejectsInjection(t *testing.T) {
	cases := []struct {
		name   string
		mirror string
	}{
		{"short form with query injection", "mnist:v1?x=1"},
		{"short form with fragment", "mnist:v1#frag"},
		{"short form with CRLF", "mnist:v1\r\nX-Injected: 1"},
		{"short form with control char", "mnist:\x01v1"},
		{"empty", "   "},
		{"path form uppercase rejected by ValidateMirror", "registry.example.com/Team/Model:v1"},
		{"path form no tag rejected by ValidateMirror", "registry.example.com/team/model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := CheckMirror(context.Background(), c.mirror, MirrorCheckOptions{Timeout: time.Second})
			if err == nil {
				t.Fatalf("mirror %q should be rejected by precheck", c.mirror)
			}
			if !strings.Contains(err.Error(), "预检失败") {
				t.Errorf("err = %v, want precheck failure", err)
			}
		})
	}
}

// 短形式合法值（Docker Hub 官方库语义）仍放行——钉住主线裁决：
// 预检放宽不破坏既有 TestCheckMirrorDockerHubFlow 的 mnist:v1 语义。
func TestCheckMirrorPrecheckAllowsShortForm(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/token",service="s"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/token":
			fmt.Fprintf(w, `{"token":"t"}`)
		case r.URL.Path == "/v2/mnist/manifests/v1":
			hits.Add(1)
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	if _, err := CheckMirror(context.Background(), "mnist:v1", MirrorCheckOptions{
		Timeout: 2 * time.Second, HubEndpoint: srv.URL, HTTPClient: srv.Client(),
	}); err != nil {
		t.Fatalf("short form mnist:v1 should pass precheck and probe: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("manifest HEAD hits = %d, want 1", hits.Load())
	}
}

// manifestURL 路径精确性：repo/tag 拼接后请求路径与预期逐字节一致
// （PathEscape 纵深防御下合法输入路径不变形）。
func TestCheckMirrorManifestPathExact(t *testing.T) {
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/team/model/manifests/v1.2.0" {
			gotPath.Store(r.URL.Path)
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("b", 64))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := CheckMirror(context.Background(), srv.URL+"/team/model:v1.2.0", MirrorCheckOptions{
		Timeout: 2 * time.Second, HTTPClient: srv.Client(),
	}); err != nil {
		t.Fatalf("CheckMirror error: %v", err)
	}
	if p, _ := gotPath.Load().(string); p != "/v2/team/model/manifests/v1.2.0" {
		t.Errorf("manifest path = %q, want exact /v2/team/model/manifests/v1.2.0", p)
	}
}

// token 换取 query 编码：service 值中的 "&scope=..." 注入被 url.Values
// 编码中和，服务器端解析出的 scope 仍是真实请求的 repository:<repo>:pull。
func TestDockerHubTokenQueryInjectedServiceNeutralized(t *testing.T) {
	var gotService, gotScope atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="http://%s/token",service="s&scope=repository:evil:pull"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/token":
			gotService.Store(r.URL.Query().Get("service"))
			gotScope.Store(r.URL.Query().Get("scope"))
			fmt.Fprintf(w, `{"token":"t"}`)
		case r.URL.Path == "/v2/mnist/manifests/v1":
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("c", 64))
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	if _, err := CheckMirror(context.Background(), "mnist:v1", MirrorCheckOptions{
		Timeout: 2 * time.Second, HubEndpoint: srv.URL, HTTPClient: srv.Client(),
	}); err != nil {
		t.Fatalf("CheckMirror error: %v", err)
	}
	if s, _ := gotService.Load().(string); s != "s&scope=repository:evil:pull" {
		t.Errorf("service param = %q, want the full injected string kept as a single value (encoded, not split)", s)
	}
	if sc, _ := gotScope.Load().(string); sc != "repository:mnist:pull" {
		t.Errorf("scope param = %q, want repository:mnist:pull (injection must not override real scope)", sc)
	}
}
