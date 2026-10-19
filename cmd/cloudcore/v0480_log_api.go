// v0.48.0 云端日志检索 API（spec 0021 US-3，G11 运维日志查看）。
//
// 5 端点（契约 97→102，只增不改）：
//   - GET    /api/v1/logs                  查询（node/source/level/q/since/until/limit/offset）
//   - GET    /api/v1/logs/nodes            有日志节点清单
//   - GET    /api/v1/logs/nodes/{nodeID}       节点概要（sources/levels 计数）
//   - GET    /api/v1/logs/nodes/{nodeID}/tail  尾部 N 条
//   - DELETE /api/v1/logs/nodes/{nodeID}       清空节点日志（轮转/清理入口）
//
// 全部端点走 apiMux 既有 auth/audit 链（与 v0470 应用面同口径）。
package main

import (
	"net/http"
	"os"
	"strconv"

	"edgeflow/cloud/pkg/cloudhub"
	"edgeflow/cloud/pkg/logstore"
	"edgeflow/pkg/protocol"
)

// logStore 是云端日志存储（main.go 装配；无条件启用内存模）。
var logStore *logstore.Store

func registerLogAPIs(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/logs", apiQueryLogs)
	mux.HandleFunc("GET /api/v1/logs/nodes", apiListLogNodes)
	mux.HandleFunc("GET /api/v1/logs/nodes/{nodeID}", apiGetLogNodeSummary)
	mux.HandleFunc("GET /api/v1/logs/nodes/{nodeID}/tail", apiTailNodeLogs)
	mux.HandleFunc("DELETE /api/v1/logs/nodes/{nodeID}", apiDeleteNodeLogs)
}

// v0480WriteJSON 写 JSON 响应（与 v0470 同形态，独立命名避免耦合）。
func v0480WriteJSON(w http.ResponseWriter, code int, v any) {
	writeJSON(w, code, v)
}

// v0480Err 统一错误体。
func v0480Err(w http.ResponseWriter, code int, msg string) {
	v0480WriteJSON(w, code, map[string]string{"error": msg})
}

// apiQueryLogs GET /api/v1/logs —— 检索（node 缺省跨节点）。
func apiQueryLogs(w http.ResponseWriter, r *http.Request) {
	if logStore == nil {
		v0480Err(w, http.StatusServiceUnavailable, "日志存储未启用")
		return
	}
	q := r.URL.Query()
	opts := logstore.QueryOpts{
		NodeID: q.Get("node"),
		Source: q.Get("source"),
		Level:  q.Get("level"),
		Q:      q.Get("q"),
	}
	if v := q.Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			opts.Since = n
		} else {
			v0480Err(w, http.StatusBadRequest, "since 非法（毫秒时间戳）")
			return
		}
	}
	if v := q.Get("until"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			opts.Until = n
		} else {
			v0480Err(w, http.StatusBadRequest, "until 非法（毫秒时间戳）")
			return
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opts.Limit = n
		} else {
			v0480Err(w, http.StatusBadRequest, "limit 非法")
			return
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opts.Offset = n
		} else {
			v0480Err(w, http.StatusBadRequest, "offset 非法")
			return
		}
	}
	entries, total := logStore.Query(opts)
	// X-Total-Count = 过滤后总数（与 §7.2 既有分页契约口径一致——复核 P2-3）
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	v0480WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// apiListLogNodes GET /api/v1/logs/nodes —— 有日志的节点清单（含条数与最新时间）。
func apiListLogNodes(w http.ResponseWriter, r *http.Request) {
	if logStore == nil {
		v0480Err(w, http.StatusServiceUnavailable, "日志存储未启用")
		return
	}
	type nodeRow struct {
		NodeID   string `json:"nodeId"`
		Count    int    `json:"count"`
		LatestTs int64  `json:"latestTs"`
	}
	var out []nodeRow
	for _, id := range logStore.NodeIDs() {
		sum := logStore.Summary(id)
		if sum == nil {
			continue
		}
		out = append(out, nodeRow{NodeID: id, Count: sum.Count, LatestTs: sum.Latest})
	}
	if out == nil {
		out = []nodeRow{}
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(len(out)))
	v0480WriteJSON(w, http.StatusOK, out)
}

// apiGetLogNodeSummary GET /api/v1/logs/nodes/{nodeID} —— 节点日志概要。
func apiGetLogNodeSummary(w http.ResponseWriter, r *http.Request) {
	if logStore == nil {
		v0480Err(w, http.StatusServiceUnavailable, "日志存储未启用")
		return
	}
	sum := logStore.Summary(r.PathValue("nodeID"))
	if sum == nil {
		v0480Err(w, http.StatusNotFound, "节点无日志")
		return
	}
	v0480WriteJSON(w, http.StatusOK, sum)
}

// apiTailNodeLogs GET /api/v1/logs/nodes/{nodeID}/tail?n= —— 尾部 N 条（默认 100 上限 1000）。
func apiTailNodeLogs(w http.ResponseWriter, r *http.Request) {
	if logStore == nil {
		v0480Err(w, http.StatusServiceUnavailable, "日志存储未启用")
		return
	}
	n := 100
	if v := r.URL.Query().Get("n"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			v0480Err(w, http.StatusBadRequest, "n 非法（正整数）")
			return
		}
		n = parsed
	}
	// 服务端收敛上限（防 n 超大拉全量）——与 logstore.Tail 内部钳制同口径。
	if n > 1000 {
		v0480Err(w, http.StatusBadRequest, "n 超限（最大 1000）")
		return
	}
	entries := logStore.Tail(r.PathValue("nodeID"), n)
	if entries == nil {
		v0480Err(w, http.StatusNotFound, "节点无日志")
		return
	}
	v0480WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// apiDeleteNodeLogs DELETE /api/v1/logs/nodes/{nodeID} —— 清空节点日志。
func apiDeleteNodeLogs(w http.ResponseWriter, r *http.Request) {
	if logStore == nil {
		v0480Err(w, http.StatusServiceUnavailable, "日志存储未启用")
		return
	}
	if !logStore.Delete(r.PathValue("nodeID")) {
		v0480Err(w, http.StatusNotFound, "节点无日志")
		return
	}
	v0480WriteJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// wireLogSyncHandler 把 LogSync 消息接到 logstore（main.go 装配用）。
func wireLogSyncHandler(hub *cloudhub.Server, store *logstore.Store) {
	hub.SetLogSyncHandler(func(nodeID string, b protocol.LogBatch) {
		store.Append(nodeID, b.Entries)
	})
}

// envLogstoreMax 读每节点环形容量 env（默认 5000；合法域 [1, 1e6]——
// 上界钳制防误配超大值首写巨量预分配，复核 P3-3）。
func envLogstoreMax() int {
	v := os.Getenv("EDGEFLOW_CLOUDCORE_LOGSTORE_MAX")
	if v == "" {
		return logstore.DefaultMaxPerNode
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 1_000_000 {
		return logstore.DefaultMaxPerNode
	}
	return n
}
