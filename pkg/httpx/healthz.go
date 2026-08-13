// Package httpx 提供 HTTP 相关的公共处理器。
//
// 当前包含 /healthz 健康检查接口，供云边组件探活（如 K8s
// readinessProbe、手动 curl 验证）使用。
package httpx

import (
	"encoding/json"
	"net/http"

	"edgeflow/pkg/version"
)

// HealthzResponse 是 /healthz 接口的响应体。
type HealthzResponse struct {
	// Status 固定为 "ok"，表示服务健康。
	Status string `json:"status"`
	// Version 附带当前程序版本信息，方便排查部署的是哪个版本。
	Version version.Info `json:"version"`
}

// Healthz 返回处理 /healthz 请求的 http.HandlerFunc。
//
// 响应示例：
//
//	{"status":"ok","version":{"version":"v0.1.0","gitCommit":"abc123","buildTime":"...","goVersion":"go1.26.2"}}
func Healthz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		resp := HealthzResponse{
			Status:  "ok",
			Version: version.Get(),
		}
		// 编码失败（如客户端已断开）时无需额外处理，忽略即可
		_ = json.NewEncoder(w).Encode(resp)
	}
}
