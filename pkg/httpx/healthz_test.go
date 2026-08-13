package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHealthz 验证 /healthz 返回 200、JSON Content-Type 和合法响应体。
func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	Healthz()(rec, req)

	// 状态码应为 200
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 %d", rec.Code, http.StatusOK)
	}

	// Content-Type 应为 application/json
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, 期望以 application/json 开头", ct)
	}

	// 响应体应为合法 JSON 且 status 为 ok、version 非空
	var resp HealthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v, 原文: %s", err, rec.Body.String())
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, 期望 %q", resp.Status, "ok")
	}
	if resp.Version.Version == "" {
		t.Error("version.version 字段不应为空")
	}
	if resp.Version.GoVersion == "" {
		t.Error("version.goVersion 字段不应为空")
	}
}

// TestHealthzPost 验证非 GET 方法也能正常响应（handler 不区分方法，简单可靠）。
func TestHealthzPost(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec := httptest.NewRecorder()

	Healthz()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("响应体 = %q, 应包含 status ok", rec.Body.String())
	}
}
