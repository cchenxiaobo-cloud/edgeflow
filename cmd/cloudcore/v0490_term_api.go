// 远程运维终端 API（v0.49.0，spec 0022，G31）。
//
// 五端点（102→107）：
//   POST   /api/v1/term/sessions            创建会话（write：operator+）
//   GET    /api/v1/term/sessions            会话列表（read：viewer+）
//   POST   /api/v1/term/sessions/{id}/exec  执行白名单命令（write，同步等结果）
//   POST   /api/v1/term/sessions/{id}/close 关闭会话（write，幂等）
//   GET    /api/v1/term/audit               命令级审计查询（read，X-Total-Count）
//
// RBAC 由全局 middleware 统一管控（rbac.Allow：POST=write operator+ /
// GET=read viewer+）；越权在 middleware 层 403，本文件不重复判定。
// 身份取审计上下文（rbac middleware 注入的凭证 ID）。
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"edgeflow/cloud/pkg/audit"
	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/termgw"
	"edgeflow/pkg/log"
	"edgeflow/pkg/protocol"
)

// registerTermAPIs 注册终端 5 端点。
func registerTermAPIs(mux *http.ServeMux, gw *termgw.Gateway) {
	mux.HandleFunc("POST /api/v1/term/sessions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NodeID string `json:"nodeId"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), nil)
			return
		}
		s, err := gw.CreateSession(body.NodeID, body.Reason, audit.IdentityOf(r.Context()))
		if err != nil {
			if errors.Is(err, termgw.ErrSessionLimit) {
				writeErr(w, http.StatusTooManyRequests, err.Error(), nil)
				return
			}
			writeErr(w, http.StatusBadRequest, err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusCreated, s)
	})

	mux.HandleFunc("GET /api/v1/term/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": gw.List(r.URL.Query().Get("nodeID"))})
	})

	mux.HandleFunc("POST /api/v1/term/sessions/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Cmd        string   `json:"cmd"`
			Args       []string `json:"args"`
			TimeoutSec int      `json:"timeoutSec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), nil)
			return
		}
		res, err := gw.Exec(r.PathValue("id"), body.Cmd, body.Args, body.TimeoutSec, audit.IdentityOf(r.Context()))
		if err != nil {
			switch {
			case errors.Is(err, termgw.ErrSessionNotFound):
				writeErr(w, http.StatusNotFound, err.Error(), nil)
			case errors.Is(err, termgw.ErrSessionState):
				writeErr(w, http.StatusConflict, err.Error(), nil)
			case errors.Is(err, termgw.ErrNodeOffline):
				writeErr(w, http.StatusServiceUnavailable, err.Error(), nil)
			default:
				writeErr(w, http.StatusInternalServerError, err.Error(), nil)
			}
			return
		}
		// 业务语义在负载（rejected/exitCode/timeout），HTTP 层 200。
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("POST /api/v1/term/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		if err := gw.Close(r.PathValue("id"), audit.IdentityOf(r.Context())); err != nil {
			if errors.Is(err, termgw.ErrSessionNotFound) {
				writeErr(w, http.StatusNotFound, err.Error(), nil)
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error(), nil)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "closed"})
	})

	mux.HandleFunc("GET /api/v1/term/audit", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		entries, total, err := termgw.ReadAudit(gw.AuditPath(), q.Get("nodeID"), limit, offset)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "审计读取失败: "+err.Error(), nil)
			return
		}
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": total})
	})
}

// wireTermResultHandler 接通 CloudHub TermResult → 网关投递。
func wireTermResultHandler(hub *cloudhub.Server, gw *termgw.Gateway) {
	hub.SetTermResultHandler(func(nodeID string, r *protocol.TermResultPayload) {
		gw.DeliverResult(nodeID, r)
	})
}

// envTermTTL 解析会话空闲 TTL（默认 10m；Go duration 语法，下限 30s）。
func envTermTTL() time.Duration {
	if v := strings.TrimSpace(os.Getenv("EDGEFLOW_CLOUDCORE_TERM_SESSION_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 30*time.Second {
			return d
		}
		log.Warnf("[termgw] EDGEFLOW_CLOUDCORE_TERM_SESSION_TTL=%q 非法（≥30s），回退 10m", v)
	}
	return 10 * time.Minute
}

// envTermInt 解析整数 env（非法回退默认；下限 1）。
func envTermInt(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
		log.Warnf("[termgw] %s=%q 非法，回退 %d", name, v, def)
	}
	return def
}
